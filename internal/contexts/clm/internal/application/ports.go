package application

import (
	"context"
	"time"
)

// ShipmentLine is a FUL shipment line as CLM reads it before its own transaction (Context Map R34,
// ShipmentLineRef v1, guardLock false). Code is empty when the line is a confirmed line with a
// price. Otherwise it is FUL's code: INVALID_STATE when the shipment is not confirmed,
// REFERENCE_INVALID when it has no price. A confirmed line never changes, so CLM keeps the
// snapshot and takes no lock on the shipment.
type ShipmentLine struct {
	Status           string
	Version          int64
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	CustomerID       string
	OrderLineID      string
	SkuID            string
	SpecRevision     int64
	UnitID           string
	UnitRevision     int64
	Quantity         string
	CurrencyCode     string
	CurrencyRevision int64
	Code             string
}

// ShipmentLines is CLM's port to FUL. Absent means the shipment or the line is unknown to the
// caller. Ask before opening the claim transaction.
type ShipmentLines interface {
	ResolveLine(ctx context.Context, shipmentID, lineID string) (ShipmentLine, bool, error)
}

// Evidence is CLM's port to EVD for sources a command cites. The map holds the content hash of
// each source the caller may read; an id that is absent is not readable. A command that cites no
// source does not ask. There is no Context Map edge from EVD to CLM, so the adapter lives in
// bootstrap and this port does not name EVD's types.
type Evidence interface {
	Resolve(ctx context.Context, ids []string) (map[string]string, error)
}

// Currencies is CLM's port to the currency definition the shipment line names (its POL revision).
// Minor units never change once published. The adapter lives in bootstrap: POL is not an upstream
// CLM may import.
type Currencies interface {
	MinorUnits(ctx context.Context, code string, revision int64, at time.Time) (int, error)
}
