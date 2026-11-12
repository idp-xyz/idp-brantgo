package calc

import (
	"fmt"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Verdicts of the margin rule (T03 §5.2). Rules are kept apart and combine as reject >
// requires_approval > pass: a refusal is an error and no verdict, and a requires_approval verdict
// never clears another refusal.
const (
	MarginPass             = "pass"
	MarginRequiresApproval = "requires_approval"
)

// Margin is the margin rule's verdict on a quote (UL TRD-MarginCheck). Revenue R is the net sales
// and Cost C the adopted snapshot's total, both in the comparison currency. Profit is R − C, Rate
// the margin (R − C) ÷ R and Markup (R − C) ÷ C, both exact, Markup nil when C is 0 (undefined,
// never 0); the margin rate is not the markup. Shortfall is R × Minimum − (R − C) in major units
// of the comparison currency, exactly, when the verdict is requires_approval and 0 otherwise: what
// a margin exception approval binds together with the revision, the business hash and the policy
// revision (S3-14).
type Margin struct {
	Revenue   kernel.Money
	Cost      kernel.Money
	Profit    kernel.Money
	Minimum   kernel.Decimal
	Rate      kernel.Decimal
	Markup    *kernel.Decimal
	Verdict   string
	Shortfall kernel.Decimal
}

// CheckMargin applies TRD-INV-007: the quote passes when R − C ≥ R × minimum_margin, compared
// exactly on the minor-unit amounts and never on a rounded percentage (879.99 against 660.00 is
// 24.9997% and fails a minimum of 0.25 even where a screen shows 25.00%). Below it the verdict is
// requires_approval with the shortfall. Illegal amounts are refusals no exception can override:
// R not positive or C negative (INVALID_KNOWN_VALUE), R and C in different currencies
// (COMPARISON_BASIS_MISMATCH), a minimum outside 0–1 (INVALID_KNOWN_VALUE); path is the quote's.
func CheckMargin(path string, revenue, cost kernel.Money, minimum kernel.Decimal) (Margin, error) {
	switch {
	case revenue.Currency() != cost.Currency():
		return Margin{}, fail(CodeComparisonBasisMismatch, path+"/pricing_policy_ref",
			fmt.Sprintf("The net sales are in %s and the cost in %s; they compare in one currency", revenue.Currency(), cost.Currency()))
	case revenue.Sign() <= 0:
		return Margin{}, fail(kernel.CodeInvalidKnownValue, path+"/lines", "The net sales in the comparison currency are positive")
	case cost.Sign() < 0:
		return Margin{}, fail(kernel.CodeInvalidKnownValue, path+"/cost_snapshot_ref", "A cost is not negative")
	case !fits(minimum, FactorScale) || minimum.Sign() < 0 || minimum.Cmp(one) > 0:
		return Margin{}, fail(kernel.CodeInvalidKnownValue, path+"/pricing_policy_ref", "A minimum margin is between 0 and 1")
	}
	profit, err := revenue.Sub(cost)
	if err != nil {
		return Margin{}, limited(path, "The profit")
	}
	r, p := revenue.Decimal(), profit.Decimal()
	rate, _ := p.Quo(r) // r is positive
	m := Margin{Revenue: revenue, Cost: cost, Profit: profit, Minimum: minimum, Rate: rate, Verdict: MarginPass}
	if !cost.IsZero() {
		markup, _ := p.Quo(cost.Decimal())
		m.Markup = &markup
	}
	if required := r.Mul(minimum); p.Cmp(required) < 0 {
		m.Verdict = MarginRequiresApproval
		m.Shortfall = required.Sub(p)
	}
	return m, nil
}

// Passes reports whether the verdict is pass.
func (m Margin) Passes() bool { return m.Verdict == MarginPass }

// Check returns nil for a passing margin and MARGIN_POLICY_REQUIRES_APPROVAL at path otherwise: the
// answer of a gate that admits a quote below the minimum only with a margin exception approval,
// which an ordinary content approval never is (BC-AT06).
func (m Margin) Check(path string) error {
	if m.Passes() {
		return nil
	}
	return fail(CodeMarginPolicyRequiresApproval, path,
		fmt.Sprintf("The margin %s is below the minimum %s by %s %s; it needs an independent margin exception approval",
			m.Rate, m.Minimum, m.Shortfall, m.Revenue.Currency()))
}
