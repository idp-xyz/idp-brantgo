package application

import (
	"context"
	"errors"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TRD's object control commands, as the keys TSK derives for them name them (R17).
const (
	trdBind        = "object_control.bind"
	trdStop        = "object_control.stop"
	trdEnterManual = "object_control.enter_manual"
	trdHandoff     = "object_control.handoff"
	trdRelease     = "object_control.release"
)

// step runs a command on an existing task: it locks the task row FOR UPDATE, checks that the actor
// sees it and the version the caller saw (task_version CAS), lets apply change it through the
// domain and writes it with its history row.
type step struct {
	s        Service
	id       string
	expected int64
	apply    func(ctx context.Context, exec bentopg.Executor, t *task.Task, act task.Act) (task.Change, error)
	t        *task.Task
}

func (h *step) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.t, err = postgres.LoadTask(ctx, exec, scope(ctx), h.id, postgres.ForUpdate)
	return err
}

func (h *step) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	t := h.t
	if t == nil || !t.Visible(d.Actor, d.Sees) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/task_id", "")
	}
	if err := t.Expect(h.expected); err != nil {
		return action.Outcome{}, err
	}
	from := t.Version
	ch, err := h.apply(ctx, exec, t, task.Act{By: d.Actor, Epoch: d.Epoch, At: h.s.Clock.Now()})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveTask(ctx, exec, scope(ctx), t, from, ch); err != nil {
		return action.Outcome{}, conflict(err)
	}
	return action.Outcome{ObjectID: t.ID, RowVersion: t.Version, Status: string(t.State), Events: events(t, ch)}, nil
}

// controlStep is a step that asks TRD before TSK's transaction (the ledger's Preparer step): it
// reads the task, lets prepare run the domain's checks — so TRD is not asked for a command TSK
// refuses — and call TRD, and then records what TRD answered in the step.
type controlStep struct {
	step
	prepare func(ctx context.Context, t *task.Task, act task.Act, unsettled int) error
}

func (h *controlStep) Prepare(ctx context.Context) error {
	var t *task.Task
	var act task.Act
	var unsettled int
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		var err error
		if t, err = postgres.LoadTask(ctx, exec, scope(ctx), h.id, postgres.NoLock); err != nil {
			return err
		}
		if t == nil || !t.Visible(d.Actor, d.Sees) {
			return kernel.Reject(kernel.CodeNotFound, "/args/task_id", "")
		}
		if err = t.Expect(h.expected); err != nil {
			return err
		}
		act = task.Act{By: d.Actor, Epoch: d.Epoch, At: h.s.Clock.Now()}
		unsettled, err = h.s.Actions.Unsettled(ctx, exec, scope(ctx), t.ID)
		return err
	})
	if err != nil {
		return err
	}
	return h.prepare(ctx, t, act, unsettled)
}

