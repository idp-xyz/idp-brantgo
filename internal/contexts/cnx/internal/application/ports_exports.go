package application

import (
	"context"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Exports is CNX's port to FUL for export licence allocations (Context Map R36 FUL→CNX, ExportLotRef
// v1, and FUL's shipments, which R36's "出口批次与出运单证明细" carries; no guard lock): what FUL says
// now of each shipment or export lot an allocation is asked for. ResolveExports answers every ref
// FUL resolves for the caller (data scope applied: one the caller does not see is absent); Code is
// REFERENCE_INVALID when FUL's record of it is not intact.
//
// FUL is not in S5 B5's tree (B1a builds shipments, B1b export lots): bootstrap wires ExportsNotWired,
// which answers DEPENDENCY_NOT_SUPPORTED, and tests implement the port. Once FUL is merged an adapter
// on fulapi fills Export from the shipment (B1a: status, row version, order binding, consignee
// address country as Destination, the end user party, its lines with SKU specification revision,
// quantity and unit, and its value) or the export lot (B1b: the same from the lot and its lines).
//
// R36 carries no guard: an allocation binds what it read (the target's version and the hash of its
// lines). A change of the shipment's lines afterwards shows at FUL's release, where
// ExportLicenseGuard v1's Check compares the lines FUL passes with the allocation's.
type Exports interface {
	ResolveExports(ctx context.Context, refs []ExportRef) (map[ExportRef]Export, error)
}

// ExportRef is a FUL shipment or export lot.
type ExportRef struct {
	Kind string // shipment | export_lot
	ID   string
}

// The FUL states an allocation needs: a prepared shipment, a registered export lot.
const (
	ShipmentPrepared    = "prepared"
	ExportLotRegistered = "registered"
)

// Export is what FUL says of a shipment or export lot: its state and version, the order revision it
// ships, its destination country, the end user party, its lines and its value.
type Export struct {
	Ref            ExportRef
	Status         string
	Version        int64
	OrderID        string
	OrderRevision  int64
	Destination    string
	EndUserPartyID string
	Lines          []ExportLine
	Value          kernel.Money
	Code           string
}

// ExportLine is a shipment or export lot line: its SKU specification revision, quantity and unit.
type ExportLine struct {
	LineID       string
	SkuID        string
	SpecRevision int64
	Quantity     kernel.Decimal
	Unit         kernel.UnitCode
}
