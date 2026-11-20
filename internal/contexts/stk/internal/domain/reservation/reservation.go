// Package reservation is the reservation aggregate (STK-AGG-Reservation, SM-STK-RESERVATION,
// STK-INV-001). A reservation is not an issue. Release is not an issue. Consumption of an order
// reservation happens in StockAvailability v1, not here.
package reservation

import "go.idp.xyz/idp-brant/internal/kernel"

// States (SM-STK-RESERVATION).
const (
	StatusActive   = "active"
	StatusReleased = "released"
	StatusConsumed = "consumed"
)

// Reservation is one hold on a balance. OrderID empty means an internal hold that stock.issue may
// consume. An order hold is consumed only by StockAvailability v1.
type Reservation struct {
	ID           string
	SKU          string
	Location     string
	Batch        string
	Custody      string
	OrderID      string
	OrderLineID  string
	Quantity     kernel.Decimal
	Remaining    kernel.Decimal
	Status       string
	UnitID       string
	UnitRevision int64
	RowVersion   int64
	OwnerActor   string
	OwnerTeam    string
}

// OrderHeld reports whether r was placed for a sales order line.
func (r Reservation) OrderHeld() bool { return r.OrderID != "" }

// Releasable reports whether r can be released. A consumed or already released hold cannot
// (STOCK_RESERVATION_CONFLICT, SM-STK-RESERVATION).
func (r Reservation) Releasable() bool {
	return r.Status == StatusActive && r.Remaining.Sign() > 0
}

// Release marks r released and returns the quantity to give back to the balance.
func (r *Reservation) Release() (kernel.Decimal, bool) {
	if !r.Releasable() {
		return kernel.Decimal{}, false
	}
	back := r.Remaining
	r.Remaining = kernel.Decimal{}
	r.Status = StatusReleased
	r.RowVersion++
	return back, true
}

// Consume takes qty from an active hold. The hold stays active while something remains and becomes
// consumed at zero.
func (r *Reservation) Consume(qty kernel.Decimal) bool {
	if r.Status != StatusActive || qty.Sign() <= 0 || r.Remaining.Cmp(qty) < 0 {
		return false
	}
	r.Remaining = r.Remaining.Sub(qty)
	if r.Remaining.Sign() == 0 {
		r.Status = StatusConsumed
	}
	r.RowVersion++
	return true
}