func conflict(err error) error {
	if errors.Is(err, postgres.ErrConflict) {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return err
}

// events are the facts a change publishes (Context Map TSK.facts; payloads carry identifiers and
// versions only, contract C4): TaskCreated, TaskControlChanged for every control change,
// TaskCompleted and TaskClosedUnfulfilled when the task reaches those states.
func events(t *task.Task, ch task.Change) []action.Event {
	ev := func(typ string, fields map[string]any) action.Event {
		fields["task_id"], fields["task_version"] = t.ID, t.Version
		return action.Event{Type: typ, AggregateType: "BusinessTask", AggregateID: t.ID, AggregateVersion: t.Version, Fields: fields}
	}
	var out []action.Event
	switch {
	case ch.Command == task.CmdCreate:
		out = append(out, ev(api.EventTaskCreated, map[string]any{"goal_code": t.Goal, "goal_version": t.GoalVersion, "lifecycle": string(t.State),
			"control_mode": string(t.Mode), "owner_id": t.Owner, "rfq_id": t.Target.RfqID, "quote_id": t.Target.QuoteID}))
	case ch.ControlChange != "":
		out = append(out, ev(api.EventTaskControlChanged, map[string]any{"control_change": ch.ControlChange, "lifecycle": string(t.State),
			"control_mode": string(t.Mode), "control_epoch": t.ControlEpoch, "quote_id": t.Target.QuoteID}))
	}
	switch {
	case ch.To == task.Completed && ch.From != task.Completed:
		out = append(out, ev(api.EventTaskCompleted, map[string]any{"goal_code": t.Goal, "goal_version": t.GoalVersion,
			"completion_actor": t.CompletionActor, "quote_id": t.Target.QuoteID}))
	case ch.To == task.ClosedUnfulfilled && ch.From != task.ClosedUnfulfilled:
		out = append(out, ev(api.EventTaskClosedUnfulfilled, map[string]any{"goal_code": t.Goal, "goal_version": t.GoalVersion,
			"close_cause": t.Termination.Cause, "quote_id": t.Target.QuoteID}))
	}
	return out
}

// perform runs h for cmd and answers with the task's control as stored after it.
func (s Service) perform(ctx context.Context, name string, req api.Command, objectID, key, reason string, validation error, h action.Handler) (api.TaskReceipt, error) {
	res, err := s.run(ctx, action.Command{Name: name, IdempotencyKey: key, ObjectID: objectID, Reason: reason}, req, validation, h)
	if err != nil {
		return api.TaskReceipt{}, err
	}
	return s.receipt(ctx, res)
}

// createCommand runs task.create: it locks the task its ID names, if any, and else creates it. A
// task ID taken by the same request of the same initiator answers from the stored task (Replayed);
// by another request, BUSINESS_KEY_CONFLICT. An open task serving the same goal for the same source
// refuses the new one (来源防重, BUSINESS_KEY_CONFLICT). The owner (a human; the initiator when
// none is named) and the agent of an AI task must hold active grants of their kinds.
type createCommand struct {
	s        Service
	cmd      api.CreateTask
	hash     string
	existing *task.Task
	replay   bool
}

func (h *createCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.replay = false
	h.existing, err = postgres.LoadTask(ctx, exec, scope(ctx), h.cmd.TaskID, postgres.ForUpdate)
	return err
}

func (h *createCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	c := h.cmd
	if t := h.existing; t != nil {
		if !t.Visible(d.Actor, d.Sees) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/task_id", "")
		}
		if t.RequestHash != h.hash || t.Initiator != d.Actor {
			return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/task_id", "The task ID holds another request")
		}
		h.replay = true
		return action.Outcome{ObjectID: t.ID, RowVersion: t.Version, Status: string(t.State)}, nil
	}
	sc := scope(ctx)
	r := task.CreateRequest{ID: c.TaskID, Goal: c.GoalCode, Mode: task.Mode(c.ControlMode), Agent: c.AgentID, Owner: c.OwnerID,
		Target: task.Target{RfqID: c.Target.RfqID, QuoteID: c.Target.QuoteID, CustomerID: c.Target.CustomerID}, RequestHash: h.hash}
	r.GoalVersion, _ = api.GoalVersion(c.GoalCode)
	if c.Source != nil {
		r.Source = &task.Source{Kind: c.Source.Kind, ID: c.Source.ID}
		open, err := postgres.OpenTaskFor(ctx, exec, sc, c.GoalCode, *r.Source)
		if err != nil {
			return action.Outcome{}, err
		}
		if open != "" {
			return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/source", "An open task serves this goal for the source")
		}
	}
	if c.Deadline != "" {
		r.Deadline, _ = kernel.ParseTimestamp(c.Deadline)
	}
	if b := c.Budget; b != nil {
		r.Budget = &task.Budget{MaxModelCalls: b.MaxModelCalls, MaxRetries: b.MaxRetries}
		if b.MaxCost != nil {
			m, err := b.MaxCost.Cost()
			if err != nil {
				return action.Outcome{}, err
			}
			r.Budget.MaxCost = &m
		}
	}
	if e := c.ExecutionBundle; e != nil {
		r.Bundle = &task.Bundle{ID: e.BundleID, SHA256: e.SHA256}
	}
	if r.Owner == "" && d.Actor.Kind == kernel.ActorHuman {
		r.Owner = d.Actor.ID
	}
	if r.Owner != "" {
		team, err := qualify(ctx, exec, r.Owner, kernel.ActorHuman, "/args/owner_id")
		if err != nil {
			return action.Outcome{}, err
		}
		r.OwnerTeam = team
	}
	if r.Agent != "" {
		if _, err := qualify(ctx, exec, r.Agent, kernel.ActorAI, "/args/agent_id"); err != nil {
			return action.Outcome{}, err
		}
	}
	t, ch, err := task.Create(r, task.Act{By: d.Actor, Epoch: d.Epoch, At: h.s.Clock.Now()})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertTask(ctx, exec, sc, t, ch); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: t.ID, RowVersion: t.Version, Status: string(t.State), Events: events(t, ch)}, nil
}

