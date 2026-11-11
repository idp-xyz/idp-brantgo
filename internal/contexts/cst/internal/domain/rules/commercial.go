package rules

import (
	"cmp"
	"slices"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// CostBasisNominalDirect is the only cost basis V1.0 computes: confirmed purchase prices and the
// direct charges the exporter bears (T02 §16.1).
const CostBasisNominalDirect = "nominal_direct_cost"

// ExcludedCostCategories are what nominal direct cost does not estimate; a result names them
// rather than reporting them as zero.
var ExcludedCostCategories = []string{"imputed_financing_cost", "expected_credit_loss", "unrealized_fx_hedge"}

// Cash flow comparisons of a supplier's payment terms with the customer's.
const (
	CashflowMatched   = "matched"
	CashflowDifferent = "different_cashflow"
)

// PaymentStage is one stage of a payment terms revision; Calendar is the identity of its calendar
// revision.
type PaymentStage struct {
	Percent  kernel.Decimal
	Trigger  string
	Days     int
	DayBasis string
	Calendar string
}

// CompareCashflow compares supplier payment stages with the customer's (T02 §16.1). Different
// schedules are recorded as different_cashflow and stay comparable on nominal direct cost: no
// financing cost is imputed, and the cost is unchanged.
func CompareCashflow(sale, supplier []PaymentStage) string {
	key := func(stages []PaymentStage) []PaymentStage {
		out := slices.Clone(stages)
		slices.SortFunc(out, func(a, b PaymentStage) int {
			return cmp.Or(a.Percent.Cmp(b.Percent), cmp.Compare(a.Trigger, b.Trigger), cmp.Compare(a.Days, b.Days),
				cmp.Compare(a.DayBasis, b.DayBasis), cmp.Compare(a.Calendar, b.Calendar))
		})
		return out
	}
	a, b := key(sale), key(supplier)
	same := slices.EqualFunc(a, b, func(x, y PaymentStage) bool {
		return x.Percent.Equal(y.Percent) && x.Trigger == y.Trigger && x.Days == y.Days && x.DayBasis == y.DayBasis && x.Calendar == y.Calendar
	})
	if same {
		return CashflowMatched
	}
	return CashflowDifferent
}
