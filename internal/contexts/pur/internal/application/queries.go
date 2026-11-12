package application

import (
	"context"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// upstreamReadable asks TRD and SUP, before PUR's read transaction, whether the caller still reads
// the order and every supply condition revision a draft binds (Python _load: quote access and the
// supply condition roots' access, re-checked on every read).
func (s Service) upstreamReadable(ctx context.Context, dr *draft.Draft) (bool, error) {
	orders, err := s.Upstream.Orders.ResolveOrders(ctx, []string{dr.OrderID})
	if err != nil {
		return false, err
	}
	if _, ok := orders[dr.OrderID]; !ok {
		return false, nil
	}
	doc, derr := dr.Decode()
	if derr != nil {
		return false, nil //nolint:nilerr // a document that does not decode is judged by Verify in the read: REFERENCE_INVALID
	}
	var refs []supapi.SupplyConditionRef
	for _, src := range doc.Sources {
		refs = append(refs, supapi.SupplyConditionRef{Kind: src.Kind, ID: src.ID, Revision: src.Revision})
		if o := src.Origin; o != nil && !slices.Contains(refs, supapi.SupplyConditionRef{Kind: o.Kind, ID: o.ID, Revision: o.Revision}) {
			refs = append(refs, supapi.SupplyConditionRef{Kind: o.Kind, ID: o.ID, Revision: o.Revision})
		}
	}
	found, err := s.resolveSupply(ctx, refs, s.Clock.Now())
	if err != nil {
		return false, err
	}
	for _, r := range refs {
		if _, ok := found[r]; !ok {
			return false, nil
		}
	}
	return true, nil
}

// GetDraft returns a draft the caller sees, audited as purchase.draft.get (pur.read; Python
// t04_purchase_get_draft): NOT_FOUND otherwise; REFERENCE_INVALID when its record is not intact or
// the caller no longer reads its order or a supply condition it binds. Its state is decided now:
// supply_expired once its supply's validity ended, history unchanged; its commitment state committed
// once a purchase commitment of it was issued.
func (s Service) GetDraft(ctx context.Context, id string) (api.DraftView, error) {
	if !kernel.ValidIdentifier(id) {
		err := kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
		if auditErr := s.Ledger.AuditRejected(ctx, api.CommandGetDraft, "", kernel.CodeSchemaInvalid); auditErr != nil {
			return api.DraftView{}, auditErr
		}
		return api.DraftView{}, err
	}
	var stored *draft.Draft
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		stored, err = postgres.LoadDraft(ctx, exec, scope(ctx), id, postgres.NoLock)
		return err
	})
	if err != nil {
		return api.DraftView{}, err
	}
	readable := false
	if stored != nil {
		if readable, err = s.upstreamReadable(ctx, stored); err != nil {
			return api.DraftView{}, err
		}
	}
	var v api.DraftView
	err = s.Ledger.Read(ctx, api.CommandGetDraft, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		dr, err := postgres.LoadDraft(ctx, exec, sc, id, postgres.NoLock)
		if err != nil {
			return err
		}
		if dr == nil || !d.Sees(dr.OwnerActor, dr.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		if err = dr.Verify(sc, "/id"); err != nil {
			return err
		}
		if !readable || stored == nil || stored.Hash != dr.Hash {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/id", "The caller no longer reads the order or a supply condition the draft binds")
		}
		owners, err := postgres.CommitmentsOfDraft(ctx, exec, sc, id)
		if err != nil {
			return err
		}
		if v, err = view(dr, s.Clock.Now()); err != nil {
			return err
		}
		v.CommitmentIDs = []string{}
		for _, o := range owners {
			if commitment.IsCommitted(o.Status) {
				v.CommitmentState = api.CommitmentCommitted
			}
			if d.Sees(o.Actor, o.Team) {
				v.CommitmentIDs = append(v.CommitmentIDs, o.ID)
			}
		}
		return nil
	})
	return v, err
}