// CreateTask creates a task (task.create).
func (s Service) CreateTask(ctx context.Context, cmd api.CreateTask) (api.TaskReceipt, error) {
	h := &createCommand{s: s, cmd: cmd, hash: requestHash(cmd)}
	res, err := s.run(ctx, action.Command{Name: api.CommandCreate, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.TaskID}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.TaskReceipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return s.receipt(ctx, res)
}

// ScopeTask moves a task through scoping (task.scope).
func (s Service) ScopeTask(ctx context.Context, cmd api.ScopeTask) (api.TaskReceipt, error) {
	h := &step{s: s, id: cmd.TaskID, expected: cmd.ExpectedVersion,
		apply: func(_ context.Context, _ bentopg.Executor, t *task.Task, act task.Act) (task.Change, error) {
			return t.Scope(cmd.Note, act)
		}}
	return s.perform(ctx, api.CommandScope, cmd, cmd.TaskID, cmd.IdempotencyKey, "", cmd.Validate(), h)
}

func ref(r *api.ObjectRef) *task.Ref {
	if r == nil {
		return nil
	}
	return &task.Ref{Kind: r.Kind, ID: r.ID, Version: r.Version}
}

// WaitTask makes a task wait (task.wait); the responsible person holds an active human grant (checked
// after the state machine, so an unlisted transition is INVALID_STATE whoever is named).
func (s Service) WaitTask(ctx context.Context, cmd api.WaitTask) (api.TaskReceipt, error) {
	h := &step{s: s, id: cmd.TaskID, expected: cmd.ExpectedVersion,
		apply: func(ctx context.Context, exec bentopg.Executor, t *task.Task, act task.Act) (task.Change, error) {
			r := task.WaitRequest{Wait: cmd.Wait, Reason: cmd.Reason, Responsible: cmd.ResponsibleID, Items: slices.Clone(cmd.Items),
				Awaiting: ref(cmd.Awaiting)}
			if cmd.DueAt != "" {
				r.DueAt, _ = kernel.ParseTimestamp(cmd.DueAt)
			}
			ch, err := t.Wait(r, act)
			if err != nil {
				return task.Change{}, err
			}
			_, err = qualify(ctx, exec, cmd.ResponsibleID, kernel.ActorHuman, "/args/responsible_id")
			return ch, err
		}}
	return s.perform(ctx, api.CommandWait, cmd, cmd.TaskID, cmd.IdempotencyKey, cmd.Reason, cmd.Validate(), h)
}

// VerifyTask moves a running task to verifying (task.verify).
func (s Service) VerifyTask(ctx context.Context, cmd api.VerifyTask) (api.TaskReceipt, error) {
	h := &step{s: s, id: cmd.TaskID, expected: cmd.ExpectedVersion,
		apply: func(ctx context.Context, exec bentopg.Executor, t *task.Task, act task.Act) (task.Change, error) {
			n, err := s.Actions.Unsettled(ctx, exec, scope(ctx), t.ID)
			if err != nil {
				return task.Change{}, err
			}
			return t.Verify(n, act)
		}}
	return s.perform(ctx, api.CommandVerify, cmd, cmd.TaskID, cmd.IdempotencyKey, "", cmd.Validate(), h)
}

// VerifyGoal verifies a verifying task's goal (goal.verify): TRD's GoalReadiness v1 assesses it
// before TSK's transaction; the transaction records the outcome (TSK-INV-001).
func (s Service) VerifyGoal(ctx context.Context, cmd api.VerifyGoal) (api.TaskReceipt, error) {
	h := &controlStep{step: step{s: s, id: cmd.TaskID, expected: cmd.ExpectedVersion}}
	var verdict Verdict
	h.prepare = func(ctx context.Context, t *task.Task, act task.Act, _ int) error {
		if err := t.CheckVerifyGoal(act); err != nil {
			return err
		}
		var err error
		verdict, err = s.Upstream.Readiness.Assess(ctx, Goal{TaskID: t.ID, Code: t.Goal, Version: t.GoalVersion, Target: t.Target})
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, t *task.Task, act task.Act) (task.Change, error) {
		n, err := s.Actions.Unsettled(ctx, exec, scope(ctx), t.ID)
		if err != nil {
			return task.Change{}, err
		}
		return t.VerifyGoal(task.Verdict{Outcome: verdict.Outcome, Unmet: slices.Clone(verdict.Unmet)}, n, act)
	}
	return s.perform(ctx, api.CommandVerifyGoal, cmd, cmd.TaskID, cmd.IdempotencyKey, "", cmd.Validate(), h)
}

// PauseTask pauses a task (task.pause); a responsible person named holds an active human grant.
func (s Service) PauseTask(ctx context.Context, cmd api.PauseTask) (api.TaskReceipt, error) {
	h := &step{s: s, id: cmd.TaskID, expected: cmd.ExpectedVersion,
		apply: func(ctx context.Context, exec bentopg.Executor, t *task.Task, act task.Act) (task.Change, error) {
			ch, err := t.Pause(task.PauseRequest{Cause: cmd.Cause, Reason: cmd.Reason, Responsible: cmd.ResponsibleID,
				ResumeCondition: cmd.ResumeCondition}, act)
			if err != nil || cmd.ResponsibleID == "" {
				return ch, err
			}
			_, err = qualify(ctx, exec, cmd.ResponsibleID, kernel.ActorHuman, "/args/responsible_id")
			return ch, err
		}}
	return s.perform(ctx, api.CommandPause, cmd, cmd.TaskID, cmd.IdempotencyKey, cmd.Reason, cmd.Validate(), h)
}

// ResumeTask resumes a waiting or paused task (task.resume).
func (s Service) ResumeTask(ctx context.Context, cmd api.ResumeTask) (api.TaskReceipt, error) {
	h := &step{s: s, id: cmd.TaskID, expected: cmd.ExpectedVersion,
		apply: func(ctx context.Context, exec bentopg.Executor, t *task.Task, act task.Act) (task.Change, error) {
			n, err := s.Actions.Unsettled(ctx, exec, scope(ctx), t.ID)
			if err != nil {
				return task.Change{}, err
			}
			return t.Resume(cmd.Reason, ref(cmd.Arrival), n, act)
		}}
	return s.perform(ctx, api.CommandResume, cmd, cmd.TaskID, cmd.IdempotencyKey, cmd.Reason, cmd.Validate(), h)
}

// stopTask runs task.take_control, task.handoff or task.cancel: a bound root is stopped at TRD first
// (object_control.stop with the task's claim; TSK-INV-002), then the task records TRD's epoch.
func (s Service) stopTask(ctx context.Context, name string, req api.Command, id string, expected int64, key, reason string, validation error) (api.TaskReceipt, error) {
	command := task.Command(name)
	h := &controlStep{step: step{s: s, id: id, expected: expected}}
	var epoch int64
	h.prepare = func(ctx context.Context, t *task.Task, act task.Act, _ int) error {
		epoch = 0
		if err := t.CheckStop(command, act); err != nil || !t.Bound() {
			return err
		}
		var err error
		epoch, err = s.Upstream.Control.Stop(ctx, ControlRequest{Key: DeriveKey(t.ID, trdStop, t.ControlEpoch), TaskID: t.ID,
			QuoteID: t.BoundQuote(), Epoch: t.ControlEpoch, Reason: ControlReason(t.ID, t.ControlEpoch)})
		return err
	}
	h.apply = func(_ context.Context, _ bentopg.Executor, t *task.Task, act task.Act) (task.Change, error) {
		switch command {
		case task.CmdTakeControl:
			return t.TakeControl(reason, epoch, act)
		case task.CmdHandoff:
			return t.Handoff(reason, epoch, act)
		}
		return t.Cancel(reason, epoch, act)
	}
	return s.perform(ctx, name, req, id, key, reason, validation, h)
}

// TakeControl takes a task over for people (task.take_control).
func (s Service) TakeControl(ctx context.Context, cmd api.TakeControl) (api.TaskReceipt, error) {
	return s.stopTask(ctx, api.CommandTakeControl, cmd, cmd.TaskID, cmd.ExpectedVersion, cmd.IdempotencyKey, cmd.Reason, cmd.Validate())
}

// Handoff hands a manual task back to its AI agent (task.handoff).
func (s Service) Handoff(ctx context.Context, cmd api.Handoff) (api.TaskReceipt, error) {
	return s.stopTask(ctx, api.CommandHandoff, cmd, cmd.TaskID, cmd.ExpectedVersion, cmd.IdempotencyKey, cmd.Reason, cmd.Validate())
}

// CancelTask requests a task's cancellation (task.cancel).
func (s Service) CancelTask(ctx context.Context, cmd api.CancelTask) (api.TaskReceipt, error) {
	return s.stopTask(ctx, api.CommandCancel, cmd, cmd.TaskID, cmd.ExpectedVersion, cmd.IdempotencyKey, cmd.Reason, cmd.Validate())
}

// FinishStop completes a stopping task's barrier (task.finish_stop): once no action is submitted or
// unknown, a bound root of a take-over enters manual control at TRD (object_control.enter_manual),
// one of a hand-back is bound again (object_control.handoff); then the task records TRD's epoch. A
// cancellation does not call TRD.
func (s Service) FinishStop(ctx context.Context, cmd api.FinishStop) (api.TaskReceipt, error) {
	h := &controlStep{step: step{s: s, id: cmd.TaskID, expected: cmd.ExpectedVersion}}
	var epoch int64
	h.prepare = func(ctx context.Context, t *task.Task, act task.Act, unsettled int) error {
		epoch = 0
		stop, err := t.CheckFinish(unsettled, act)
		if err != nil || !t.Bound() || stop.Purpose == task.StopCancel {
			return err
		}
		r := ControlRequest{TaskID: t.ID, QuoteID: t.BoundQuote(), Epoch: t.ControlEpoch, Reason: ControlReason(t.ID, t.ControlEpoch),
			Initiator: t.Owner, Agent: t.Agent}
		if stop.Purpose == task.StopTakeControl {
			r.Key = DeriveKey(t.ID, trdEnterManual, t.ControlEpoch)
			epoch, err = s.Upstream.Control.EnterManual(ctx, r)
		} else {
			r.Key = DeriveKey(t.ID, trdHandoff, t.ControlEpoch)
			epoch, err = s.Upstream.Control.Handoff(ctx, r)
		}
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, t *task.Task, act task.Act) (task.Change, error) {
		n, err := s.Actions.Unsettled(ctx, exec, scope(ctx), t.ID)
		if err != nil {
			return task.Change{}, err
		}
		return t.FinishStop(n, epoch, act)
	}
	return s.perform(ctx, api.CommandFinishStop, cmd, cmd.TaskID, cmd.IdempotencyKey, "", cmd.Validate(), h)
}

// CloseUnfulfilled closes a task without its goal (task.close_unfulfilled).
func (s Service) CloseUnfulfilled(ctx context.Context, cmd api.CloseUnfulfilled) (api.TaskReceipt, error) {
	h := &step{s: s, id: cmd.TaskID, expected: cmd.ExpectedVersion,
		apply: func(ctx context.Context, exec bentopg.Executor, t *task.Task, act task.Act) (task.Change, error) {
			n, err := s.Actions.Unsettled(ctx, exec, scope(ctx), t.ID)
			if err != nil {
				return task.Change{}, err
			}
			return t.CloseUnfulfilled(cmd.Cause, cmd.Reason, n, act)
		}}
	return s.perform(ctx, api.CommandCloseUnfulfilled, cmd, cmd.TaskID, cmd.IdempotencyKey, cmd.Reason, cmd.Validate(), h)
}

// FailTask ends a task as failed (task.fail); the person responsible for what follows holds an
// active human grant.
func (s Service) FailTask(ctx context.Context, cmd api.FailTask) (api.TaskReceipt, error) {
	h := &step{s: s, id: cmd.TaskID, expected: cmd.ExpectedVersion,
		apply: func(ctx context.Context, exec bentopg.Executor, t *task.Task, act task.Act) (task.Change, error) {
			n, err := s.Actions.Unsettled(ctx, exec, scope(ctx), t.ID)
			if err != nil {
				return task.Change{}, err
			}
			ch, err := t.Fail(cmd.Reason, cmd.ResponsibleID, n, act)
			if err != nil {
				return task.Change{}, err
			}
			_, err = qualify(ctx, exec, cmd.ResponsibleID, kernel.ActorHuman, "/args/responsible_id")
			return ch, err
		}}
	return s.perform(ctx, api.CommandFail, cmd, cmd.TaskID, cmd.IdempotencyKey, cmd.Reason, cmd.Validate(), h)
}

// BindObject binds the quote root the task's action created (task.bind_object): TRD's
// object_control.bind first, then the task records the epoch TRD returned.
func (s Service) BindObject(ctx context.Context, cmd api.BindObject) (api.TaskReceipt, error) {
	h := &controlStep{step: step{s: s, id: cmd.TaskID, expected: cmd.ExpectedVersion}}
	var epoch int64
	h.prepare = func(ctx context.Context, t *task.Task, act task.Act, _ int) error {
		if err := t.CheckBind(cmd.QuoteID, act); err != nil {
			return err
		}
		var err error
		epoch, err = s.Upstream.Control.Bind(ctx, ControlRequest{Key: DeriveKey(t.ID, trdBind, t.ControlEpoch), TaskID: t.ID,
			QuoteID: cmd.QuoteID, Epoch: t.ControlEpoch, Initiator: t.Owner, Agent: t.Agent})
		return err
	}
	h.apply = func(_ context.Context, _ bentopg.Executor, t *task.Task, act task.Act) (task.Change, error) {
		return t.BindObject(cmd.QuoteID, epoch, act)
	}
	return s.perform(ctx, api.CommandBindObject, cmd, cmd.TaskID, cmd.IdempotencyKey, "", cmd.Validate(), h)
}

// ReleaseObject gives a task's root back (task.release): TRD's object_control.release with the
// task's claim first, then the task records TRD's epoch; the lifecycle does not change.
func (s Service) ReleaseObject(ctx context.Context, cmd api.ReleaseObject) (api.TaskReceipt, error) {
	h := &controlStep{step: step{s: s, id: cmd.TaskID, expected: cmd.ExpectedVersion}}
	var epoch int64
	h.prepare = func(ctx context.Context, t *task.Task, act task.Act, unsettled int) error {
		if err := t.CheckRelease(unsettled, act); err != nil {
			return err
		}
		var err error
		epoch, err = s.Upstream.Control.Release(ctx, ControlRequest{Key: DeriveKey(t.ID, trdRelease, t.ControlEpoch), TaskID: t.ID,
			QuoteID: t.BoundQuote(), Epoch: t.ControlEpoch, Reason: ControlReason(t.ID, t.ControlEpoch)})
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, t *task.Task, act task.Act) (task.Change, error) {
		n, err := s.Actions.Unsettled(ctx, exec, scope(ctx), t.ID)
		if err != nil {
			return task.Change{}, err
		}
		return t.Release(cmd.Reason, epoch, n, act)
	}
	return s.perform(ctx, api.CommandRelease, cmd, cmd.TaskID, cmd.IdempotencyKey, cmd.Reason, cmd.Validate(), h)
}
