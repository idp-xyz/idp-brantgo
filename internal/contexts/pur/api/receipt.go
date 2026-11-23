package api

import (
	"context"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// ReceiptRequest v1 (Context Map R28 PUR→STK, guardLock false: 收货与外厂我方物料). STK reads it
// before its own transaction. It is not a supplier delivery: supplier_delivery.verify stays the
// direct-ship fact and never becomes a warehouse receipt (STK-INV-002, UL PUR-SupplierDelivery).
// An approved hold_for_reuse may be cited as a receipt source once STK is enabled (S6-D02).
//
// ResolveReceiptRequest returns NOT_FOUND when the commitment line is not visible to the caller.
// Code is INVALID_STATE when the line exists but the commitment is not issued or confirmed.
// DirectShip is true when a verified supplier delivery already covers the line. UnitCostMinor is
// the line's net per PriceBasisQuantity, one HALF_UP rounding, for the FIFO layer STK may open.
// There is no new PUR migration: the figures are the commitment's stored draft.

// ReceiptRequest is one commitment line as a warehouse or custody receipt source.
type ReceiptRequest struct {
	CommitmentID         string
	OrderLineID          string
	OrderID              string
	Status               string
	RowVersion           int64
	SupplierID           string
	PONumber             string
	SkuID                string
	SpecRevision         int64
	UnitID               string
	UnitRevision         int64
	OrderedQuantity      string
	AllocatedQuantity    string
	Currency             string
	MinorUnits           int
	NetMinor             string
	PriceBasisQuantity   string
	UnitCostMinor        string
	HoldForReuse         bool
	HoldForReuseQuantity string
	DirectShip           bool
	Code                 string
}

// ReceiptRequests answer ReceiptRequest v1 lookups of STK (R28). Ask before opening a stock
// transaction. A line outside the caller's data scope is NOT_FOUND.
type ReceiptRequests interface {
	ResolveReceiptRequest(ctx context.Context, commitmentID, orderLineID string) (ReceiptRequest, error)
}

// NetUnitCost is the minor-unit cost of one price-basis quantity: netMinor × basis / quantity,
// rounded once HALF_UP. It is the FIFO reference on ReceiptRequest.UnitCostMinor.
func NetUnitCost(currency string, minorUnits int, netMinor, basis, quantity string) (string, error) {
	cur, err := kernel.NewCurrency(currency, minorUnits)
	if err != nil {
		return "", err
	}
	net, err := kernel.ParseMinor(cur, netMinor)
	if err != nil || net.Sign() < 0 {
		return "", kernel.ErrInvalidAmount
	}
	q, err := kernel.ParseDecimal(quantity, kernel.QuantityFormat)
	if err != nil || q.Sign() <= 0 {
		return "", kernel.ErrInvalidQuantity
	}
	b := kernel.DecimalFromInt(1)
	if basis != "" {
		b, err = kernel.ParseDecimal(basis, kernel.QuantityFormat)
		if err != nil || b.Sign() <= 0 {
			return "", kernel.ErrInvalidQuantity
		}
	}
	factor, err := b.Quo(q)
	if err != nil {
		return "", err
	}
	unit, err := net.MulHalfUp(factor)
	if err != nil {
		return "", err
	}
	return unit.MinorString(), nil
}
