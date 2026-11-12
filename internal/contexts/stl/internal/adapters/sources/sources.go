// Package sources implements STL's ports to the sources of settlement statements on the upstreams'
// published contracts, before STL's transaction (S5-04b): FUL's ShipmentLineLookups (Context Map
// R30, ShipmentLineRef v1) and PUR's DeliveryLookups (R31, PurchaseSettlementSource v1). Neither
// relationship has a guard lock: a confirmed shipment line and a verified delivery version never
// change, and STL keeps the settlement state itself (STL-INV-001). STL never calls FUL's or PUR's
// transaction owner.
package sources

import (
	"context"

	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/application"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// FUL answers STL's port Shipments from FUL's api. Lookups returns FUL's lookups once FUL is
// composed: FUL is built after STL, so bootstrap binds it by a function (S5 decision D-2); nil means
// FUL is not there, and every line is absent.
type FUL struct {
	Lookups func() fulapi.ShipmentLineLookups
}

var _ application.Shipments = FUL{}

// ResolveShipmentSources asks FUL's ResolveShipmentLines (an active grant, FUL's data scope).
func (f FUL) ResolveShipmentSources(ctx context.Context, keys []api.SettlementSourceRef) (map[api.SettlementSourceRef]application.ShipmentSource, error) {
	out := map[api.SettlementSourceRef]application.ShipmentSource{}
	var lookups fulapi.ShipmentLineLookups
	if f.Lookups != nil {
		lookups = f.Lookups()
	}
	if len(keys) == 0 || lookups == nil {
		return out, nil
	}
	ask := make([]fulapi.ShipmentLineKey, 0, len(keys))
	for _, k := range keys {
		ask = append(ask, fulapi.ShipmentLineKey{ShipmentID: k.ShipmentID, LineID: k.LineID})
	}
	found, err := lookups.ResolveShipmentLines(ctx, ask)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		r, ok := found[fulapi.ShipmentLineKey{ShipmentID: k.ShipmentID, LineID: k.LineID}]
		if !ok {
			continue
		}
		l := settlement.ShipmentLine{ShipmentID: r.Key.ShipmentID, LineID: r.Key.LineID, ShipmentVersion: r.ShipmentVersion,
			ShippedAt: r.ConfirmedAt, OrderID: r.Order.OrderID, OrderRevision: r.Order.OrderRevision, OrderHash: r.Order.OrderHash,
			CustomerID: r.CustomerID, OrderLineID: r.OrderLineID, SkuID: r.SkuID, SpecRevision: r.SpecRevision, UnitID: r.UnitID,
			UnitRevision: r.UnitRevision, OrderLineQuantity: r.OrderLineQuantity, Quantity: r.Quantity, Net: r.Amounts.NetMinor,
			Tax: r.Amounts.TaxMinor, Payable: r.Amounts.PayableMinor, CommitmentID: r.CommitmentID, SupplierID: r.SupplierID}
		if p := r.Price; p != nil {
			l.Currency, l.UnitPrice, l.BasisNet, l.BasisTax = p.Currency.ID, p.UnitPrice, p.NetMinor, p.TaxMinor
		}
		out[k] = application.ShipmentSource{Line: l, Check: r.Check}
	}
	return out, nil
}

// PUR answers STL's port Deliveries from PUR's api.
type PUR struct {
	Lookups purapi.DeliveryLookups
}

var _ application.Deliveries = PUR{}

// ResolveDeliverySources asks PUR's ResolveSettlementSources (an active grant, PUR's data scope).
func (p PUR) ResolveDeliverySources(ctx context.Context, refs []api.SettlementSourceRef) (map[api.SettlementSourceRef]application.DeliverySource, error) {
	out := map[api.SettlementSourceRef]application.DeliverySource{}
	if len(refs) == 0 || p.Lookups == nil {
		return out, nil
	}
	ask := make([]purapi.DeliveryLineRef, 0, len(refs))
	for _, r := range refs {
		ask = append(ask, purapi.DeliveryLineRef{DeliveryID: r.DeliveryID, LineID: r.LineID, Version: r.Version})
	}
	found, err := p.Lookups.ResolveSettlementSources(ctx, ask)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		s, ok := found[purapi.DeliveryLineRef{DeliveryID: r.DeliveryID, LineID: r.LineID, Version: r.Version}]
		if !ok {
			continue
		}
		c := s.Line
		l := settlement.DeliveryLine{DeliveryID: s.Ref.DeliveryID, LineID: s.Ref.LineID, Version: s.Ref.Version, HandedOverAt: kernel.Instant(s.HandedOverAt),
			CommitmentID: c.Ref.CommitmentID, SupplierID: c.SupplierID, OrderID: c.OrderID, OrderRevision: c.OrderRevision, OrderHash: c.OrderHash,
			SkuID: c.SkuID, SpecRevision: c.SpecRevision, UnitID: c.UnitID, UnitRevision: c.UnitRevision, PurchaseQuantity: c.PurchaseQuantity,
			VerifiedQuantity: s.VerifiedQuantity, Currency: s.Currency, MinorUnits: s.MinorUnits, LineNet: s.LineNetMinor, LineTax: s.LineTaxMinor}
		out[r] = application.DeliverySource{Line: l, Check: s.Check}
	}
	return out, nil
}

