package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// priceFact is what TRD's ActiveOrderRef v1 says of the order's prices at its current revision.
func priceFact(f trdapi.ActiveOrderFact) shipment.PriceFact {
	out := shipment.PriceFact{CurrencyID: f.CurrencyRef.ID, CurrencyRevision: f.CurrencyRef.Revision, Lines: map[string]shipment.LinePriceFact{}}
	for _, l := range f.Lines {
		out.Lines[l.LineID] = shipment.LinePriceFact{UnitPrice: l.UnitPrice, NetMinor: l.NetMinor, TaxMinor: l.TaxMinor, PayableMinor: l.PayableMinor}
	}
	return out
}

// shipmentLineRefs returns ShipmentLineRef v1 of every line of shipment sh, by line ID, with the
// lines' prices and the lots that hold them.
func shipmentLineRefs(sh *shipment.Shipment, prices map[string]shipment.Price, taken map[string]string) []api.ShipmentLineRef {
	out := make([]api.ShipmentLineRef, 0, len(sh.Lines))
	for _, l := range sh.Lines {
		r := api.ShipmentLineRef{Key: api.ShipmentLineKey{ShipmentID: sh.ID, LineID: l.ID}, ShipmentStatus: sh.Status, ShipmentVersion: sh.RowVersion,
			Order:      api.OrderBinding{OrderID: sh.Order.OrderID, OrderRevision: sh.Order.OrderRevision, OrderHash: sh.Order.OrderHash},
			CustomerID: sh.CustomerID, OrderLineID: l.OrderLineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision, UnitID: l.UnitID,
			UnitRevision: l.UnitRevision, OrderLineQuantity: postgres.Text(l.OrderQuantity), Quantity: postgres.Text(l.Quantity),
			SupplierDelivery: api.DeliveryLineRef{DeliveryID: l.Delivery.DeliveryID, LineID: l.Delivery.LineID, Version: l.DeliveryVersion},
			CommitmentID:     l.CommitmentID, SupplierID: l.SupplierID, QualityReleaseID: l.ReleaseID, ExportLotID: taken[l.ID]}
		if c := sh.Confirmation; c != nil {
			r.ConfirmedAt = c.Confirmed.At
		}
		if p, ok := prices[l.ID]; ok {
			r.Price = &api.LinePrice{Currency: api.CurrencyRef{ID: p.CurrencyID, Revision: p.CurrencyRevision}, UnitPrice: postgres.Text(p.UnitPrice),
				OrderLineQuantity: postgres.Text(l.OrderQuantity), NetMinor: postgres.MinorText(p.Net), TaxMinor: postgres.MinorText(p.Tax),
				PayableMinor: postgres.MinorText(p.Payable)}
			r.Amounts = amounts(p.Of(l.Quantity, l.OrderQuantity))
		}
		switch {
		case sh.Status != shipment.StatusConfirmed:
			r.Code = kernel.CodeInvalidState
		case r.Price == nil:
			r.Code = kernel.CodeReferenceInvalid
		}
		out = append(out, r)
	}
	return out
}

// visibleShipment returns shipment id with its line prices and the lots holding its lines when d's
// actor sees it, else nil.
func visibleShipment(ctx context.Context, exec bentopg.Executor, d authorization.Decision, id string) (*shipment.Shipment, map[string]shipment.Price,
	map[string]string, error) {
	sc := scope(ctx)
	sh, err := postgres.LoadShipment(ctx, exec, sc, id, postgres.NoLock)
	if err != nil || sh == nil || !d.Sees(sh.OwnerActor, sh.OwnerTeam) {
		return nil, nil, nil, err
	}
	prices, err := postgres.LoadPrices(ctx, exec, sc, id)
	if err != nil {
		return nil, nil, nil, err
	}
	taken, err := postgres.TakenLines(ctx, exec, sc, id)
	return sh, prices, taken, err
}

