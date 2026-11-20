package application

import (
	"context"
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/adapters/postgres"
	actions "go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/action"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ActionExecution (TSK-AGG-ActionExecution, TSK-INV-003; S4 unit B1). TSK asks the target context's
// ActionReconciliation v1 (Context Map R18 TRD, R66 EVD, R67 INQ, R68 SUP, R69 CST) before its own
// transaction, as the caller, on every action.record_attempt and action.reconcile, and records the
// answer in the transaction: the target's ledger is the truth of what became of an action's key and
// hash. None of these relationships is a guard edge: TSK holds no lock of a target.
//
// Lock order of an action command: the action row FOR UPDATE (its own root), its task row FOR SHARE —
// so the task's control commands, which lock the task FOR UPDATE and count its open actions,
// serialize with every recorded attempt —, then the company's authority guard FOR SHARE (the
// ledger).

// Answer is what a target context's ActionReconciliation v1 answered for a key and a request hash:
// committed with the target's stored receipt, payload_conflict or not_committed.
type Answer struct {
	State   kernel.ReconcileState
	Receipt *api.TargetReceipt // committed only
}

// Reconciler is TSK's port to one target context's ActionReconciliation v1: ReconcileHash
// reconciles by (command, idempotency key, request hash) as the principal of ctx, executing
// nothing. A refusal is the target's *kernel.Rejection, passed on.
type Reconciler interface {
	ReconcileHash(ctx context.Context, command, idempotencyKey, requestHash string) (Answer, error)
}

// Reconcilers are TSK's ports to the contexts a task step writes, by context code (evd, inq, sup,
// cst, trd).
type Reconcilers map[string]Reconciler

// Complete reports whether every context a task step writes has its port.
func (r Reconcilers) Complete() bool {
	for _, c := range actions.Contexts {
		if r[c] == nil {
			return false
		}
	}
	return true
}

// StoredActions counts a task's open actions in TSK's store (Actions; TSK-INV-003).
type StoredActions struct{}

// Unsettled counts the task's actions whose attempt is submitted or unknown.
func (StoredActions) Unsettled(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, taskID string) (int, error) {
	return postgres.OpenActions(ctx, exec, scope, taskID)
}

// actionRules are the rules of ActionExecution's commands and reads: a task step's executor — a
// human or the task's AI agent (role templates: sales_clerk, merchandiser, purchaser, costing,
// ai_q01_agent) — records its attempts and reconciles them; no system actor executes task steps.
// Reads need tsk.read.
func actionRules() []authorization.Rule {
	executors := []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}
	return []authorization.Rule{
		{Command: api.CommandRecordAttempt, Permission: api.CommandRecordAttempt, ActorKinds: executors},
		{Command: api.CommandReconcileAction, Permission: api.CommandReconcileAction, ActorKinds: executors},
		{Command: api.CommandGetAction, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandTaskActions, Permission: api.PermissionRead, ActorKinds: anyActor},
	}
}

// actionRequest is request for ActionExecution's commands.
func actionRequest(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.RecordAttempt:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandRecordAttempt, key, c
	case api.ReconcileAction:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandReconcileAction, key, c
	}
	return "", "", nil
}

// claimEpoch is the control epoch of the claim a step of t carries: the task's when a root is
// bound, else 0 (Claim).
func claimEpoch(t *task.Task) int64 {
	if t.Bound() {
		return t.ControlEpoch
	}
	return 0
}

// executor checks that d's actor executes steps of t: it sees the task (NOT_FOUND otherwise) and,
// an AI actor, is the task's agent (PERMISSION_DENIED).
func executor(t *task.Task, d authorization.Decision) error {
	if t == nil || !t.Visible(d.Actor, d.Sees) {
		return kernel.Reject(kernel.CodeNotFound, "/args/task_id", "")
	}
	if d.Actor.Kind == kernel.ActorAI && d.Actor.ID != t.Agent {
		return kernel.Reject(kernel.CodePermissionDenied, "/", "The actor is not the task's AI agent")
	}
	return nil
}

// checkAttempt is the task's side of action.record_attempt: the executor of t's steps, and for a
// submission the holder of the task's claim (D-2, TSK-INV-004: not while the task stops or has
// ended; the agent only in control mode ai while scoping or running) at its current control epoch
// (CONTROL_EPOCH_STALE: the claim predates a control change of the task).
func checkAttempt(t *task.Task, d authorization.Decision, cmd api.RecordAttempt) error {
	if err := executor(t, d); err != nil {
		return err
	}
	if cmd.Outcome != api.ActionSubmitted {
		return nil
	}
	if err := t.Claim(d.Actor); err != nil {
		return err
	}
	if cmd.ControlEpoch != claimEpoch(t) {
		return api.Reject(api.CodeControlEpochStale, "/args/control_epoch", "The claim predates a control change of the task")
	}
	return nil
}

