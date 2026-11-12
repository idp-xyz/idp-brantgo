package shipment

import "go.idp.xyz/idp-brant/internal/kernel"

// The quantity rules of an order's fulfilment (tactical model FUL-AGG-OrderFulfilment; S5 decision
// D-3 "lock roots, derive balances"). What an order line, a supplier delivery line or a quality
// release has given is never stored: it is the sum of the lines of FUL's confirmed shipments, read
// by a transaction that holds the order's fulfilment root FOR UPDATE. A release and a delivery line
// each serve one order line, so that one lock serializes every confirmation that consumes them.
// Each rule takes what was consumed before, what the shipment ships, and the bound.

// WithinOrderLine refuses shipping more of order line id than it orders, with what the order's
// confirmed shipments shipped of it over every revision: SHIPMENT_QUANTITY_EXCEEDED at path
// (FUL-INV-001; zero tolerance in S5, D-19).
func WithinOrderLine(id string, before, shipping, ordered kernel.Decimal, path string) error {
	if total := before.Add(shipping); total.Cmp(ordered) > 0 {
		return Reject(CodeShipmentQuantityExceeded, path, "Order line "+id+" orders "+ordered.String()+"; "+before.String()+
			" shipped before and "+shipping.String()+" in this shipment exceed it")
	}
	return nil
}

// WithinDelivery refuses shipping more of a supplier delivery line than PUR verified, with what
// FUL's confirmed shipments shipped of it over every version: SUPPLIER_DELIVERY_INSUFFICIENT at
// path (FUL-INV-003; a supplier's claim never counts).
func WithinDelivery(before, shipping, verified kernel.Decimal, path string) error {
	if total := before.Add(shipping); total.Cmp(verified) > 0 {
		return Reject(CodeSupplierDeliveryInsufficient, path, "The supplier delivery line has "+verified.String()+" verified; "+before.String()+
			" shipped before and "+shipping.String()+" in this shipment exceed it")
	}
	return nil
}

// WithinRelease refuses shipping more under a quality release than it released, with what FUL's
// confirmed shipments shipped under it: QUALITY_RELEASE_INSUFFICIENT at path. A revoked release
// keeps what was shipped under it; it serves no further shipment.
func WithinRelease(before, shipping, released kernel.Decimal, path string) error {
	if total := before.Add(shipping); total.Cmp(released) > 0 {
		return Reject(CodeQualityReleaseInsufficient, path, "The quality release released "+released.String()+"; "+before.String()+
			" shipped before and "+shipping.String()+" in this shipment exceed it")
	}
	return nil
}

// CheckOrderLines judges FUL-INV-001 for the whole shipment at its confirmation, against the order
// line quantities of the order revision it binds — the order's current revision, as OrderGuard v1
// proved in the same transaction.
func (s *Shipment) CheckOrderLines(before map[string]kernel.Decimal) error {
	for _, u := range s.OrderLines() {
		if err := WithinOrderLine(u.OrderLineID, before[u.OrderLineID], u.Quantity, u.Ordered, StoredLinePath(u.Line, "quantity")); err != nil {
			return err
		}
	}
	return nil
}
