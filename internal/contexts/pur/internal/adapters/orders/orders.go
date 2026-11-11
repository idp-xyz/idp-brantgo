// Package orders implements PUR's port to TRD (application.Orders) on TRD's published contract
// (Context Map R19 TRD→PUR, ActiveOrderRef v1 with guardLock): ActiveOrderLookups before PUR's
// transaction, OrderGuard v1 inside it with PUR's own executor. It never calls TRD's transaction
// owner.
package orders

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/application"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
)

// TRD answers PUR's port from TRD's api.
type TRD struct {
	Lookups trdapi.ActiveOrderLookups
}

var _ application.Orders = TRD{}

// ResolveOrders asks TRD's ActiveOrderLookups (an active grant, TRD's data scope) before PUR's
// transaction and returns what TRD says of each visible order.
func (t TRD) ResolveOrders(ctx context.Context, ids []string) (map[string]application.Order, error) {
	out := map[string]application.Order{}
	if err := trdapi.ValidateOrderIDs(ids); err != nil || len(ids) == 0 {
		return out, err
	}
	facts, err := t.Lookups.ResolveActiveOrders(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, f := range facts {
		out[id] = orderOf(f)
	}
	return out, nil
}

// GuardOrder runs TRD's OrderGuard v1 (brant_trd.guard_order_active_v1) with PUR's executor, inside
// PUR's transaction after PUR locked its own roots: it locks the order row FOR SHARE until PUR's
// transaction ends.
func (TRD) GuardOrder(ctx context.Context, exec bentopg.Executor, b application.OrderBinding) (application.OrderVerdict, error) {
	binding := trdapi.OrderBinding{OrderID: b.OrderID, OrderRevision: b.OrderRevision, OrderHash: b.OrderHash}
	v, err := trdapi.ScanOrderGuard(binding, exec.QueryRow(ctx, trdapi.OrderGuardSQL, trdapi.OrderGuardArgs(binding)...).Scan)
	if err != nil {
		return application.OrderVerdict{}, err
	}
	return application.OrderVerdict{Known: v.Known, Status: v.Status, Code: v.Code}, nil
}

// orderOf is TRD's ActiveOrderFact as PUR's port carries it.
func orderOf(f trdapi.ActiveOrderFact) application.Order {
	o := application.Order{Binding: application.OrderBinding{OrderID: f.Binding.OrderID, OrderRevision: f.Binding.OrderRevision,
		OrderHash: f.Binding.OrderHash}, Status: f.Status, Code: f.Code, QuoteID: f.QuoteID, BusinessRevision: f.BusinessRevision,
		BusinessHash: f.BusinessHash, ReviewID: f.ReviewID, CustomerID: f.CustomerRef.PartyID}
	if r := f.RfqRef; r.RfqID != "" {
		o.Rfq = &application.OrderRfq{RfqID: r.RfqID, RowVersion: r.RowVersion}
	}
	if s := f.CostSnapshotRef; s.SnapshotID != "" {
		o.CostSnapshot = &application.OrderSnapshot{SnapshotID: s.SnapshotID, ResultHash: s.ResultHash}
	}
	if d := f.Delivery; d.Kind != "" {
		o.Delivery = &application.OrderDelivery{Kind: d.Kind, Date: d.Date, Milestone: d.Milestone, PlaceID: d.PlaceRef.ID, TimeZone: d.TimeZone}
	}
	for _, l := range f.Lines {
		o.Lines = append(o.Lines, application.OrderLine{LineID: l.LineID, RfqLineID: l.RfqLineID, SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision,
			Quantity: l.Quantity, Unit: application.OrderUnit{ID: l.UnitRef.ID, Revision: l.UnitRef.Revision}})
	}
	return o
}
