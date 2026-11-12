package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The object control commands (WBS S3-08, SM-TRD-CONTROL; Python t03_storage bind, stop and
// unbind). Each locks the quote root FOR UPDATE — the same row every quote write locks first — so a
// stop that commits first refuses a write waiting behind it, and a write that commits first is kept
// (BC-AT20). They compare and increment the control epoch, never the root version: a binding does
// not change the quote's content. Their receipts carry the control epoch as RowVersion and the
// control state as Status.

// BindControl binds a quote root to a task (object_control.bind; human or system).
func (s Service) BindControl(ctx context.Context, cmd api.BindControl) (api.Receipt, error) {
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if !h.visible(d) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/quote_id", "")
		}
		if err := h.q.Bind(cmd.TaskID, cmd.ExpectedControlEpoch, d.Actor, d.Epoch, s.Clock.Now()); err != nil {
			return action.Outcome{}, err
		}
		return h.control(ctx, exec)
	}
	return s.run(ctx, action.Command{Name: api.CommandBindControl, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID},
		cmd, cmd.Validate(), h)
}

// StopControl fences the bound task out (object_control.stop; human only).
func (s Service) StopControl(ctx context.Context, cmd api.StopControl) (api.Receipt, error) {
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if !h.visible(d) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/quote_id", "")
		}
		if err := h.q.Stop(cmd.ExpectedControlEpoch, d.Actor, s.Clock.Now()); err != nil {
			return action.Outcome{}, err
		}
		return h.control(ctx, exec)
	}
	return s.run(ctx, action.Command{Name: api.CommandStopControl, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
}

// ReleaseControl gives the root back (object_control.release; human only).
func (s Service) ReleaseControl(ctx context.Context, cmd api.ReleaseControl) (api.Receipt, error) {
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if !h.visible(d) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/quote_id", "")
		}
		if err := h.q.Release(cmd.ExpectedControlEpoch, d.Actor, s.Clock.Now()); err != nil {
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
