// Package shipments reads FUL's ShipmentLineRef v1 for CLM (Context Map R34, guardLock false).
// CLM asks before its own transaction. A confirmed shipment line never changes, and CLM writes
// nothing in brant_ful.
package shipments

import (
	"context"

	"go.idp.xyz/idp-brant/internal/contexts/clm/internal/application"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
)

// FUL answers CLM's shipment-line port from FUL's lookup.
type FUL struct {
	Lookups fulapi.ShipmentLineLookups
}

var _ application.ShipmentLines = FUL{}

// ResolveLine asks FUL's ResolveShipmentLines. An unknown shipment or line is absent.
func (f FUL) ResolveLine(ctx context.Context, shipmentID, lineID string) (application.ShipmentLine, bool, error) {
	if f.Lookups == nil {
		return application.ShipmentLine{}, false, nil
	}
	key := fulapi.ShipmentLineKey{ShipmentID: shipmentID, LineID: lineID}
	found, err := f.Lookups.ResolveShipmentLines(ctx, []fulapi.ShipmentLineKey{key})
	if err != nil {
		return application.ShipmentLine{}, false, err
	}
	r, ok := found[key]
	if !ok {
		return application.ShipmentLine{}, false, nil
	}
	line := application.ShipmentLine{Status: r.ShipmentStatus, Version: r.ShipmentVersion, OrderID: r.Order.OrderID,
		OrderRevision: r.Order.OrderRevision, OrderHash: r.Order.OrderHash, CustomerID: r.CustomerID, OrderLineID: r.OrderLineID,
		SkuID: r.SkuID, SpecRevision: r.SpecRevision, UnitID: r.UnitID, UnitRevision: r.UnitRevision, Quantity: r.Quantity, Code: r.Code}
	if p := r.Price; p != nil {
		line.CurrencyCode, line.CurrencyRevision = p.Currency.ID, p.Currency.Revision
	}
	return line, true, nil
}
