package application

import (
	"context"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// goal.verify (WBS S4-01d; SM-TSK-TASK verifying → completed | waiting_input | waiting_external;
// TSK-INV-001; Context Map R18 GoalReadiness v1). Before TSK's transaction (the ledger's Preparer
// step) the command reads the task, checks it may be verified — so TRD is not asked for a command
// TSK refuses — and asks TRD's GoalReadiness v1 through the Readiness port for the task's goal on
// its quote root, with TRD's control of a bound root. A task that names no quote root is judged by
// TSK without TRD; a root TRD does not show the verifying actor (NOT_FOUND, PERMISSION_DENIED) is
// Q01.scope unsatisfied; any other refusal refuses goal.verify. Inside the transaction the task row
// is locked FOR UPDATE and compared with the version read (task_version CAS: the verification judges
// the task as it was read), the unsettled actions are counted (TSK-INV-003), the domain records the
// verification and moves the task, and the verification is written after the task's history row.
// TSK holds no lock of TRD; TRD's answer is the state of its records at its checked_at.

// verifyGoal is goal.verify's handler: controlStep's Prepare and Lock, and an Apply that writes the
// verification after the task.
type verifyGoal struct {
	controlStep
	assessment   task.Assessment
	verification task.Verification
}

func (h *verifyGoal) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	out, err := h.controlStep.Apply(ctx, exec, d)
	if err != nil {
		return out, err
	}
	v := h.verification
	if err = postgres.InsertVerification(ctx, exec, scope(ctx), v); err != nil {
		return action.Outcome{}, err
	}
	// TaskCompleted names the records the goal was judged on (identifiers and versions, contract C4).
	for _, e := range out.Events {
		if e.Type == api.EventTaskCompleted {
			e.Fields["business_revision"], e.Fields["order_id"], e.Fields["order_revision"] = v.BusinessRevision, v.OrderID, v.OrderRevision
		}
	}
	return out, nil
}

// assess asks TRD for the goal of t (before TSK's transaction), or judges it without TRD when t
// names no quote root or TRD does not show the root to the verifying actor.
func (s Service) assess(ctx context.Context, t *task.Task, at time.Time) (task.Assessment, error) {
	if t.Target.QuoteID == "" {
		return t.LocalAssessment("", at), nil
	}
	a, err := s.Upstream.Readiness.Assess(ctx, Goal{TaskID: t.ID, Code: t.Goal, Version: t.GoalVersion, Target: t.Target, Bound: t.BoundQuote()})
	if rej, ok := kernel.AsRejection(err); ok && (rej.Code == kernel.CodeNotFound || rej.Code == kernel.CodePermissionDenied) {
		return t.LocalAssessment(rej.Code, at), nil
	}
	return a, err
}

// VerifyGoal verifies a verifying task's goal (goal.verify): TRD's GoalReadiness v1 judges it before
// TSK's transaction; the transaction records the verification and moves the task (TSK-INV-001).
func (s Service) VerifyGoal(ctx context.Context, cmd api.VerifyGoal) (api.TaskReceipt, error) {
	h := &verifyGoal{controlStep: controlStep{step: step{s: s, id: cmd.TaskID, expected: cmd.ExpectedVersion}}}
	h.prepare = func(ctx context.Context, t *task.Task, act task.Act, _ int) error {
		if err := t.CheckVerifyGoal(act); err != nil {
			return err
		}
		var err error
		h.assessment, err = s.assess(ctx, t, act.At)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, t *task.Task, act task.Act) (task.Change, error) {
		n, err := s.Actions.Unsettled(ctx, exec, scope(ctx), t.ID)
		if err != nil {
			return task.Change{}, err
		}
		var ch task.Change
		ch, h.verification, err = t.RecordVerification(h.assessment, n, act)
		return ch, err
	}
	return s.perform(ctx, api.CommandVerifyGoal, cmd, cmd.TaskID, cmd.IdempotencyKey, "", cmd.Validate(), h)
}

// GoalVerifications returns the goal verifications of a task the caller sees, oldest first
// (task.verifications, tsk.read).
func (s Service) GoalVerifications(ctx context.Context, taskID string) ([]api.GoalVerification, error) {
	var out []api.GoalVerification
	if !kernel.ValidIdentifier(taskID) {
		return out, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	err := s.Ledger.Read(ctx, api.CommandVerifications, taskID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		t, err := postgres.LoadTask(ctx, exec, scope(ctx), taskID, postgres.NoLock)
		if err != nil {
			return err
		}
		if t == nil || !t.Visible(d.Actor, d.Sees) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		rows, err := postgres.LoadVerifications(ctx, exec, scope(ctx), taskID)
		if err != nil {
			return err
		}
		out = make([]api.GoalVerification, len(rows))
		for i, r := range rows {
			out[i] = verificationView(r)
		}
		return nil
	})
	return out, err
}

func verificationView(r postgres.RecordedVerification) api.GoalVerification {
	v := api.GoalVerification{TaskID: r.TaskID, TaskVersion: r.TaskVersion, Lifecycle: string(r.To), GoalCode: r.Goal, GoalVersion: r.GoalVersion,
		Assessor: r.Assessor, Outcome: r.Outcome, Code: r.Code, Unmet: slices.Clone(r.Unmet), QuoteID: r.QuoteID, RootVersion: r.RootVersion,
		BusinessRevision: r.BusinessRevision, BusinessHash: r.BusinessHash, OrderID: r.OrderID, OrderRevision: r.OrderRevision,
		OrderHash: r.OrderHash, UnsettledActions: r.Unsettled, CheckedAt: r.CheckedAt, RecordedAt: r.RecordedAt, Actor: r.By,
		Assertions: make([]api.GoalAssertion, len(r.Assertions))}
	if c := r.Control; c != nil {
		v.Control = &api.RootControlView{State: c.State, TaskID: c.TaskID, ControlEpoch: c.Epoch}
	}
	for i, a := range r.Assertions {
		x := api.GoalAssertion{Assertion: a.Assertion, Answer: a.Answer, Code: a.Code, Checks: make([]api.GoalCheck, len(a.Checks)),
			Evidence: make([]api.GoalEvidence, len(a.Evidence))}
		for j, c := range a.Checks {
			x.Checks[j] = api.GoalCheck(c)
		}
		for j, e := range a.Evidence {
			x.Evidence[j] = api.GoalEvidence(e)
		}
		v.Assertions[i] = x
	}
	return v
}
