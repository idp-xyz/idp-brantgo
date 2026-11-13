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
// The adapter on fulapi (adapters/upstream FUL, S5 C3a) fills Export from the shipment or the export
// lot: state, row version, order binding, lines with SKU specification revision, quantity, unit and
// amount, value, and the end user and destination from the parties the shipment binds. FUL records
// no export date: a lot's export date is the one customs states on its declaration, CNX's own
// fact (exportOfLot).
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

// The FUL states an allocation needs: a prepared shipment, a registered export lot; a tax refund
// allocation refuses a cancelled export lot.
const (
	ShipmentPrepared    = "prepared"
	ExportLotRegistered = "registered"
	ExportLotCancelled  = "cancelled"
)

// Export is what FUL says of a shipment or export lot: its state and version, the order revision
// (and hash) it ships, its destination country, the end user party, its lines and its value.
type Export struct {
	Ref            ExportRef
	Status         string
	Version        int64
	OrderID        string
	OrderRevision  int64
	OrderHash      string
	Destination    string
	EndUserPartyID string
	Lines          []ExportLine
	Value          kernel.Money
	Code           string
}

// ExportLine is a shipment or export lot line: its SKU specification revision, quantity and unit,
// and for an export lot line (S5 C3b, tax refund allocations) the shipment and order line it comes
// from and its amount in the order currency.
type ExportLine struct {
	LineID       string
	SkuID        string
	SpecRevision int64
	Quantity     kernel.Decimal
	Unit         kernel.UnitCode
	ShipmentID   string
	OrderLineID  string
	Amount       kernel.Money
	// ShipmentLineID is an export lot line's FUL shipment line (S5 C10: a credit note's statement names
	// shipment lines; CN-AT11 ties it to the lot by them).
	ShipmentLineID string
}
