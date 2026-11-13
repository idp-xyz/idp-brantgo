package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The object control commands (SM-TRD-CONTROL, TSK-INV-004; WBS S3-08, S4-01b; Python t03_storage
// bind, stop and unbind; Q01 V0.1 store.py take_control and finish_control). Each locks the quote
// root FOR UPDATE — the same row every quote write locks first — so a stop that commits first
// refuses a write waiting behind it, and a write that commits first is kept (BC-AT20). They compare
// and increment the control epoch, never the root version: a binding does not change the quote's
// content. Their receipts carry the control epoch as RowVersion and the control state as Status.
//
// Every change but the bind needs the bound task's claim (S4 decision D-4), which only TSK's
// trusted adapter puts into the context; the bind and the handoff read the grants of the task's
// initiator and agent through the authority guard rows FOR SHARE (Authorizer.Qualify), after the
// root and the caller's own guard rows, as the lock order requires (decision D-5). A replay of a
// control change is not fenced (Visible): TSK retries a step with its key after a crash between its
// two steps (SM-TSK-TASK task.take_control), when the epoch has already moved.

// BindControl binds a quote root to a task (object_control.bind; human or system).
func (s Service) BindControl(ctx context.Context, cmd api.BindControl) (api.Receipt, error) {
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if !h.visible(d) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/quote_id", "")
		}
		b := quote.Binding{Initiator: cmd.InitiatorID, Agent: cmd.AgentID}
		if err := h.q.Bind(cmd.TaskID, b, cmd.ExpectedControlEpoch, d.Actor, d.Epoch, grantsOf(ctx, exec), s.Clock.Now()); err != nil {
			return action.Outcome{}, err
		}
		return h.control(ctx, exec)
	}
	return s.run(ctx, action.Command{Name: api.CommandBindControl, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID},
		cmd, cmd.Validate(), h)
}

// StopControl fences the bound task out (object_control.stop; human only, with the task's claim).
func (s Service) StopControl(ctx context.Context, cmd api.StopControl) (api.Receipt, error) {
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if !h.visible(d) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/quote_id", "")
		}
		if err := h.q.Stop(claim(ctx), cmd.ExpectedControlEpoch, d.Actor, s.Clock.Now()); err != nil {
			return action.Outcome{}, err
		}
		return h.control(ctx, exec)
	}
	return s.run(ctx, action.Command{Name: api.CommandStopControl, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
}

// EnterManualControl puts a stopping root under manual control (object_control.enter_manual; human
// or system, with the task's claim).
func (s Service) EnterManualControl(ctx context.Context, cmd api.EnterManualControl) (api.Receipt, error) {
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if !h.visible(d) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/quote_id", "")
		}
		if err := h.q.EnterManual(claim(ctx), cmd.ExpectedControlEpoch, d.Actor, s.Clock.Now()); err != nil {
			return action.Outcome{}, err
		}
		return h.control(ctx, exec)
	}
	return s.run(ctx, action.Command{Name: api.CommandEnterManualControl, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
}

// HandoffControl binds a stopping root to its task again (object_control.handoff; human or system,
// with the task's claim).
func (s Service) HandoffControl(ctx context.Context, cmd api.HandoffControl) (api.Receipt, error) {
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if !h.visible(d) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/quote_id", "")
		}
		b := quote.Binding{Initiator: cmd.InitiatorID, Agent: cmd.AgentID}
		if err := h.q.Handoff(claim(ctx), b, cmd.ExpectedControlEpoch, d.Actor, d.Epoch, grantsOf(ctx, exec), s.Clock.Now()); err != nil {
			return action.Outcome{}, err
		}
		return h.control(ctx, exec)
	}
	return s.run(ctx, action.Command{Name: api.CommandHandoffControl, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
}

// ReleaseControl gives the root back (object_control.release; human only, with the task's claim).
func (s Service) ReleaseControl(ctx context.Context, cmd api.ReleaseControl) (api.Receipt, error) {
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if !h.visible(d) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/quote_id", "")
		}
		if err := h.q.Release(claim(ctx), cmd.ExpectedControlEpoch, d.Actor, s.Clock.Now()); err != nil {
			return action.Outcome{}, err
		}
		return h.control(ctx, exec)
	}
	return s.run(ctx, action.Command{Name: api.CommandReleaseControl, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
}

// control writes the changed object control (the root row only) and answers with the control
// epoch and state.
func (h *quoteCommand) control(ctx context.Context, exec bentopg.Executor) (action.Outcome, error) {
	if err := postgres.SaveQuote(ctx, exec, scope(ctx), h.q, false, h.from, h.fromEpoch, h.s.Clock.Now()); err != nil {
		return action.Outcome{}, err
	}
	c := h.q.Control
	return action.Outcome{ObjectID: h.q.ID, RowVersion: c.Epoch, Status: string(c.State)}, nil
}

// claim returns the control claim the trusted adapter put into ctx, or nil.
func claim(ctx context.Context) *quote.Claim {
	c, ok := api.ControlFromContext(ctx)
	if !ok {
		return nil
	}
	return &quote.Claim{TaskID: c.TaskID, Epoch: c.ControlEpoch}
}

// grantsOf reads actors' grants now in the transaction of exec, through the authority guard rows FOR
// SHARE (Authorizer.Qualify, which reads with the caller's executor and decides nothing): a change
// of the grant either committed before and is seen, or waits until TRD's transaction ends.
func grantsOf(ctx context.Context, exec bentopg.Executor) quote.Grants {
	return func(actorID string) (quote.Grant, error) {
		h, err := authorization.Authorizer{}.Qualify(ctx, exec, actorID)
		if err != nil {
			return quote.Grant{}, err
		}
		return quote.Grant{Kind: h.Actor.Kind, Active: h.Active, ChangedEpoch: h.ChangedEpoch}, nil
	}
}

// fence is the write of d's actor in ctx as the root's object control meets it (TRD-INV-015,
// decisions D-3 and D-5): the claim in ctx, the actor's kind, and the grants read through exec.
func fence(ctx context.Context, exec bentopg.Executor, d authorization.Decision) quote.Write {
	return quote.Write{Claim: claim(ctx), Actor: d.Actor.Kind, Grants: grantsOf(ctx, exec)}
}
