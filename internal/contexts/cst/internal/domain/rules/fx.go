package rules

import (
	"fmt"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Purposes of an exchange rate; only cost_estimate rates price cost (CST-INV-006).
const (
	PurposeCostEstimate = "cost_estimate"
	PurposeQuotation    = "quotation"
)

// FXRate is an fx_rate revision a scenario names: one From is Rate To, for Purpose, observed at
// ObservedAt and in force over Validity. Ref is the revision's identity, kept in the result.
type FXRate struct {
	Path       string
	Ref        string
	From, To   kernel.Currency
	Rate       kernel.Decimal
	Purpose    string
	ObservedAt time.Time
	Validity   kernel.ValidityInterval
}

// FX is how one evaluation converts into its calculation currency: the rates the scenario names
// (fx_refs, at Path), the calculation instant and the pricing policy's maximum observation age.
type FX struct {
	Path   string
	Target kernel.Currency
	Rates  []FXRate
	At     time.Time
	MaxAge time.Duration
}

// Convert applies CST-INV-006 to amount m, a cost in its original currency, and returns it in the
// calculation currency with the Ref of the rate used. The same currency is an explicit 1 and takes
// no rate. Different currencies take exactly one named rate of the direction m → Target and
// purpose cost_estimate, in force at At and observed no longer than MaxAge before it; the product
// is rounded HALF_UP to the target's minor units once (100 USD × 7.123456 = 712.35 CNY). A missing
// rate is never 1, a rate of the opposite direction is never inverted and a quotation rate never
// prices cost: each is FX_NOT_APPLICABLE, as is a stale or out-of-range rate.
func (x FX) Convert(m kernel.Money) (kernel.Money, string, error) {
	from := m.Currency()
	if from == x.Target {
		return m, "", nil
	}
	if from.Code() == x.Target.Code() {
		return kernel.Money{}, "", fail(kernel.CodeReferenceInvalid, x.Path, fmt.Sprintf("Two definitions of %s disagree on minor units", from))
	}
	var found []FXRate
	reverse, otherPurpose := false, ""
	for _, r := range x.Rates {
		switch {
		case r.From.Code() == from.Code() && r.To.Code() == x.Target.Code() && r.Purpose == PurposeCostEstimate:
			found = append(found, r)
		case r.From.Code() == from.Code() && r.To.Code() == x.Target.Code():
			otherPurpose = r.Purpose
		case r.From.Code() == x.Target.Code() && r.To.Code() == from.Code():
			reverse = true
		}
	}
	switch {
	case len(found) > 1:
		return kernel.Money{}, "", fail(CodeFXNotApplicable, x.Path, fmt.Sprintf("Several cost_estimate rates convert %s into %s; the scenario names one", from, x.Target))
	case len(found) == 1:
	case otherPurpose != "":
		return kernel.Money{}, "", fail(CodeFXNotApplicable, x.Path, fmt.Sprintf("A %s rate does not price cost; cost needs a cost_estimate rate", otherPurpose))
	case reverse:
		return kernel.Money{}, "", fail(CodeFXNotApplicable, x.Path, fmt.Sprintf("Only a %s→%s rate is named; a rate is never inverted", x.Target, from))
	default:
		return kernel.Money{}, "", fail(CodeFXNotApplicable, x.Path, fmt.Sprintf("No cost_estimate rate converts %s into %s; a missing rate is never 1", from, x.Target))
	}
	r := found[0]
	if x.MaxAge <= 0 {
		return kernel.Money{}, "", fail(CodeCostInputNotReady, x.Path, "The pricing policy sets no maximum exchange rate age")
	}
	at := kernel.Instant(x.At)
	observed := kernel.Instant(r.ObservedAt)
	switch {
	case !r.Validity.Valid() || !r.Validity.Contains(at):
		return kernel.Money{}, "", fail(CodeFXNotApplicable, r.Path, "The exchange rate is not in force at the calculation time")
	case r.ObservedAt.IsZero() || observed.After(at):
		return kernel.Money{}, "", fail(CodeFXNotApplicable, r.Path, "The exchange rate was observed after the calculation time")
	case at.Sub(observed) > x.MaxAge:
		return kernel.Money{}, "", fail(CodeFXNotApplicable, r.Path, fmt.Sprintf("The exchange rate was observed more than %s before the calculation", x.MaxAge))
	case !fits(r.Rate, FactorScale) || r.Rate.Sign() <= 0 || r.Rate.Cmp(maxFXRate) > 0:
		return kernel.Money{}, "", fail(CodeFXNotApplicable, r.Path, "An exchange rate is positive, at most 1000, with at most 12 fractional digits")
	case r.From != from || r.To != x.Target:
		return kernel.Money{}, "", fail(kernel.CodeReferenceInvalid, r.Path, "The rate's currency definitions disagree on minor units")
	}
	rate, err := kernel.NewExchangeRate(r.From, r.To, r.Rate)
	if err != nil {
		return kernel.Money{}, "", fail(CodeFXNotApplicable, r.Path, "")
	}
	out, err := rate.Convert(m)
	if err != nil {
		return kernel.Money{}, "", fail(CodeCalculationLimitExceeded, r.Path, "The converted amount is out of range")
	}
	return out, r.Ref, nil
}

// ComponentCost is one purchase component or charge as costed: its tax split and cost in the
// original currency, the cost in the calculation currency, and the rate used ("" for the same
// currency).
type ComponentCost struct {
	Tax    TaxedCost
	Amount kernel.Money
	FXRate string
}

// Cost costs one component (T02 §13.3 steps 2–4): the exact amount raw of currency c is split by
// the tax treatment and rounded to c, its cost converted into the calculation currency and rounded
// there, and the result checked against the business bound of 10^12 (at path, the component's).
// Components are costed one by one: two lines of 0.005 USD are 0.01 each, never merged to reduce
// rounding.
func Cost(path string, raw kernel.Decimal, c kernel.Currency, tax TaxTreatment, fx FX) (ComponentCost, error) {
	taxed, err := tax.Cost(c, raw)
	if err != nil {
		return ComponentCost{}, err
	}
	amount, ref, err := fx.Convert(taxed.Cost)
	if err != nil {
		return ComponentCost{}, err
	}
	if err := checkMoney(amount, path); err != nil {
		return ComponentCost{}, err
	}
	return ComponentCost{Tax: taxed, Amount: amount, FXRate: ref}, nil
}
