package application

import (
	"context"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func invalidID(s Service, ctx context.Context, command string) error {
	err := kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	if auditErr := s.Ledger.AuditRejected(ctx, command, "", kernel.CodeSchemaInvalid); auditErr != nil {
		return auditErr
	}
	return err
}

// GetShipment returns a shipment the caller sees, audited as shipment.get (ful.read): NOT_FOUND
// otherwise.
func (s Service) GetShipment(ctx context.Context, id string) (api.ShipmentView, error) {
	if !kernel.ValidIdentifier(id) {
		return api.ShipmentView{}, invalidID(s, ctx, api.CommandGetShipment)
	}
	var v api.ShipmentView
	err := s.Ledger.Read(ctx, api.CommandGetShipment, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sh, err := postgres.LoadShipment(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if sh == nil || !d.Sees(sh.OwnerActor, sh.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = shipmentView(sh)
		return nil
	})
	return v, err
}

func shipmentView(s *shipment.Shipment) api.ShipmentView {
	v := api.ShipmentView{ShipmentID: s.ID, Status: s.Status, RowVersion: s.RowVersion, Route: s.Route, Order: api.OrderBinding{OrderID: s.Order.OrderID,
		OrderRevision: s.Order.OrderRevision, OrderHash: s.Order.OrderHash}, CustomerID: s.CustomerID, Lines: []api.ShipmentLineView{},
		Parties: []api.ShipmentParty{}, PreparedBy: s.Prepared.By, PreparedAt: s.Prepared.At, AuthorityEpoch: s.Prepared.Epoch}
	for _, l := range s.Lines {
		v.Lines = append(v.Lines, api.ShipmentLineView{LineID: l.ID, OrderLineID: l.OrderLineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision,
			UnitID: l.UnitID, UnitRevision: l.UnitRevision, OrderLineQuantity: postgres.Text(l.OrderQuantity), Quantity: postgres.Text(l.Quantity),
			SupplierDelivery: api.DeliveryLineRef{DeliveryID: l.Delivery.DeliveryID, LineID: l.Delivery.LineID, Version: l.DeliveryVersion},
			CommitmentID:     l.CommitmentID, SupplierID: l.SupplierID, QualityReleaseID: l.ReleaseID})
	}
	for _, p := range s.Parties {
		v.Parties = append(v.Parties, api.ShipmentParty(p))
	}
	v.PackingPlan, v.ActualPacking = packingView(s.Plan), packingView(s.Actual)
	if c := s.Confirmation; c != nil {
		cv := &api.ConfirmationView{ConfirmedBy: c.Confirmed.By, ConfirmedAt: c.Confirmed.At, AuthorityEpoch: c.Confirmed.Epoch,
			EvidenceIDs: slices.Clone(c.Evidence), Guards: []api.GuardEvidence{}, GuardsSHA256: c.EntriesSHA256, UncheckedGuards: slices.Clone(c.Unchecked)}
		for _, e := range c.Entries {
			cv.Guards = append(cv.Guards, api.GuardEvidence(e))
		}
		v.Confirmation = cv
	}
	if x := s.Cancellation; x != nil {
		v.Cancellation = &api.CancellationView{Reason: x.Reason, CancelledBy: x.Cancelled.By, CancelledAt: x.Cancelled.At, AuthorityEpoch: x.Cancelled.Epoch}
	}
	return v
}

func packingView(p *shipment.Packing) *api.PackingView {
	if p == nil {
		return nil
	}
	text := func(d *kernel.Decimal) string {
		if d == nil {
			return ""
		}
		return postgres.Text(*d)
	}
	v := &api.PackingView{Kind: p.Kind, Version: p.Seq, Packages: []api.Package{}, CartonCount: p.Cartons(), EvidenceIDs: slices.Clone(p.Evidence),
		RecordedBy: p.Recorded.By, RecordedAt: p.Recorded.At, AuthorityEpoch: p.Recorded.Epoch}
	for _, k := range p.Packages {
		pk := api.Package{Cartons: k.Cartons, CartonFrom: k.CartonFrom, GrossWeightKg: text(k.Gross), NetWeightKg: text(k.Net), VolumeCbm: text(k.Volume)}
		for _, c := range k.Contents {
			pk.Contents = append(pk.Contents, api.PackageContent{LineID: c.LineID, QuantityPerCarton: postgres.Text(c.PerCarton)})
		}
		v.Packages = append(v.Packages, pk)
	}
	if gross, net, volume, ok := p.Totals(); ok {
		v.GrossWeightKg, v.NetWeightKg, v.VolumeCbm = postgres.Text(gross), postgres.Text(net), postgres.Text(volume)
	}
	return v
}

// GetFulfilment returns the fulfilment of an order TRD shows the caller, audited as
// order_fulfilment.get (ful.read): the order's lines at its current revision with what its
// confirmed shipments shipped of each over every revision, what remains and what prepared
// shipments plan, and its shipments — those outside the caller's data scope without their IDs.
// NOT_FOUND for an order TRD does not show the caller.
func (s Service) GetFulfilment(ctx context.Context, orderID string) (api.FulfilmentView, error) {
	if !kernel.ValidIdentifier(orderID) {
		return api.FulfilmentView{}, invalidID(s, ctx, api.CommandGetFulfilment)
	}
	orders, err := s.Upstream.Orders.ResolveActiveOrders(ctx, []string{orderID})
	if err != nil {
		return api.FulfilmentView{}, err
	}
	fact, known := orders[orderID]
	var v api.FulfilmentView
	err = s.Ledger.Read(ctx, api.CommandGetFulfilment, orderID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		if !known {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		list, sums, err := postgres.LoadOrderShipments(ctx, exec, scope(ctx), orderID)
		if err != nil {
			return err
		}
		v = fulfilmentView(fact, list, sums, d)
		v.CheckedAt = kernel.Instant(s.Clock.Now())
		return nil
	})
	return v, err
}

func fulfilmentView(f trdapi.ActiveOrderFact, list []postgres.OrderShipment, sums []postgres.OrderLineSums, d authorization.Decision) api.FulfilmentView {
	v := api.FulfilmentView{Order: api.OrderBinding{OrderID: f.Binding.OrderID, OrderRevision: f.Binding.OrderRevision, OrderHash: f.Binding.OrderHash},
		OrderStatus: f.Status, Lines: []api.FulfilmentLine{}, Shipments: []api.FulfilmentShipment{}}
	used := make([]bool, len(sums))
	for _, l := range f.Lines {
		line := api.FulfilmentLine{OrderLineID: l.LineID, UnitID: l.UnitRef.ID, UnitRevision: l.UnitRef.Revision, Shipped: "0", Prepared: "0"}
		ordered, _ := api.Quantity(l.Quantity)
		line.OrderQuantity = postgres.Text(ordered)
		var shipped kernel.Decimal
		for i, x := range sums {
			if x.OrderLineID == l.LineID && x.UnitID == l.UnitRef.ID && x.UnitRevision == l.UnitRef.Revision {
				shipped, used[i] = x.Shipped, true
				line.Shipped, line.Prepared = postgres.Text(x.Shipped), postgres.Text(x.Prepared)
			}
		}
		remaining := ordered.Sub(shipped)
		if remaining.Sign() < 0 {
			remaining = kernel.Decimal{}
		}
		line.Remaining = postgres.Text(remaining)
		v.Lines = append(v.Lines, line)
	}
	// What earlier revisions shipped of lines or units the current revision no longer has.
	for i, x := range sums {
		if !used[i] {
			v.Lines = append(v.Lines, api.FulfilmentLine{OrderLineID: x.OrderLineID, UnitID: x.UnitID, UnitRevision: x.UnitRevision,
				Shipped: postgres.Text(x.Shipped), Prepared: postgres.Text(x.Prepared)})
		}
	}
	for _, s := range list {
		e := api.FulfilmentShipment{Status: s.Status, OrderRevision: s.OrderRevision, ConfirmedAt: s.ConfirmedAt}
		if d.Sees(s.OwnerActor, s.OwnerTeam) {
			e.ShipmentID = s.ID
		}
		v.Shipments = append(v.Shipments, e)
	}
	return v
}

// ResolveOrderShipped answers ShipmentFacts v1 before the caller's transaction (api.ShipmentFacts,
// R70): what FUL's confirmed shipments shipped of each order's lines, in a transaction of FUL's
// through the same function TRD calls inside its own. It needs an active grant only and applies no
// data scope; it is not audited.
func (s Service) ResolveOrderShipped(ctx context.Context, orderIDs []string) (map[string][]api.ShippedLine, error) {
	out := map[string][]api.ShippedLine{}
	if err := api.ValidateOrderIDs(orderIDs); err != nil || len(orderIDs) == 0 {
		return out, err
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		for _, id := range orderIDs {
			rows, err := exec.Query(ctx, api.OrderShippedSQL, api.OrderShippedArgs(id)...)
			if err != nil {
				return err
			}
			lines := []api.ShippedLine{}
			for rows.Next() {
				l, err := api.ScanOrderShipped(rows.Scan)
				if err != nil {
					rows.Close()
					return err
				}
				lines = append(lines, l)
			}
			rows.Close()
			if err = rows.Err(); err != nil {
				return err
			}
			out[id] = lines
		}
		return nil
	})
	return out, err
}