// ask asks the target context's ActionReconciliation v1 what became of key and hash, as the caller.
func (s Service) ask(ctx context.Context, target actions.Target, key string) (actions.Answer, error) {
	r := s.Upstream.Reconcilers[target.Context]
	if r == nil {
		return actions.Answer{}, api.Reject(api.CodeDependencyNotSupported, "/args/target/context", "The context's ActionReconciliation v1 is not wired")
	}
	a, err := r.ReconcileHash(ctx, target.Command, key, target.RequestHash)
	if err != nil {
		return actions.Answer{}, targetRejection(err)
	}
	out := actions.Answer{State: a.State}
	if a.Receipt != nil {
		out.Result = &actions.Result{ActionID: a.Receipt.ActionID, ObjectID: a.Receipt.ObjectID, RowVersion: a.Receipt.RowVersion,
			Status: a.Receipt.Status}
	}
	return out, nil
}

// targetRejection puts a target's refusal of the request's form at the members of TSK's request
// that carried it: an unknown command (the target does not have it) at /args/target/command, a
// malformed hash at /args/target/request_hash. Other refusals and errors pass on unchanged.
func targetRejection(err error) error {
	rej, ok := kernel.AsRejection(err)
	if !ok || rej.Code != kernel.CodeSchemaInvalid || len(rej.Fields) == 0 {
		return err
	}
	switch rej.Fields[0].Path {
	case "/args/command", "/command":
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/target/command", "The target context has no such command")
	case "/args/request_hash":
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/target/request_hash", "")
	}
	return err
}

func targetReceipt(r *actions.Result) *api.TargetReceipt {
	if r == nil {
		return nil
	}
	return &api.TargetReceipt{ActionID: r.ActionID, ObjectID: r.ObjectID, RowVersion: r.RowVersion, Status: r.Status}
}

func actionConflict(err error) error {
	if errors.Is(err, postgres.ErrActionConflict) {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/action_id", "")
	}
	return err
}

// save writes e at its new version with step s: a new action (registered by its first record) or
// a change of the one stored at from.
func save(ctx context.Context, exec bentopg.Executor, e *actions.Execution, from int64, s actions.Step) error {
	if from == 0 {
		return postgres.InsertAction(ctx, exec, scope(ctx), e, s)
	}
	return actionConflict(postgres.SaveAction(ctx, exec, scope(ctx), e, from, s))
}

// actionEvents are the facts a step publishes: ActionReconciled when it records the target's answer
// in a reconciliation, or a committed or conflicting answer anywhere (Context Map TSK.facts;
// identifiers and versions only, contract C4).
func actionEvents(e *actions.Execution, s actions.Step) []action.Event {
	if s.Command != actions.CmdReconcile && !s.State.Final() {
		return nil
	}
	fields := map[string]any{"task_id": e.TaskID, "business_intent_id": e.Intent, "attempt": s.Attempt,
		"target_context": e.Target.Context, "target_command": e.Target.Command, "request_hash": e.Target.RequestHash,
		"state": string(e.State), "reconcile_state": string(s.Answer)}
	if r := s.Result; r != nil {
		fields["target_action_id"], fields["target_object_id"], fields["target_row_version"] = r.ActionID, r.ObjectID, r.RowVersion
	}
	return []action.Event{{Type: api.EventActionReconciled, AggregateType: "ActionExecution", AggregateID: e.ID, AggregateVersion: e.Version,
		Fields: fields}}
}

func outcome(e *actions.Execution, s *actions.Step) action.Outcome {
	out := action.Outcome{ObjectID: e.ID, RowVersion: e.Version, Status: string(e.State)}
	if s != nil {
		out.Events = actionEvents(e, *s)
	}
	return out
}

func version(e *actions.Execution) int64 {
	if e == nil {
		return 0
	}
	return e.Version
}

// attemptCommand runs action.record_attempt. Prepare reads the task and the intent's action, makes
// the checks that need no answer — so a target is not asked for a request TSK refuses — and asks
// the target; Lock locks the action and its task; Apply decides again under the locks with the
// answer and records it.
type attemptCommand struct {
	s      Service
	cmd    api.RecordAttempt
	req    actions.Request
	answer actions.Answer
	asked  bool
	t      *task.Task
	e      *actions.Execution
}

