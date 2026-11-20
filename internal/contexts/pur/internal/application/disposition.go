package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Dispositions of delivered goods (WBS S5R-04; S5R-D05; domain/commitment disposition.go;
// api/disposition.go; SM-PUR-COMMITMENT-DISPOSITION, PUR-INV-013).
//
// Lock order (tactical lockOrder, PUR-INV-013 "领域守卫＋承诺根 FOR UPDATE"): the commitment root FOR UPDATE;
// for a decision then the disposition row FOR UPDATE, which only ever happens under its commitment's
// lock; the commitment's verified deliveries and dispositions are read without a lock — every delivery
// verification and every disposition command locks this commitment root first, so they are as the last
// one committed; then the company's authority guard FOR SHARE (the ledger's Authorize); then the rows,
// the ledger's result, audit and Outbox. No upstream guard is called: a disposition only takes from what
// DownstreamCommitments v1 shows TRD (S5 decision D-1: TRD reading it as still open refuses
// conservatively), and the EVD sources a request cites are read before the transaction (R76). The
// commitment's row is never written: its status and version stay.

var dispositionCmds = []string{api.CommandDisposeCommitment, api.CommandDecideDisposition}

// dispositionRequestOf is the domain's request for cmd, whose structure Validate checked.
func dispositionRequestOf(cmd api.DisposeCommitment) commitment.DispositionRequest {
	q, _ := kernel.ParseDecimal(cmd.Quantity, kernel.QuantityFormat)
	return commitment.DispositionRequest{ID: cmd.DispositionID, CommitmentID: cmd.CommitmentID, OrderLineID: cmd.OrderLineID, Kind: cmd.Disposition,
		Quantity: q, Reason: cmd.Reason, Evidence: evidenceOf(cmd.Evidence)}
}

func dispositionOutcome(x *commitment.Disposition, events ...action.Event) action.Outcome {
	return action.Outcome{ObjectID: x.ID, RowVersion: x.RowVersion, Status: x.Status, Events: events}
}

// lineUnit returns the unit draft dr buys order line id in — the order line's (PUR-INV-012) — and the
// order line IDs it buys.
func lineUnit(dr *draft.Draft, id string) (commitment.Unit, []string, error) {
	doc, err := dr.Decode()
	if err != nil {
		return commitment.Unit{}, nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/commitment_id", "The stored draft is not intact")
	}
	var unit commitment.Unit
	lines := make([]string, 0, len(doc.Lines))
	for _, l := range doc.Lines {
		lines = append(lines, l.OrderLineID)
		if l.OrderLineID == id {
			unit = commitment.Unit{ID: l.UnitRef.ID, Revision: l.UnitRef.Revision}
		}
	}
	return unit, lines, nil
}

// disposeCommand runs purchase.commitment.dispose.
type disposeCommand struct {
	s        Service
	cmd      api.DisposeCommitment
	req      commitment.DispositionRequest
	readable map[string]bool
	l        locked
	existing *commitment.Disposition
	replay   bool
}

// Prepare reads which of the cited EVD sources the requester reads.
func (h *disposeCommand) Prepare(ctx context.Context) error {
	var err error
	h.readable, err = h.s.readable(ctx, h.cmd.Evidence)
	return err
}

func (h *disposeCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.replay, h.existing = false, nil
	if h.l, err = lockCommitment(ctx, exec, h.cmd.CommitmentID, false); err != nil {
		return err
	}
	h.existing, err = postgres.LoadDisposition(ctx, exec, scope(ctx), h.cmd.DispositionID, postgres.NoLock)
	return err
}

// Apply refuses, in this order: a disposition ID taken by another request or requester
// (VERSION_CONFLICT; the same request by the same requester is answered from the stored disposition), a
// commitment the requester does not see (NOT_FOUND), a draft not intact (REFERENCE_INVALID), Dispose's
// refusals (INVALID_STATE for a commitment never issued, REFERENCE_INVALID for a line it does not buy,
// INVALID_KNOWN_VALUE for a quantity beyond what is verified delivered of the line less its other
// dispositions) and evidence the requester does not read (REFERENCE_INVALID).
func (h *disposeCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc := scope(ctx)
	if x := h.existing; x != nil {
		c, err := postgres.LoadCommitment(ctx, exec, sc, x.CommitmentID, postgres.NoLock)
		if err != nil {
			return action.Outcome{}, err
		}
		if c == nil || !d.Sees(c.OwnerActor, c.OwnerTeam) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/disposition_id", "")
		}
		if err = x.Answer(h.req, d.Actor); err != nil {
			return action.Outcome{}, err
		}
		h.replay = true
		return dispositionOutcome(x), nil
	}
	if _, err := commitmentDraft(ctx, d, h.l); err != nil {
		return action.Outcome{}, err
	}
	c, dr := h.l.c, h.l.dr
	unit, lines, err := lineUnit(dr, h.req.OrderLineID)
	if err != nil {
		return action.Outcome{}, err
	}
	verified, err := postgres.VerifiedOfCommitment(ctx, exec, sc, c.ID, "")
	if err != nil {
		return action.Outcome{}, err
	}
	others, err := postgres.DispositionsOfCommitment(ctx, exec, sc, c.ID)
	if err != nil {
		return action.Outcome{}, err
	}
	x, err := commitment.Dispose(h.req, c, lines, unit, verified, others, d.Actor, d.Epoch, h.s.Clock.Now())
	if err != nil {
		return action.Outcome{}, err
	}
	if err = cited(h.cmd.Evidence, h.readable); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertDisposition(ctx, exec, sc, x); err != nil {
		return action.Outcome{}, err
	}
	return dispositionOutcome(x), nil
}

