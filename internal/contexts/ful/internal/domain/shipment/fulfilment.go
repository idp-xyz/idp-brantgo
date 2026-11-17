package shipment

import (
	"maps"

	"go.idp.xyz/idp-brant/internal/kernel"
)

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

// WithinCommitmentLine refuses shipping more of purchase commitment line ref than may ship of it:
// what PUR verified of it over all its deliveries less what its approved dispositions took out of the
// order, with what FUL's confirmed shipments shipped of it over every delivery and version:
// SUPPLIER_DELIVERY_INSUFFICIENT at path (FUL-INV-003, S5R-D05: disposed goods never ship).
func WithinCommitmentLine(ref CommitmentLine, before, shipping kernel.Decimal, t CommitmentLineTotals, path string) error {
	may := t.Verified.Sub(t.Disposed)
	if total := before.Add(shipping); total.Cmp(may) > 0 {
		return Reject(CodeSupplierDeliveryInsufficient, path, "Purchase commitment "+ref.CommitmentID+" line "+ref.OrderLineID+" has "+
			t.Verified.String()+" verified and "+t.Disposed.String()+" disposed of by approved dispositions (S5R-D05); "+before.String()+
			" shipped before and "+shipping.String()+" in this shipment exceed the "+may.String()+" that may ship")
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

// Before returns what was consumed before this shipment: c itself for a shipment not yet
// confirmed, and c less the shipment's own lines for a confirmed one (a document release re-judges
// the quantities its shipment already consumed).
func (s *Shipment) Before(c Consumed) Consumed {
	if s.Status != StatusConfirmed {
		return c
	}
	out := Consumed{OrderLines: maps.Clone(c.OrderLines), Deliveries: maps.Clone(c.Deliveries), CommitmentLines: maps.Clone(c.CommitmentLines),
		Releases: maps.Clone(c.Releases)}
	if out.OrderLines == nil {
		out.OrderLines = map[string]kernel.Decimal{}
	}
	if out.Deliveries == nil {
		out.Deliveries = map[DeliveryRef]kernel.Decimal{}
	}
	if out.CommitmentLines == nil {
		out.CommitmentLines = map[CommitmentLine]kernel.Decimal{}
	}
	if out.Releases == nil {
		out.Releases = map[string]kernel.Decimal{}
	}
	for _, l := range s.Lines {
		cl := CommitmentLine{CommitmentID: l.CommitmentID, OrderLineID: l.OrderLineID}
		out.OrderLines[l.OrderLineID] = out.OrderLines[l.OrderLineID].Sub(l.Quantity)
		out.Deliveries[l.Delivery] = out.Deliveries[l.Delivery].Sub(l.Quantity)
		out.CommitmentLines[cl] = out.CommitmentLines[cl].Sub(l.Quantity)
		out.Releases[l.ReleaseID] = out.Releases[l.ReleaseID].Sub(l.Quantity)
	}
	return out
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
