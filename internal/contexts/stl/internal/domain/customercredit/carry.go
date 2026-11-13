package customercredit

import (
	"fmt"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Why a reservation is released (S5R-D08; SM-STL-CREDIT-RESERVATION): a person or agent abandons a
// reservation of an order that was not activated, or one that expired; the system releases what an
// order held at its end (PL-13) and what a later order revision superseded.
const (
	CauseAbandoned  = "abandoned"
	CauseExpired    = "expired"
	CauseOrderEnded = "order_ended"
	CauseSuperseded = "superseded"
)

// ReleaseCauses returns every release cause.
func ReleaseCauses() []string {
	return []string{CauseAbandoned, CauseExpired, CauseOrderEnded, CauseSuperseded}
}

// Carry is how the system carries reservation From over to an amended order revision (S5R-D08: when
// OrderAmended names no reservation for the new revision): ID and Binding are the new reservation's;
// Portion and Secured are the new revision's credit portion and documents-secured amount in the
// order currency under From's credit policy (Portion); Exposure is Portion in the limit currency at
// From's credit rate (the order currency's amount when there is none); AppliedAt is when the
// amendment took effect, By the system's act now.
type Carry struct {
	ID        string
	From      *Reservation
	Binding   Binding
	Portion   kernel.Money
	Secured   kernel.Money
	Exposure  kernel.Money
	AppliedAt time.Time
	By        Act
}

// CarryOver returns the consumed reservation that carries c.From over to c.Binding (S5R-D08,
// SM-STL-CREDIT-RESERVATION "承接"): From holds credit (reserved or consumed) for an earlier revision of
// the same order; the new revision's portion is no larger than From's, so no limit check applies
// (a larger one needs a reservation of its own before the amendment is applied, D-9); something is
// due on credit or secured by documents. The new reservation keeps From's customer, owner, policy,
// rate and expiry, and is consumed at AppliedAt.
func CarryOver(c Carry) (*Reservation, error) {
	f := c.From
	switch {
	case f.Status != ReservationReserved && f.Status != ReservationConsumed:
		return nil, state("/args/carried_from", "Reservation "+f.ID+" is "+f.Status)
	case f.Binding.OrderID != c.Binding.OrderID:
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/carried_from", "Reservation "+f.ID+" is for order "+f.Binding.OrderID)
	case f.Binding.OrderRevision >= c.Binding.OrderRevision:
		return nil, state("/args/carried_from", fmt.Sprintf("Reservation %s is for order revision %d, not an earlier one", f.ID,
			f.Binding.OrderRevision))
	case c.Portion.Currency() != f.Amount.Currency() || c.Secured.Currency() != f.Amount.Currency() || c.Exposure.Currency() != f.Exposure.Currency():
		return nil, Reject(CodeFXScopeMismatch, "/args/carried_from", "The amended revision's amounts are not in the reservation's currencies")
	case c.Portion.Sign() < 0 || c.Secured.Sign() < 0 || (c.Portion.Sign() == 0 && c.Secured.Sign() == 0):
		return nil, state("/args/order_revision", "The amended revision has nothing due after shipment or invoice: there is nothing to carry over")
	case c.AppliedAt.After(c.By.At):
		return nil, invalid("/args/activated_at", "The amendment took effect in the future")
	}
	if x, _ := c.Portion.Cmp(f.Amount); x > 0 {
		return nil, state("/args/carried_from", fmt.Sprintf("The amended revision's credit portion %s exceeds the %s reservation %s holds: "+
			"a larger portion is reserved before the amendment is applied (D-9)", c.Portion, f.Amount, f.ID))
	}
	exposure := c.Exposure
	if f.FX == nil {
		exposure = c.Portion
	}
	if x, _ := exposure.Cmp(f.Exposure); x > 0 {
		exposure = f.Exposure // rounding never carries more than was held
	}
	if c.Portion.Sign() == 0 {
		exposure, _ = kernel.NewMoney(f.Exposure.Currency(), 0)
	}
	by := c.By
	return &Reservation{ID: c.ID, PartyID: f.PartyID, Binding: c.Binding, Amount: c.Portion, Secured: c.Secured, Exposure: exposure, FX: f.FX,
		Basis: BasisCarriedOver, Policy: f.Policy, Status: ReservationConsumed, Version: 1, ExpiresAt: f.ExpiresAt, OwnerActor: f.OwnerActor,
		OwnerTeam: f.OwnerTeam, Reserved: by, Consumed: &by, ActivatedAt: kernel.Instant(c.AppliedAt), CarriedFrom: f.ID}, nil
}