// DisposeCommitment requests the disposition of goods delivered for a commitment line and not shipped
// (purchase.commitment.dispose; S5R-D05, PUR-INV-013). Human only; it publishes no event.
func (s Service) DisposeCommitment(ctx context.Context, cmd api.DisposeCommitment) (api.DispositionReceipt, error) {
	validation := cmd.Validate()
	h := &disposeCommand{s: s, cmd: cmd}
	if validation == nil {
		h.req = dispositionRequestOf(cmd)
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandDisposeCommitment, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.DispositionID,
		Reason: cmd.Reason}, cmd, validation, h)
	if err != nil {
		return api.DispositionReceipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return s.dispositionReceipt(ctx, res)
}

// decideDisposition runs purchase.commitment.disposition.decide.
type decideDisposition struct {
	s   Service
	cmd api.DecideDisposition
	l   locked
	x   *commitment.Disposition
}

// Lock locks the commitment of the disposition FOR UPDATE, then the disposition row.
func (h *decideDisposition) Lock(ctx context.Context, exec bentopg.Executor) error {
	sc := scope(ctx)
	h.l, h.x = locked{}, nil
	cid, err := postgres.DispositionCommitment(ctx, exec, sc, h.cmd.DispositionID)
	if err != nil || cid == "" {
		return err
	}
	if h.l, err = lockCommitment(ctx, exec, cid, false); err != nil || h.l.c == nil {
		return err
	}
	h.x, err = postgres.LoadDisposition(ctx, exec, sc, h.cmd.DispositionID, postgres.ForUpdate)
	return err
}