// view is a stored, verified draft as the reads show it at instant now, not committed.
func view(dr *draft.Draft, now time.Time) (api.DraftView, error) {
	doc, err := dr.Decode()
	if err != nil {
		return api.DraftView{}, err
	}
	at := kernel.Instant(now)
	v := api.DraftView{DraftID: dr.ID, Status: dr.Status, State: dr.State(at), RowVersion: dr.RowVersion, CommitmentState: dr.CommitmentState,
		OrderID: dr.OrderID, OrderRevision: dr.OrderRevision, OrderHash: dr.OrderHash, SupplierID: dr.SupplierID, Currency: dr.Currency,
		DraftHash: dr.Hash, Document: string(dr.Document), Lines: []api.LineView{}, NetMinor: doc.Totals.NetMinor, TaxMinor: doc.Totals.TaxMinor,
		GrossMinor: doc.Totals.GrossMinor, SelectionReason: doc.SelectionReason, SupplyValidUntil: dr.SupplyValidUntil,
		PreparedBy: dr.Prepared.By, PreparedAt: dr.Prepared.At, AuthorityEpoch: dr.Prepared.Epoch, CheckedAt: at}
	if x := dr.Void; x != nil {
		v.VoidedBy, v.VoidedAt, v.VoidReason, v.VoidEpoch = x.By, x.At, x.Reason, x.Epoch
	}
	for _, l := range doc.Lines {
		lv := api.LineView{OrderLineID: l.OrderLineID, RfqLineID: l.RfqLineID, SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision,
			UnitID: l.UnitRef.ID, OrderQuantity: l.OrderQuantity, AllocatedQuantity: l.AllocatedQuantity, OverbuyQuantity: l.OverbuyQuantity,
			PurchaseQuantity: l.PurchaseQuantity, Source: api.SourceRef{Kind: l.Source.Kind, ID: l.Source.ID, Revision: l.Source.Revision,
				ConditionLineID: l.Source.ConditionLineID}, PriceGroupID: l.PriceGroupID, NetMinor: l.NetMinor, TaxMinor: l.TaxMinor,
			GrossMinor: l.GrossMinor, CostMinor: l.CostMinor, SoldCostMinor: l.SoldCostMinor, SupplierDate: l.Delivery.SupplierDate,
			RequiredDate: l.Delivery.RequiredDate, SlackDays: l.Delivery.SlackDays, SameSource: l.CostingReference.SameSource}
		if l.Overbuy != nil {
			lv.OverbuyReason = l.Overbuy.Reason
		}
		if c := l.CostingReference.CostVarianceMinor; c != nil {
			lv.CostVarianceMinor = *c
		}
		v.Lines = append(v.Lines, lv)
	}
	return v, nil
}

// GetAllocations returns the demand allocation of every drafted line of an order the caller sees
// (TRD's lookup decides that, before PUR's read transaction), audited as demand_allocation.get
// (pur.read). Entries of drafts the caller does not see show their quantity without the draft ID.
func (s Service) GetAllocations(ctx context.Context, orderID string) ([]api.AllocationView, error) {
	if !kernel.ValidIdentifier(orderID) {
		err := kernel.Reject(kernel.CodeSchemaInvalid, "/order_id", "")
		if auditErr := s.Ledger.AuditRejected(ctx, api.CommandGetAllocations, "", kernel.CodeSchemaInvalid); auditErr != nil {
			return nil, auditErr
		}
		return nil, err
	}
	orders, err := s.Upstream.Orders.ResolveOrders(ctx, []string{orderID})
	if err != nil {
		return nil, err
	}
	_, visible := orders[orderID]
	var out []api.AllocationView
	err = s.Ledger.Read(ctx, api.CommandGetAllocations, orderID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		if !visible {
			return kernel.Reject(kernel.CodeNotFound, "/order_id", "")
		}
		sc := scope(ctx)
		roots, err := postgres.AllocationsOfOrder(ctx, exec, sc, orderID)
		if err != nil {
			return err
		}
		var ids []string
		for _, a := range roots {
			for _, e := range a.Entries {
				ids = append(ids, e.DraftID)
			}
		}
		owners, err := postgres.DraftOwners(ctx, exec, sc, ids)
		if err != nil {
			return err
		}
		out = []api.AllocationView{}
		for _, a := range roots {
			v := api.AllocationView{OrderID: a.Key.OrderID, OrderLineID: a.Key.LineID, OrderRevision: a.OrderRevision, UnitID: a.Unit.ID,
				UnitRevision: a.Unit.Revision, Demand: text(a.Demand), Allocated: text(a.Allocated), Remaining: text(a.Remaining()),
				RowVersion: a.RowVersion, Entries: []api.AllocationEntryView{}}
			for _, e := range a.Entries {
				ev := api.AllocationEntryView{Quantity: text(e.Quantity), Effective: e.Effective(), ReleasedAt: e.At}
				if o, ok := owners[e.DraftID]; ok && d.Sees(o.Actor, o.Team) {
					ev.DraftID = e.DraftID
				}
				v.Entries = append(v.Entries, ev)
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

func text(d kernel.Decimal) string {
	s, _ := d.Exact()
	return s
}
