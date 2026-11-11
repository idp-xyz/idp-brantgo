package application

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/allocation"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// requestOf is the domain's request for cmd, whose structure Validate checked.
func requestOf(cmd api.PrepareDraft) draft.Request {
	r := draft.Request{DraftID: cmd.DraftID, OrderID: cmd.OrderID, SelectionReason: cmd.SelectionReason}
	for _, l := range cmd.Lines {
		q, _ := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		x := draft.RequestLine{OrderLineID: l.OrderLineID, Quantity: q, Source: draft.Source{Ref: draft.Ref{Kind: l.Source.Kind, ID: l.Source.ID,
			Revision: l.Source.Revision}, ConditionLineID: l.Source.ConditionLineID}}
		if o := l.Overbuy; o != nil {
			oq, _ := kernel.ParseDecimal(o.Quantity, kernel.QuantityFormat)
			x.Overbuy = &draft.Overbuy{Quantity: oq, Reason: o.Reason}
		}
		r.Lines = append(r.Lines, x)
	}
	return r
}

// prepareCommand runs purchase.draft.prepare. Before the transaction it reads the upstream facts;
// inside it, it locks the draft row (a taken ID) or the order lines' DemandAllocation roots FOR
// UPDATE in (order_id, order_line_id) order, decides, re-checks every adopted reference through its
// owner's guard and writes the draft, its lines, the roots and their entries.
type prepareCommand struct {
	s        Service
	req      draft.Request
	in       draft.Inputs
	order    *Order
	existing *draft.Draft
	roots    map[allocation.Key]*allocation.Allocation
	replay   bool
}

func (h *prepareCommand) Prepare(ctx context.Context) error {
	var taken bool
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		d, err := postgres.LoadDraft(ctx, exec, scope(ctx), h.req.DraftID, postgres.NoLock)
		taken = d != nil
		return err
	})
	if err != nil || taken {
		// A taken draft ID is answered from the stored draft; nothing upstream is asked.
		return err
	}
	h.in, h.order, err = h.s.facts(ctx, h.req, h.s.Clock.Now())
	return err
}

func (h *prepareCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.existing, h.roots, h.replay = nil, nil, false
	sc := scope(ctx)
	d, err := postgres.LoadDraft(ctx, exec, sc, h.req.DraftID, postgres.ForUpdate)
	if err != nil || d != nil {
		h.existing = d
		return err
	}
	var keys []allocation.Key
	for _, l := range h.req.Lines {
		keys = append(keys, allocation.Key{OrderID: h.req.OrderID, LineID: l.OrderLineID})
	}
	slices.SortFunc(keys, allocation.Compare)
	h.roots, err = postgres.LoadAllocations(ctx, exec, sc, keys, postgres.ForUpdate)
	return err
}

func (h *prepareCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc := scope(ctx)
	if dr := h.existing; dr != nil {
		if !d.Sees(dr.OwnerActor, dr.OwnerTeam) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/draft_id", "")
		}
		if err := dr.Verify(sc, "/args/draft_id"); err != nil {
			return action.Outcome{}, err
		}
		if err := dr.Answer(h.req, d.Actor); err != nil {
			return action.Outcome{}, err
		}
		h.replay = true
		return action.Outcome{ObjectID: dr.ID, RowVersion: dr.RowVersion, Status: dr.Status}, nil
	}
	if h.order == nil {
		// Taken at lookup and gone now cannot happen (drafts are never deleted); TRD has no such order.
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/order_id", "")
	}
	in := h.in
	in.Scope, in.Now, in.Preparer, in.Epoch, in.Allocations = sc, kernel.Instant(h.s.Clock.Now()), d.Actor, d.Epoch, h.roots
	if slices.ContainsFunc(h.req.Lines, func(l draft.RequestLine) bool { return l.Overbuy != nil }) {
		own, err := h.s.Ledger.Authorizer.Qualify(ctx, exec, d.Actor.ID)
		if err != nil {
			return action.Outcome{}, err
		}
		in.OverbuyApproved = own.Holds(api.PermissionApproveOverbuy)
	}
	p, err := draft.Prepare(h.req, in)
	if err != nil {
		return action.Outcome{}, err
	}
	adopt, err := h.s.guard(ctx, exec, p, h.order.Binding, in.Now)
	if err != nil {
		return action.Outcome{}, err
	}
	dr, err := p.Seal(adopt.Status, adopt.Code, adopt.Input, d.Team)
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertDraft(ctx, exec, sc, dr); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveAllocations(ctx, exec, sc, p.Allocations, in.Now); err != nil {
		return action.Outcome{}, err
	}
	lineIDs := make([]string, len(dr.Lines))
	for i, l := range dr.Lines {
		lineIDs[i] = l.OrderLineID
	}
	slices.Sort(lineIDs)
	event := action.Event{Type: api.EventPurchaseDraftPrepared, AggregateType: "PurchaseDraft", AggregateID: dr.ID, AggregateVersion: dr.RowVersion,
		Fields: map[string]any{"draft_id": dr.ID, "order_id": dr.OrderID, "order_revision": dr.OrderRevision, "order_hash": dr.OrderHash,
			"supplier_id": dr.SupplierID, "draft_hash": dr.Hash, "currency": dr.Currency, "commitment_state": dr.CommitmentState,
			"order_line_ids": lineIDs, "supply_valid_until": kernel.FormatTimestamp(dr.SupplyValidUntil)}}
	return action.Outcome{ObjectID: dr.ID, RowVersion: dr.RowVersion, Status: dr.Status, Events: []action.Event{event}}, nil
}