// Apply refuses, in this order: a disposition the decider does not see (NOT_FOUND), Decidable's refusals
// (VERSION_CONFLICT, INVALID_STATE, INDEPENDENT_REVIEW_REQUIRED); then a rejection is recorded; an
// approval goes on with Approve's refusal of a line whose approved dispositions would exceed what is
// verified delivered of it now (INVALID_KNOWN_VALUE) and publishes PurchaseCommitmentDisposed.
func (h *decideDisposition) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc := scope(ctx)
	c, x := h.l.c, h.x
	if x == nil || c == nil || !d.Sees(c.OwnerActor, c.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/disposition_id", "")
	}
	if err := x.Decidable(h.cmd.ExpectedVersion, d.Actor); err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	act := commitment.Act{By: d.Actor, Epoch: d.Epoch, At: now, Reason: h.cmd.Reason}
	if h.cmd.Decision == api.DecisionReject {
		x.Reject(act)
		if err := postgres.SaveDisposition(ctx, exec, sc, x, now); err != nil {
			return action.Outcome{}, err
		}
		return dispositionOutcome(x), nil
	}
	verified, err := postgres.VerifiedOfCommitment(ctx, exec, sc, c.ID, "")
	if err != nil {
		return action.Outcome{}, err
	}
	others, err := postgres.DispositionsOfCommitment(ctx, exec, sc, c.ID)
	if err != nil {
		return action.Outcome{}, err
	}
	if err = x.Approve(verified[x.OrderLineID], others, act); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveDisposition(ctx, exec, sc, x, now); err != nil {
		return action.Outcome{}, err
	}
	// The payload is the tactical model's (eventPayloads PurchaseCommitmentDisposed); the aggregate is the
	// commitment, whose version a disposition does not move.
	event := action.Event{Type: api.EventPurchaseCommitmentDisposed, AggregateType: "PurchaseCommitment", AggregateID: c.ID,
		AggregateVersion: c.RowVersion, Fields: map[string]any{"commitment_id": c.ID, "disposition_id": x.ID, "order_id": x.OrderID,
			"order_line_id": x.OrderLineID, "kind": x.Kind, "quantity": text(x.Quantity), "unit_id": x.Unit.ID, "decided_by": d.Actor.ID}}
	return dispositionOutcome(x, event), nil
}

// DecideDisposition approves or rejects a requested disposition (purchase.commitment.disposition.decide;
// S5R-D05, PUR-INV-013). Human only, independent of the requester.
func (s Service) DecideDisposition(ctx context.Context, cmd api.DecideDisposition) (api.DispositionReceipt, error) {
	h := &decideDisposition{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandDecideDisposition, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.DispositionID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.DispositionReceipt{}, err
	}
	return s.dispositionReceipt(ctx, res)
}

// dispositionReceipt completes res with what the stored disposition disposes of.
func (s Service) dispositionReceipt(ctx context.Context, res api.Receipt) (api.DispositionReceipt, error) {
	out := api.DispositionReceipt{Receipt: res}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		x, err := postgres.LoadDisposition(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || x == nil {
			return err
		}
		out.CommitmentID, out.OrderID, out.OrderLineID, out.Disposition, out.Quantity = x.CommitmentID, x.OrderID, x.OrderLineID, x.Kind,
			text(x.Quantity)
		return nil
	})
	return out, err
}

// dispositionViews returns xs, in their order (by disposition ID), as the commitment read shows them.
func dispositionViews(xs []*commitment.Disposition) []api.DispositionView {
	out := make([]api.DispositionView, 0, len(xs))
	for _, x := range xs {
		v := api.DispositionView{DispositionID: x.ID, OrderLineID: x.OrderLineID, Status: x.Status, RowVersion: x.RowVersion, Disposition: x.Kind,
			Quantity: text(x.Quantity), UnitID: x.Unit.ID, UnitRevision: x.Unit.Revision, Reason: x.Reason, Evidence: evidenceView(x.Evidence),
			RequestedBy: x.Requested.By, RequestedAt: x.Requested.At, RequestEpoch: x.Requested.Epoch}
		if dc := x.Decision; dc != nil {
			v.Decision, v.DecidedBy, v.DecidedAt, v.DecideEpoch, v.DecisionReason = api.DecisionReject, dc.By, dc.At, dc.Epoch, dc.Reason
			if dc.Approve {
				v.Decision = api.DecisionApprove
			}
		}
		out = append(out, v)
	}
	return out
}