func (h *attemptCommand) Prepare(ctx context.Context) error {
	h.asked = false
	var ask bool
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		t, err := postgres.LoadTask(ctx, exec, scope(ctx), h.cmd.TaskID, postgres.NoLock)
		if err != nil {
			return err
		}
		if err = checkAttempt(t, d, h.cmd); err != nil {
			return err
		}
		e, err := postgres.LoadActionOfIntent(ctx, exec, scope(ctx), h.cmd.BusinessIntentID, postgres.NoLock)
		if err != nil {
			return err
		}
		ask, err = actions.Precheck(e, h.req)
		return err
	})
	if err != nil || !ask {
		return err
	}
	h.answer, err = h.s.ask(ctx, h.req.Target, actions.ID(h.req.Intent))
	h.asked = err == nil
	return err
}

func (h *attemptCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	if h.e, err = postgres.LoadActionOfIntent(ctx, exec, scope(ctx), h.cmd.BusinessIntentID, postgres.ForUpdate); err != nil {
		return err
	}
	h.t, err = postgres.LoadTask(ctx, exec, scope(ctx), h.cmd.TaskID, postgres.ForShare)
	return err
}

func (h *attemptCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if err := checkAttempt(h.t, d, h.cmd); err != nil {
		return action.Outcome{}, err
	}
	ask, err := actions.Precheck(h.e, h.req)
	if err != nil {
		return action.Outcome{}, err
	}
	if !ask {
		// Final: the action answers with what it holds; nothing is recorded or sent again (CT11).
		return outcome(h.e, nil), nil
	}
	if !h.asked {
		return action.Outcome{}, errors.New("tsk: the target's answer is missing")
	}
	from := version(h.e)
	e, s, err := actions.Record(h.e, h.req, h.answer, actions.Act{By: d.Actor, Epoch: d.Epoch, ControlEpoch: claimEpoch(h.t), At: h.s.Clock.Now()})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = save(ctx, exec, e, from, *s); err != nil {
		return action.Outcome{}, err
	}
	h.e = e
	return outcome(e, s), nil
}

// RecordAttempt records an attempt of a task step's action (action.record_attempt).
func (s Service) RecordAttempt(ctx context.Context, cmd api.RecordAttempt) (api.ActionReceipt, error) {
	h := &attemptCommand{s: s, cmd: cmd, req: actions.Request{Intent: cmd.BusinessIntentID, TaskID: cmd.TaskID,
		Target:  actions.Target{Context: cmd.Target.Context, Command: cmd.Target.Command, RequestHash: cmd.Target.RequestHash},
		Outcome: actions.State(cmd.Outcome), Code: cmd.Code, DelegationID: cmd.DelegationID, GrantID: cmd.GrantID}}
	res, err := s.run(ctx, action.Command{Name: api.CommandRecordAttempt, IdempotencyKey: cmd.IdempotencyKey,
		ObjectID: actions.ID(cmd.BusinessIntentID)}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.ActionReceipt{}, err
	}
	return s.actionReceipt(ctx, res)
}

// reconcileCommand runs action.reconcile: Prepare reads the task and the action, refuses a final
// action and asks the target; Lock and Apply as attemptCommand's.
type reconcileCommand struct {
	s      Service
	cmd    api.ReconcileAction
	answer actions.Answer
	asked  bool
	t      *task.Task
	e      *actions.Execution
}

// find checks that d's actor executes steps of t and that e is an action of t (NOT_FOUND at
// /args/action_id otherwise).
func (h *reconcileCommand) find(t *task.Task, e *actions.Execution, d authorization.Decision) error {
	if err := executor(t, d); err != nil {
		return err
	}
	if e == nil || e.TaskID != h.cmd.TaskID {
		return kernel.Reject(kernel.CodeNotFound, "/args/action_id", "")
	}
	return nil
}

func (h *reconcileCommand) Prepare(ctx context.Context) error {
	h.asked = false
	var e *actions.Execution
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		t, err := postgres.LoadTask(ctx, exec, scope(ctx), h.cmd.TaskID, postgres.NoLock)
		if err != nil {
			return err
		}
		if e, err = postgres.LoadAction(ctx, exec, scope(ctx), h.cmd.ActionID, postgres.NoLock); err != nil {
			return err
		}
		if err = h.find(t, e, d); err != nil {
			return err
		}
		return e.CheckReconcile()
	})
	if err != nil {
		return err
	}
	h.answer, err = h.s.ask(ctx, e.Target, e.Key())
	h.asked = err == nil
	return err
}

func (h *reconcileCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	if h.e, err = postgres.LoadAction(ctx, exec, scope(ctx), h.cmd.ActionID, postgres.ForUpdate); err != nil {
		return err
	}
	h.t, err = postgres.LoadTask(ctx, exec, scope(ctx), h.cmd.TaskID, postgres.ForShare)
	return err
}