// adoption is AdoptabilityGuard v1's verdict on the order's cost snapshot.
type adoption struct{ Status, Code, Input string }

// guard re-checks, inside PUR's transaction and after its roots are locked, every reference the
// preparation adopted, through the owners' guard functions with PUR's executor, in context code
// order (ADR-BR-011): CAT's SkuGuard v1 for each specification revision, CST's AdoptabilityGuard v1
// for the cost snapshot (recorded, never blocking: R21), POL's ConfigGuard v1 for each
// configuration, PTY's PartyGuard v1 for the supplier at the row version the supply conditions bind
// (with the supplier role), SUP's SupplyConditionGuard v1 for each supply condition revision (its
// dependent source offer included) and TRD's OrderGuard v1 for the order at its revision and hash.
// Each locks its rows FOR SHARE until PUR commits, so an upstream change either committed first and
// is refused here, or waits. A configuration or supply condition whose content hash moved since the
// lookup is REFERENCE_STALE.
func (s Service) guard(ctx context.Context, exec bentopg.Executor, p *draft.Prepared, order OrderBinding, at time.Time) (adoption, error) {
	specs := slices.SortedFunc(maps.Keys(p.Specs), func(a, b draft.Spec) int {
		return cmp.Or(cmp.Compare(a.SkuID, b.SkuID), cmp.Compare(a.Revision, b.Revision))
	})
	for _, sp := range specs {
		ref := catapi.SpecRef{SkuID: sp.SkuID, SpecRevision: sp.Revision}
		v, err := catapi.ScanSkuGuard(ref, exec.QueryRow(ctx, catapi.SkuGuardSQL, catapi.SkuGuardArgs(ref)...).Scan)
		if err != nil {
			return adoption{}, err
		}
		if err = v.Check(p.Specs[sp]); err != nil {
			return adoption{}, err
		}
	}
	snap := cstapi.CostSnapshotRef{SnapshotID: p.Snapshot.SnapshotID, ResultHash: p.Snapshot.ResultHash}
	a, err := cstapi.ScanAdoptabilityGuard(snap, exec.QueryRow(ctx, cstapi.AdoptabilityGuardSQL, cstapi.AdoptabilityGuardArgs(snap, at)...).Scan)
	if err != nil {
		return adoption{}, err
	}
	if !a.Known {
		return adoption{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "The order's cost snapshot is not CST's")
	}
	configs := slices.SortedFunc(maps.Keys(p.Configs), func(a, b draft.ConfigKey) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Revision, b.Revision))
	})
	for _, k := range configs {
		use := p.Configs[k]
		ref := polapi.ConfigRef{Kind: k.Kind, ConfigID: k.ID, Revision: k.Revision}
		v, err := polapi.ScanConfigGuard(ref, exec.QueryRow(ctx, polapi.ConfigGuardSQL, polapi.ConfigGuardArgs(ref, at)...).Scan)
		if err != nil {
			return adoption{}, err
		}
		if err = v.Check(use.Path); err != nil {
			return adoption{}, err
		}
		if v.ContentHash != use.ContentHash {
			return adoption{}, api.Reject(api.CodeReferenceStale, use.Path)
		}
	}
	parties := slices.Clone(p.Parties)
	slices.SortFunc(parties, func(a, b draft.PartyUse) int { return cmp.Compare(a.PartyID, b.PartyID) })
	for _, u := range parties {
		b := ptyapi.PartyBinding{PartyID: u.PartyID, RowVersion: u.RowVersion}
		v, err := ptyapi.ScanPartyGuard(b, exec.QueryRow(ctx, ptyapi.PartyGuardSQL, ptyapi.PartyGuardArgs(b)...).Scan)
		if err != nil {
			return adoption{}, err
		}
		if err = v.Check(u.Path); err != nil {
			return adoption{}, err
		}
		if !v.HasRole(ptyapi.RoleSupplier) {
			return adoption{}, kernel.Reject(kernel.CodeReferenceInvalid, u.Path, "The party is not a supplier")
		}
	}
	sources := slices.SortedFunc(maps.Keys(p.Sources), func(a, b draft.Ref) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Revision, b.Revision))
	})
	for _, r := range sources {
		use := p.Sources[r]
		ref := supapi.SupplyConditionRef{Kind: r.Kind, ID: r.ID, Revision: r.Revision}
		v, err := supapi.ScanSupplyConditionGuard(ref, exec.QueryRow(ctx, supapi.SupplyConditionGuardSQL, supapi.SupplyConditionGuardArgs(ref, at)...).Scan)
		if err != nil {
			return adoption{}, err
		}
		if err = v.Check(use.Path); err != nil {
			return adoption{}, err
		}
		if v.ContentHash != use.ContentHash {
			return adoption{}, api.Reject(api.CodeReferenceStale, use.Path)
		}
	}
	v, err := s.Upstream.Orders.GuardOrder(ctx, exec, order)
	if err != nil {
		return adoption{}, err
	}
	if err = orderCheck(v, "/args/order_id"); err != nil {
		return adoption{}, err
	}
	return adoption{Status: a.Status, Code: a.Code, Input: a.Input}, nil
}