var (
	_ application.OrderShipments = FUL{}
	_ application.OrderPurchases = PUR{}
)

// ResolveOrderShipments asks FUL's ResolveOrderShipmentLines (an active grant, FUL's data scope) for the lines of
// every shipment of the orders, in batches of fulapi.MaxOrderLookups.
func (f FUL) ResolveOrderShipments(ctx context.Context, orderIDs []string) (map[string][]application.OrderShipmentLine, error) {
	out := map[string][]application.OrderShipmentLine{}
	var lookups fulapi.ShipmentLineLookups
	if f.Lookups != nil {
		lookups = f.Lookups()
	}
	if lookups == nil {
		return out, nil
	}
	for start := 0; start < len(orderIDs); start += fulapi.MaxOrderLookups {
		found, err := lookups.ResolveOrderShipmentLines(ctx, orderIDs[start:min(start+fulapi.MaxOrderLookups, len(orderIDs))])
		if err != nil {
			return nil, err
		}
		for id, lines := range found {
			list := []application.OrderShipmentLine{}
			for _, r := range lines {
				list = append(list, application.OrderShipmentLine{ShipmentID: r.Key.ShipmentID, LineID: r.Key.LineID, Status: r.ShipmentStatus,
					OrderLineID: r.OrderLineID, UnitID: r.UnitID, Quantity: r.Quantity})
			}
			out[id] = list
		}
	}
	return out, nil
}

// ResolveOrderPurchases asks PUR for the order's commitments (its audited purchase.commitment.list, pur.read)
// and their lines' PurchaseLineRef v1 (amounts and verified deliveries). PUR refusing the read (no pur.read, an
// order the caller does not see) is "not shown", not an error.
func (p PUR) ResolveOrderPurchases(ctx context.Context, orderID string) ([]application.OrderPurchaseLine, bool, error) {
	q, ok := p.Lookups.(purapi.CommitmentQueries)
	if !ok || p.Lookups == nil {
		return nil, false, nil
	}
	views, err := q.GetOrderCommitments(ctx, orderID)
	if _, refused := kernel.AsRejection(err); refused {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var refs []purapi.CommitmentLineRef
	status := map[string]purapi.CommitmentView{}
	for _, v := range views {
		status[v.CommitmentID] = v
		for _, l := range v.Lines {
			refs = append(refs, purapi.CommitmentLineRef{CommitmentID: v.CommitmentID, OrderLineID: l.OrderLineID})
		}
	}
	var out []application.OrderPurchaseLine
	for start := 0; start < len(refs); start += purapi.MaxCommitmentLineRefs {
		ask := refs[start:min(start+purapi.MaxCommitmentLineRefs, len(refs))]
		found, err := p.Lookups.ResolvePurchaseLines(ctx, ask)
		if err != nil {
			return nil, false, err
		}
		for _, r := range ask {
			l, ok := found[r]
			if !ok {
				continue
			}
			v := status[r.CommitmentID]
			line := application.OrderPurchaseLine{CommitmentID: r.CommitmentID, OrderLineID: r.OrderLineID, Status: v.Status, SupplierID: v.SupplierID,
				Currency: l.Currency, MinorUnits: l.MinorUnits, NetMinor: l.NetMinor}
			for _, d := range l.Deliveries {
				line.Deliveries = append(line.Deliveries, api.SettlementSourceRef{DeliveryID: d.Ref.DeliveryID, LineID: d.Ref.LineID, Version: d.Ref.Version})
			}
			out = append(out, line)
		}
	}
	return out, true, nil
}