func (h *reconcileCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if err := h.find(h.t, h.e, d); err != nil {
		return action.Outcome{}, err
	}
	if !h.asked {
		return action.Outcome{}, errors.New("tsk: the target's answer is missing")
	}
	e := h.e
	from := e.Version
	s, err := e.Reconcile(h.answer, actions.Act{By: d.Actor, Epoch: d.Epoch, ControlEpoch: claimEpoch(h.t), At: h.s.Clock.Now()})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = save(ctx, exec, e, from, s); err != nil {
		return action.Outcome{}, err
	}
	return outcome(e, &s), nil
}

// ReconcileAction records what the target's ledger holds for an action (action.reconcile).
func (s Service) ReconcileAction(ctx context.Context, cmd api.ReconcileAction) (api.ActionReceipt, error) {
	h := &reconcileCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandReconcileAction, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ActionID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.ActionReceipt{}, err
	}
	return s.actionReceipt(ctx, res)
}

// actionReceipt completes res with the action as it is stored now and the attempt of the version
// res names.
func (s Service) actionReceipt(ctx context.Context, res api.Receipt) (api.ActionReceipt, error) {
	out := api.ActionReceipt{Receipt: res, IdempotencyKey: res.ObjectID}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		e, err := postgres.LoadAction(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || e == nil {
			return err
		}
		out.State, out.Result = string(e.State), targetReceipt(e.Result)
		steps, err := postgres.LoadSteps(ctx, exec, scope(ctx), e.ID)
		for _, st := range steps {
			if st.Version == res.RowVersion {
				out.Attempt = st.Attempt
			}
		}
		return err
	})
	return out, err
}

// GetAction returns an action the caller sees, with its log (action.get, tsk.read).
func (s Service) GetAction(ctx context.Context, actionID string) (api.ActionView, error) {
	var out api.ActionView
	if !kernel.ValidIdentifier(actionID) {
		return out, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	err := s.Ledger.Read(ctx, api.CommandGetAction, actionID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		e, err := postgres.LoadAction(ctx, exec, scope(ctx), actionID, postgres.NoLock)
		if err != nil {
			return err
		}
		if e == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		t, err := postgres.LoadTask(ctx, exec, scope(ctx), e.TaskID, postgres.NoLock)
		if err != nil {
			return err
		}
		if t == nil || !t.Visible(d.Actor, d.Sees) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		out, err = actionView(ctx, exec, e)
		return err
	})
	return out, err
}

// TaskActions returns the actions of a task the caller sees, oldest first (action.list, tsk.read).
func (s Service) TaskActions(ctx context.Context, taskID string) ([]api.ActionView, error) {
	var out []api.ActionView
	if !kernel.ValidIdentifier(taskID) {
		return nil, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	err := s.Ledger.Read(ctx, api.CommandTaskActions, taskID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		t, err := postgres.LoadTask(ctx, exec, scope(ctx), taskID, postgres.NoLock)
		if err != nil {
			return err
		}
		if t == nil || !t.Visible(d.Actor, d.Sees) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		all, err := postgres.TaskActions(ctx, exec, scope(ctx), taskID)
		if err != nil {
			return err
		}
		for _, e := range all {
			v, err := actionView(ctx, exec, e)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

func actionView(ctx context.Context, exec bentopg.Executor, e *actions.Execution) (api.ActionView, error) {
	steps, err := postgres.LoadSteps(ctx, exec, scope(ctx), e.ID)
	if err != nil {
		return api.ActionView{}, err
	}
	v := api.ActionView{ActionID: e.ID, BusinessIntentID: e.Intent, TaskID: e.TaskID,
		Target: api.TargetCommand{Context: e.Target.Context, Command: e.Target.Command, RequestHash: e.Target.RequestHash}, IdempotencyKey: e.Key(),
		State: string(e.State), Version: e.Version, Attempts: e.Attempts, Result: targetReceipt(e.Result), RegisteredBy: e.RegisteredBy,
		CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
	for _, s := range steps {
		v.Log = append(v.Log, api.AttemptView{Version: s.Version, Attempt: s.Attempt, Command: s.Command, State: string(s.State),
			Answer: string(s.Answer), Code: s.Code, ControlEpoch: s.ControlEpoch, DelegationID: s.DelegationID, GrantID: s.GrantID,
			Result: targetReceipt(s.Result), Actor: s.By, AuthorityEpoch: s.Epoch, At: s.At})
	}
	return v, nil
}
