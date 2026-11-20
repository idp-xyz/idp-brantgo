package customercredit

import (
	"slices"
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

// Rates are the exchange rates of purpose credit (POL fx_rate, purpose credit) the exposure converts
// receivables in another currency than the limit's with, keyed by the currency code each converts
// from (S5R-D22, STL-INV-004: "敞口中不同币种的应收（含期初）按 POL fx_rate 中用途为 credit 的汇率折算").
// Every rate converts into the limit currency.
type Rates map[string]kernel.ExchangeRate

// RatesPath is where a missing credit rate is refused: credit.reserve names the rates for the
// exposure in exposure_fx_rates (the order's own fx_rate converts receivables in its currency too).
const RatesPath = "/args/exposure_fx_rates"

// converter converts amounts into the limit currency c at the credit rate of their currency,
// HALF_UP to c's minor units once (S5R-D22; it replaces S5 B2b's conversion at the rate of the
// order's reservation, which an opening receivable does not have). An amount already in c is kept;
// zero converts to zero; an amount in another currency without a rate is FX_SCOPE_MISMATCH (the
// exposure fails closed).
type converter struct {
	c     kernel.Currency
	rates Rates
}

func (cv converter) convert(order string, m kernel.Money) (kernel.Money, error) {
	if m.Currency() == cv.c {
		return m, nil
	}
	if m.Sign() == 0 {
		return kernel.NewMoney(cv.c, 0)
	}
	x, ok := cv.rates[m.Currency().Code()]
	if !ok || x.From() != m.Currency() || x.To() != cv.c {
		what := "The receivables of order " + order
		if order == "" {
			what = "An opening receivable"
		}
		return kernel.Money{}, Reject(CodeFXScopeMismatch, RatesPath, what+" is in "+m.Currency().Code()+", the limit in "+cv.c.Code()+
			": name an fx_rate of purpose credit from "+m.Currency().Code()+" into "+cv.c.Code())
	}
	return x.Convert(m)
}

// Unconverted returns, in code order, the currencies of owed other than c that have an amount the
// exposure counts (an open amount, or a credit stage's amount) and no rate in rates converts: the
// exposure cannot be stated without them.
func Unconverted(c kernel.Currency, owed []OwedInstallment, rates Rates) []string {
	var out []string
	for _, o := range owed {
		cur := o.Amount.Currency()
		open, err := o.Amount.Sub(o.Applied)
		if cur == c || (err == nil && open.Sign() <= 0 && (!credit(o.Trigger) || o.Amount.Sign() == 0)) {
			continue
		}
		if x, ok := rates[cur.Code()]; ok && x.From() == cur && x.To() == c {
			continue
		}
		if !slices.Contains(out, cur.Code()) {
			out = append(out, cur.Code())
		}
	}
	slices.Sort(out)
	return out
}

// ReceivablesOf is what a customer owes on owed, its business receivables' installments, as the
// exposure of D-4 counts it in the limit currency c: the open amounts, per order what was formed on
// its credit stages (FormedBy; FormedDocumentsBy for document-controlled ones), and the due date of
// the earliest installment still open. Amounts in another currency than c — an order's receivables
// in its currency or an opening receivable in its own — convert at the credit rate of their currency
// in rates (S5R-D22), else FX_SCOPE_MISMATCH.
func ReceivablesOf(c kernel.Currency, owed []OwedInstallment, rates Rates) (Receivables, error) {
	out := Receivables{FormedBy: map[string]kernel.Money{}, FormedDocumentsBy: map[string]kernel.Money{}}
	var err error
	if out.Open, err = kernel.NewMoney(c, 0); err != nil {
		return Receivables{}, err
	}
	cv := converter{c: c, rates: rates}
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
