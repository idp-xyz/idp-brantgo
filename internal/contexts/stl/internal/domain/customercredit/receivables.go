package customercredit

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// HeldOf is reservation r as the exposure counts it.
func HeldOf(r *Reservation) Held {
	return Held{OrderID: r.Binding.OrderID, OrderRevision: r.Binding.OrderRevision, Exposure: r.Exposure,
		DocumentsSecured: !r.Secured.Currency().IsZero() && r.Secured.Sign() > 0}
}

// OwedInstallment is one installment of a customer's business receivable (settlement statements,
// S5-04b) as the exposure counts it: its order, the trigger and release control of the payment stage
// it is due for ("" beyond the plan), its amount and what stands applied on it in the order
// currency, and when it falls due.
type OwedInstallment struct {
	OrderID        string
	Trigger        string
	ReleaseControl string
	Amount         kernel.Money
	Applied        kernel.Money
	DueAt          time.Time
}

// credit reports whether an installment of trigger replaces a reservation: the stages due after
// shipment or invoice are the credit portion (TRD-INV-022).
func credit(trigger string) bool {
	return trigger == TriggerAfterShipment || trigger == TriggerAfterInvoice
}

// converter converts an order's amounts into the limit currency c at the rate its reservation was
// made at: the reservation's portion and its exposure in the limit currency (the latest revision's
// reservation of the order that has a portion), HALF_UP to c's minor units. An amount already in c
// is kept; zero converts to zero; an amount in another currency without such a reservation is
// FX_SCOPE_MISMATCH (the exposure fails closed).
type converter struct {
	c     kernel.Currency
	rates map[string]*Reservation
}

func newConverter(c kernel.Currency, reservations []Reservation) converter {
	cv := converter{c: c, rates: map[string]*Reservation{}}
	for i := range reservations {
		r := &reservations[i]
		if r.Amount.Currency().IsZero() || r.Amount.Sign() <= 0 || r.Exposure.Currency() != c {
			continue
		}
		if cur, ok := cv.rates[r.Binding.OrderID]; !ok || r.Binding.OrderRevision > cur.Binding.OrderRevision {
			cv.rates[r.Binding.OrderID] = r
		}
	}
	return cv
}

func (cv converter) convert(order string, m kernel.Money) (kernel.Money, error) {
	if m.Currency() == cv.c {
		return m, nil
	}
	if m.Sign() == 0 {
		return kernel.NewMoney(cv.c, 0)
	}
	r, ok := cv.rates[order]
	if !ok || r.Amount.Currency() != m.Currency() {
		return kernel.Money{}, Reject(CodeFXScopeMismatch, "/args/credit_policy",
			"The receivables of order "+order+" are in "+m.Currency().Code()+", the limit in "+cv.c.Code()+", and no credit rate of the order converts them")
	}
	rate, err := r.Exposure.Decimal().Quo(r.Amount.Decimal())
	if err != nil {
		return kernel.Money{}, err
	}
	return kernel.RoundMoney(cv.c, m.Decimal().Mul(rate))
}

// ReceivablesOf is what a customer owes on owed, its business receivables' installments, as the
// exposure of D-4 counts it in the limit currency c: the open amounts, per order what was formed on
// its credit stages (FormedBy; FormedDocumentsBy for document-controlled ones), and the due date of
// the earliest installment still open. Amounts in another currency than c convert at the order's
// reservation rate (converter).
func ReceivablesOf(c kernel.Currency, owed []OwedInstallment, reservations []Reservation) (Receivables, error) {
	out := Receivables{FormedBy: map[string]kernel.Money{}, FormedDocumentsBy: map[string]kernel.Money{}}
	var err error
	if out.Open, err = kernel.NewMoney(c, 0); err != nil {
		return Receivables{}, err
	}
	cv := newConverter(c, reservations)
	add := func(into map[string]kernel.Money, order string, m kernel.Money) error {
		x, err := cv.convert(order, m)
		if err != nil {
			return err
		}
		if cur, ok := into[order]; ok {
			x, err = cur.Add(x)
		}
		into[order] = x
		return err
	}
	for _, o := range owed {
		open, err := o.Amount.Sub(o.Applied)
		if err != nil {
			return Receivables{}, err
		}
		if open.Sign() > 0 {
			x, err := cv.convert(o.OrderID, open)
			if err != nil {
				return Receivables{}, err
			}
			if out.Open, err = out.Open.Add(x); err != nil {
				return Receivables{}, err
			}
			if out.EarliestDue.IsZero() || o.DueAt.Before(out.EarliestDue) {
				out.EarliestDue = o.DueAt
			}
		}
		if !credit(o.Trigger) {
			continue
		}
		into := out.FormedBy
		if o.ReleaseControl == ReleaseDocuments {
			into = out.FormedDocumentsBy
		}
		if err = add(into, o.OrderID, o.Amount); err != nil {
			return Receivables{}, err
		}
	}
	return out, nil
}
