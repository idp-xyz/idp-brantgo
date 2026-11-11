package calc

import (
	"slices"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Calculation is the server's calculation of a quote revision, which the freeze stores and hashes
// with it and nothing else replaces: the scope and sales terms bindings, the amounts per line and
// charge and their totals in the quote currency, the net sales and the cost in the comparison
// currency, the margin verdict and the payment plan, on nominal direct cost with the cost
// categories it does not estimate named rather than taken as 0. Lines and charges keep the quote's
// order; payments keep the stages'.
type Calculation struct {
	Scope                  ScopeBinding
	Terms                  TermsBinding
	Currency               kernel.Currency
	Lines                  []Amounts
	Charges                []ChargeAmounts
	Totals                 Totals
	Comparison             Comparison
	Cost                   kernel.Money
	Margin                 Margin
	Payments               []Payment
	CostBasis              string
	ExcludedCostCategories []string
}

// Calculate calculates quote q on facts f (T03 §5, in the order of the package documentation).
// It returns the first refusal: a scope, terms, amount, exchange rate, cost or payment failure is
// an error, and so never a margin verdict. A margin below the minimum is no error: the Calculation
// carries the requires_approval verdict, which Margin.Check turns into
// MARGIN_POLICY_REQUIRES_APPROVAL where a gate needs a margin exception approval.
func Calculate(q Quote, f Facts) (Calculation, error) {
	if q.Currency.IsZero() {
		return Calculation{}, fail(kernel.CodeDraftNotReady, q.Path+"/currency_ref", "The quote names no currency")
	}
	scope, err := BindScope(q, f)
	if err != nil {
		return Calculation{}, err
	}
	terms, err := BindTerms(q)
	if err != nil {
		return Calculation{}, err
	}
	lines := make([]Amounts, len(q.Lines))
	for i, l := range q.Lines {
		if lines[i], err = PriceLine(q.Currency, l); err != nil {
			return Calculation{}, err
		}
	}
	charges := make([]ChargeAmounts, len(q.Charges))
	for i, c := range q.Charges {
		if charges[i], err = PriceCharge(q.Currency, c); err != nil {
			return Calculation{}, err
		}
	}
	totals, err := Total(q.Currency, q.Path, lines, charges)
	if err != nil {
		return Calculation{}, err
	}
	comparison, err := Compare(q.Path, totals.Net, f.Policy.CalculationCurrency, f.FX, f.At, f.Policy.MaxFXAge)
	if err != nil {
		return Calculation{}, err
	}
	margin, err := CheckMargin(q.Path, comparison.Revenue, f.Cost.Total, f.Policy.MinimumMargin)
	if err != nil {
		return Calculation{}, err
	}
	payments, err := AllocatePayments(q.Path, totals.Payable, f.Stages)
	if err != nil {
		return Calculation{}, err
	}
	return Calculation{
		Scope: scope, Terms: terms, Currency: q.Currency, Lines: lines, Charges: charges, Totals: totals,
		Comparison: comparison, Cost: f.Cost.Total, Margin: margin, Payments: payments,
		CostBasis: f.Cost.CostBasis, ExcludedCostCategories: slices.Clone(f.Cost.ExcludedCostCategories),
	}, nil
}