// orderCheck passes on OrderGuard v1's verdict at path.
func orderCheck(v OrderVerdict, path string) error {
	switch {
	case !v.Known:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	case v.Code == "":
		return nil
	case v.Code == OrderNotActive || v.Code == OrderApprovalRequired || v.Code == OrderApprovalRevoked || v.Code == OrderStale:
		return api.Reject(v.Code, path)
	}
	return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
}

// PrepareDraft prepares a purchase draft (purchase.draft.prepare; Python t04_purchase prepare).
func (s Service) PrepareDraft(ctx context.Context, cmd api.PrepareDraft) (api.DraftReceipt, error) {
	h := &prepareCommand{s: s, req: requestOf(cmd)}
	err := cmd.Validate()
	if err != nil {
		h.req = draft.Request{}
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandPrepareDraft, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.DraftID},
		cmd, err, h)
	if err != nil {
		return api.DraftReceipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return s.receipt(ctx, res)
}

// receipt completes res with what the stored draft binds.
func (s Service) receipt(ctx context.Context, res api.Receipt) (api.DraftReceipt, error) {
	out := api.DraftReceipt{Receipt: res}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		dr, err := postgres.LoadDraft(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || dr == nil {
			return err
		}
		doc, err := dr.Decode()
		if err != nil {
			return err
		}
		out.OrderID, out.OrderRevision, out.OrderHash, out.SupplierID, out.Currency = dr.OrderID, dr.OrderRevision, dr.OrderHash, dr.SupplierID, dr.Currency
		out.DraftHash, out.CommitmentState, out.SupplyValidUntil = dr.Hash, dr.CommitmentState, dr.SupplyValidUntil
		out.NetMinor, out.TaxMinor, out.GrossMinor = doc.Totals.NetMinor, doc.Totals.TaxMinor, doc.Totals.GrossMinor
		return nil
	})
	return out, err
}