// ResolveShipmentLines answers ShipmentLineRef v1 (R30) lookups of STL in a FUL transaction of its
// own: the lines of the shipments the caller sees (an active grant, FUL's data scope). A line is a
// source of sales settlement when its Code is "".
func (s Service) ResolveShipmentLines(ctx context.Context, keys []api.ShipmentLineKey) (map[api.ShipmentLineKey]api.ShipmentLineRef, error) {
	out := map[api.ShipmentLineKey]api.ShipmentLineRef{}
	if err := api.ValidateShipmentLineKeys(keys); err != nil || len(keys) == 0 {
		return out, err
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		seen := map[string][]api.ShipmentLineRef{}
		for _, k := range keys {
			refs, ok := seen[k.ShipmentID]
			if !ok {
				sh, prices, taken, err := visibleShipment(ctx, exec, d, k.ShipmentID)
				if err != nil {
					return err
				}
				if sh != nil {
					refs = shipmentLineRefs(sh, prices, taken)
				}
				seen[k.ShipmentID] = refs
			}
			for _, r := range refs {
				if r.Key == k {
					out[k] = r
				}
			}
		}
		return nil
	})
	return out, err
}

// ResolveOrderShipmentLines answers ShipmentLineRef v1 for every line of every shipment of each
// order that the caller sees, in shipment ID and line order.
func (s Service) ResolveOrderShipmentLines(ctx context.Context, orderIDs []string) (map[string][]api.ShipmentLineRef, error) {
	out := map[string][]api.ShipmentLineRef{}
	if err := api.ValidateIDs(orderIDs, api.MaxOrderLookups); err != nil || len(orderIDs) == 0 {
		return out, err
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		for _, id := range orderIDs {
			list, _, err := postgres.LoadOrderShipments(ctx, exec, scope(ctx), id)
			if err != nil {
				return err
			}
			refs := []api.ShipmentLineRef{}
			for _, o := range list {
				sh, prices, taken, err := visibleShipment(ctx, exec, d, o.ID)
				if err != nil {
					return err
				}
				if sh != nil {
					refs = append(refs, shipmentLineRefs(sh, prices, taken)...)
				}
			}
			out[id] = refs
		}
		return nil
	})
	return out, err
}

// ResolveExportLots answers ExportLotRef v1 (R36) lookups of CNX in a FUL transaction of its own:
// the lots the caller sees (an active grant, FUL's data scope), each with how it stands against its
// shipment now.
func (s Service) ResolveExportLots(ctx context.Context, ids []string) (map[string]api.ExportLotRef, error) {
	out := map[string]api.ExportLotRef{}
	if err := api.ValidateIDs(ids, api.MaxLotLookups); err != nil || len(ids) == 0 {
		return out, err
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		for _, id := range ids {
			l, err := postgres.LoadLot(ctx, exec, scope(ctx), id)
			if err != nil {
				return err
			}
			if l == nil || !d.Sees(l.OwnerActor, l.OwnerTeam) {
				continue
			}
			sh, err := postgres.LoadShipment(ctx, exec, scope(ctx), l.ShipmentID, postgres.NoLock)
			if err != nil {
				return err
			}
			out[id] = lotRef(l, sh)
		}
		return nil
	})
	return out, err
}

// ResolveShipmentExportLots answers ExportLotRef v1 for the lots of each shipment that the caller
// sees, in lot ID order.
func (s Service) ResolveShipmentExportLots(ctx context.Context, shipmentIDs []string) (map[string][]api.ExportLotRef, error) {
	out := map[string][]api.ExportLotRef{}
	if err := api.ValidateIDs(shipmentIDs, api.MaxLotLookups); err != nil || len(shipmentIDs) == 0 {
		return out, err
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		for _, id := range shipmentIDs {
			refs := []api.ExportLotRef{}
			sh, err := postgres.LoadShipment(ctx, exec, scope(ctx), id, postgres.NoLock)
			if err != nil {
				return err
			}
			if sh != nil {
				lots, err := postgres.LotsOfShipment(ctx, exec, scope(ctx), id)
				if err != nil {
					return err
				}
				for _, l := range lots {
					if d.Sees(l.OwnerActor, l.OwnerTeam) {
						refs = append(refs, lotRef(l, sh))
					}
				}
			}
			out[id] = refs
		}
		return nil
	})
	return out, err
}
