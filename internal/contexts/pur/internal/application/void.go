package application

import (
	"context"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/allocation"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// voidCommand runs purchase.draft.void: it locks the draft FOR UPDATE, then the DemandAllocation
// roots of its lines in (order_id, order_line_id) order, voids the draft and releases its entries.
// It reads nothing upstream: voiding judges PUR's own facts, and it only removes from what
// DownstreamCommitments v1 shows, so it takes no order lock.
type voidCommand struct {
	s        Service
	cmd      api.VoidDraft
	existing *draft.Draft
	roots    map[allocation.Key]*allocation.Allocation
}

func (h *voidCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.existing, h.roots = nil, nil
	sc := scope(ctx)
	d, err := postgres.LoadDraft(ctx, exec, sc, h.cmd.DraftID, postgres.ForUpdate)
	if err != nil || d == nil {
		return err
	}
	h.existing = d
	var keys []allocation.Key
	for _, l := range d.Lines {
		if l.OrderLineID != "" {
			keys = append(keys, allocation.Key{OrderID: d.OrderID, LineID: l.OrderLineID})
		}
	}
	slices.SortFunc(keys, allocation.Compare)
	h.roots, err = postgres.LoadAllocations(ctx, exec, sc, keys, postgres.ForUpdate)
	return err
}

// Apply refuses, in this order: a draft the caller does not see (NOT_FOUND), a record that is not
// intact (REFERENCE_INVALID), another version (VERSION_CONFLICT), a committed draft — a purchase
// commitment of it was issued (PURCHASE_DRAFT_COMMITTED, PUR-INV-010, TD-AT06) — and a voided one
// (INVALID_STATE).
func (h *voidCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc := scope(ctx)
	dr := h.existing
	if dr == nil || !d.Sees(dr.OwnerActor, dr.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/draft_id", "")
	}
	if err := dr.Verify(sc, "/args/draft_id"); err != nil {
		return action.Outcome{}, err
	}
	// Read after the draft's allocation roots are locked: an issue of its commitment, which locks the
	// same roots, has committed and is seen (PUR-INV-010).
	owners, err := postgres.CommitmentsOfDraft(ctx, exec, sc, dr.ID)
	if err != nil {
		return action.Outcome{}, err
	}
	committed := slices.ContainsFunc(owners, func(o postgres.CommitmentOwner) bool { return commitment.IsCommitted(o.Status) })
	if err := dr.Voidable(h.cmd.ExpectedVersion, committed); err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	var changed []*allocation.Allocation
	for _, l := range dr.Lines {
		a := h.roots[allocation.Key{OrderID: dr.OrderID, LineID: l.OrderLineID}]
		if a == nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/draft_id", "The draft's allocation is missing")
		}
		if _, err := a.Release(dr.ID, now); err != nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/draft_id", "The draft's allocation is not effective")
		}
		changed = append(changed, a)
	}
	allocation.Sort(changed)
	from := dr.RowVersion
	dr.VoidBy(d.Actor, d.Epoch, now, h.cmd.Reason)
	if err := postgres.VoidDraft(ctx, exec, sc, dr, from); err != nil {
		return action.Outcome{}, err
	}
	if err := postgres.SaveAllocations(ctx, exec, sc, changed, now); err != nil {
		return action.Outcome{}, err
	}
	lineIDs := make([]string, len(dr.Lines))
	for i, l := range dr.Lines {
		lineIDs[i] = l.OrderLineID
	}
	slices.Sort(lineIDs)
	event := action.Event{Type: api.EventPurchaseDraftVoided, AggregateType: "PurchaseDraft", AggregateID: dr.ID, AggregateVersion: dr.RowVersion,
		Fields: map[string]any{"draft_id": dr.ID, "order_id": dr.OrderID, "order_revision": dr.OrderRevision, "supplier_id": dr.SupplierID,
			"draft_hash": dr.Hash, "released_order_line_ids": lineIDs}}
	return action.Outcome{ObjectID: dr.ID, RowVersion: dr.RowVersion, Status: dr.Status, Events: []action.Event{event}}, nil
}

// VoidDraft voids a purchase draft not committed to the supplier and releases its allocations
// (purchase.draft.void; SM-PUR-DRAFT, PUR-INV-007, TD-AT06). Python has no void (A46 限制: 不支持草稿
// 作废): an intentional extension the tactical model registers.
func (s Service) VoidDraft(ctx context.Context, cmd api.VoidDraft) (api.DraftReceipt, error) {
	h := &voidCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandVoidDraft, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.DraftID, Reason: cmd.Reason},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.DraftReceipt{}, err
	}
	return s.receipt(ctx, res)
}
