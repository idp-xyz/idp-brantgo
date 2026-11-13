package application

import (
	"context"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// GetTask returns a task the caller sees with its history (task.get, tsk.read).
func (s Service) GetTask(ctx context.Context, taskID string) (api.TaskView, error) {
	var out api.TaskView
	if !kernel.ValidIdentifier(taskID) {
		return out, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	err := s.Ledger.Read(ctx, api.CommandGetTask, taskID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		t, err := postgres.LoadTask(ctx, exec, scope(ctx), taskID, postgres.NoLock)
		if err != nil {
			return err
		}
		if t == nil || !t.Visible(d.Actor, d.Sees) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		history, err := postgres.LoadHistory(ctx, exec, scope(ctx), taskID)
		if err != nil {
			return err
		}
		out = view(t, history)
		return nil
	})
	return out, err
}

// Claim issues the claim of a task step to the caller (task.claim, tsk.read; D-2, TSK-INV-004). It
// reads the task as committed: a take-over that commits later moves the epoch at TRD first, so a
// step that carries this claim after it is refused there (TRD is the fence).
func (s Service) Claim(ctx context.Context, taskID string) (api.Claim, error) {
	var out api.Claim
	if !kernel.ValidIdentifier(taskID) {
		return out, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	err := s.Ledger.Read(ctx, api.CommandClaim, taskID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		t, err := postgres.LoadTask(ctx, exec, scope(ctx), taskID, postgres.NoLock)
		if err != nil {
			return err
		}
		if t == nil || !t.Visible(d.Actor, d.Sees) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		if err = t.Claim(d.Actor); err != nil {
			return err
		}
		out = api.Claim{TaskID: t.ID, TaskVersion: t.Version, Lifecycle: string(t.State), ControlMode: string(t.Mode), Actor: d.Actor,
			IssuedAt: kernel.Instant(s.Clock.Now())}
		if t.Bound() {
			out.ControlEpoch, out.QuoteID = t.ControlEpoch, t.BoundQuote()
		}
		return nil
	})
	return out, err
}

func view(t *task.Task, history []task.Change) api.TaskView {
	v := api.TaskView{TaskID: t.ID, GoalCode: t.Goal, GoalVersion: t.GoalVersion, Lifecycle: string(t.State), ControlMode: string(t.Mode),
		ControlEpoch: t.ControlEpoch, TaskVersion: t.Version, Initiator: t.Initiator, OwnerID: t.Owner, AgentID: t.Agent,
		Target: api.Target{RfqID: t.Target.RfqID, QuoteID: t.Target.QuoteID, CustomerID: t.Target.CustomerID}, Deadline: t.Deadline,
		PausedFrom: string(t.PausedFrom), ScopeSettledAt: t.ScopeSettledAt, CompletionActor: t.CompletionActor, CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt}
	if s := t.Source; s != nil {
		v.Source = &api.SourceRef{Kind: s.Kind, ID: s.ID}
	}
	if b := t.Budget; b != nil {
		v.Budget = &api.Budget{MaxModelCalls: b.MaxModelCalls, MaxRetries: b.MaxRetries}
		if m := b.MaxCost; m != nil {
			v.Budget.MaxCost = &api.CostCap{Currency: m.Currency().Code(), MinorUnits: m.Currency().MinorUnits(), Amount: m.Amount()}
		}
	}
	if b := t.Bundle; b != nil {
		v.ExecutionBundle = &api.BundleRef{BundleID: b.ID, SHA256: b.SHA256}
	}
	if b := t.Binding; b != nil {
		v.Binding = &api.BindingView{QuoteID: b.QuoteID, State: b.State, ControlEpoch: b.Epoch, BoundAt: b.BoundAt, ReleasedAt: b.ReleasedAt}
	}
	if c := t.WaitCond; c != nil {
		v.Wait = &api.WaitView{Wait: c.Kind, Reason: c.Reason, ResponsibleID: c.Responsible, Items: slices.Clone(c.Items), DueAt: c.DueAt,
			Since: c.Since}
		if a := c.Awaiting; a != nil {
			v.Wait.Awaiting = &api.ObjectRef{Kind: a.Kind, ID: a.ID, Version: a.Version}
		}
	}
	if p := t.PauseCond; p != nil {
		v.Pause = &api.PauseView{Cause: p.Cause, Reason: p.Reason, ResponsibleID: p.Responsible, ResumeCondition: p.ResumeCondition, Since: p.Since}
	}
	if s := t.Stop; s != nil {
		v.Stop = &api.StopView{Purpose: s.Purpose, From: string(s.From), Reason: s.Reason, RequestedBy: s.By, RequestedAt: s.At}
	}
	if e := t.Termination; e != nil {
		v.Termination = &api.TerminationView{Cause: e.Cause, Reason: e.Reason, ResponsibleID: e.Responsible, By: e.By, At: e.At}
	}
	for _, h := range history {
		v.History = append(v.History, api.TransitionView{Seq: h.Version, Command: string(h.Command), From: string(h.From), To: string(h.To),
			ControlMode: string(h.Mode), ControlEpoch: h.ControlEpoch, ControlChange: h.ControlChange, TaskVersion: h.Version, Actor: h.By,
			AuthorityEpoch: h.Epoch, At: h.At, Reason: h.Reason})
	}
	return v
}
