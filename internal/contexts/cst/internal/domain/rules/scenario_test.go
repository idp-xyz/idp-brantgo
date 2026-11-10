package rules

import (
	"fmt"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// scenario is the input of one evaluation as the application layer would resolve it; evaluate
// composes the rules in the order of the package documentation. It is the reference for the
// CostScenario evaluation built on this package.
type scenario struct {
	demand      []DemandLine
	allocations []Allocation
	conditions  []Condition
	goodsTax    TaxTreatment
	goodsIn     kernel.Currency
	fees        []Fee
	feeTax      TaxTreatment
	feeIn       kernel.Currency
	measurement *Measurement
	sale        []Obligation
	required    []RequiredFee
	fx          FX
}

type evaluation struct {
	total      kernel.Money
	components map[string]kernel.Money
	shares     map[string][]Share
	plan       ChargePlan
}

func zeroTax() TaxTreatment { // the fixture's tax.zero: 0, exclusive, nonrecoverable
	return TaxTreatment{Path: "/tax_profile/tax.zero", Rate: dec("0"), PriceBasis: TaxExclusive, Recovery: Nonrecoverable}
}

// pythonFixture is the Python costing fixture (tests/costing_fixture.py with tests/fixtures/
// costing_inputs.json and costing_registry.json): 120 PCS of sku.1 from offer.1 at 5 USD, a 60 USD
// freight borne by the seller and required by the comparison basis, calculation currency USD.
func pythonFixture(t *testing.T) scenario {
	return scenario{
		demand: baseDemand(t), allocations: []Allocation{baseAllocation(t, "120")}, conditions: []Condition{baseCondition(t)},
		goodsTax: zeroTax(), goodsIn: usd, fees: []Fee{fixtureFee()}, feeTax: zeroTax(), feeIn: usd,
		sale: saleFreight, required: requiredFreight, fx: toUSD(),
	}
}

func evaluate(s scenario) (evaluation, error) {
	out := evaluation{components: map[string]kernel.Money{}, shares: map[string][]Share{}}
	groups, err := Purchase(s.demand, s.allocations, s.conditions)
	if err != nil {
		return out, err
	}
	purchases := make([]PurchaseCharge, len(groups))
	for i, g := range groups {
		purchases[i] = PurchaseCharge{Path: g.Condition.Path, ComponentID: fmt.Sprintf("purchase.%d", i+1),
			ObligationID: fmt.Sprintf("goods.%d", i+1), Includes: g.Condition.IncludedObligations}
	}
	if out.plan, err = PlanCharges(purchases, s.fees, s.sale, s.required); err != nil {
		return out, err
	}
	base := map[string]kernel.Money{}
	var amounts []kernel.Money
	for i, g := range groups {
		c, err := Cost(g.Condition.Path, g.Amount, s.goodsIn, s.goodsTax, s.fx)
		if err != nil {
			return out, err
		}
		shares, err := g.Allocate(c.Amount)
		if err != nil {
			return out, err
		}
		for _, sh := range shares {
			prev, ok := base[sh.LineID]
			if !ok {
				prev, _ = kernel.NewMoney(s.fx.Target, 0)
			}
			base[sh.LineID], _ = prev.Add(sh.Amount)
		}
		out.components[purchases[i].ComponentID], out.shares[purchases[i].ComponentID] = c.Amount, shares
		amounts = append(amounts, c.Amount)
	}
	lines := make([]BaseLine, len(s.demand))
	for i, d := range s.demand {
		lines[i] = BaseLine{LineID: d.LineID, Quantity: d.Quantity.Value(), Purchase: base[d.LineID]}
	}
	for _, f := range s.fees {
		if d, _ := out.plan.Decision(f.ComponentID); d.Status != Payable {
			continue
		}
		q, err := f.Chargeable.Quantity(s.measurement)
		if err != nil {
			return out, err
		}
		raw, err := f.Billing.Charge(f.Chargeable, q)
		if err != nil {
			return out, err
		}
		c, err := Cost(f.Path, raw, s.feeIn, s.feeTax, s.fx)
		if err != nil {
			return out, err
		}
		shares, err := f.Allocation.Distribute(c.Amount, lines)
		if err != nil {
			return out, err
		}
		out.components[f.ComponentID], out.shares[f.ComponentID] = c.Amount, shares
		amounts = append(amounts, c.Amount)
	}
	out.total, err = Total(s.fx.Target, "/result/total_minor", amounts...)
	return out, err
}

// The totals of Python test_costing_domains.py and test_costing.py on the fixture, in cents, each
// with the independent arithmetic that yields it.
func TestPythonFixtureTotals(t *testing.T) {
	cases := []struct {
		name   string // the Python test
		edit   func(s *scenario)
		want   int64
		oracle int64
	}{
		{"test_costing.py test_actual_database_inputs_evaluate_freeze_and_read_after_restart", func(*scenario) {},
			66000, 120*500 + 6000},
		{"test_explicit_fx_produces_independent_known_cost", func(s *scenario) {
			s.goodsIn = eur
			s.fx.Rates = []FXRate{pythonRate(t)}
		}, 72000, 120*500*11/10 + 6000},
		{"test_nonrecoverable_exclusive_tax_increases_cost", func(s *scenario) {
			s.goodsTax = TaxTreatment{Path: "/tax_profile/domain.tax", Rate: dec("0.13"), PriceBasis: TaxExclusive, Recovery: Nonrecoverable}
		}, 73800, 60000 + 60000*13/100 + 6000},
		{"test_recoverable_exclusive_tax_does_not_increase_cost", func(s *scenario) {
			s.goodsTax = TaxTreatment{Path: "/tax_profile/domain.tax", Rate: dec("0.13"), PriceBasis: TaxExclusive, Recovery: Recoverable}
		}, 66000, 60000 + 6000},
		{"test_inclusive_recoverable_tax_is_removed_once", func(s *scenario) {
			s.goodsTax = TaxTreatment{Path: "/tax_profile/domain.tax", Rate: dec("0.13"), PriceBasis: TaxInclusive, Recovery: Recoverable}
		}, 59097, (2*60000*100+113)/(2*113) + 6000},
		{"test_graduated_tiers_price_each_band_once", func(s *scenario) {
			s.conditions[0].Price = twoTiers(ModeGraduated)
			s.conditions[0].Price.Path = s.conditions[0].Path
		}, 64000, 100*500 + 20*400 + 6000},
		{"test_explicit_sku_conversion_prices_ten_dozen_as_120_pieces", func(s *scenario) {
			s.conditions = []Condition{dozenCondition(t, "1", dozenToPCS)}
			s.allocations = []Allocation{dozenAllocation(t)}
		}, 66000, 10*6000 + 6000},
		{"test_different_supplier_payment_schedule_is_not_imputed_financing (cost unchanged)", func(*scenario) {},
			66000, 66000},
		{"test_anchor_history_does_not_reactivate_old_commercial_role (cost unchanged)", func(*scenario) {},
			66000, 66000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := pythonFixture(t)
			c.edit(&s)
			e, err := evaluate(s)
			ok(t, err)
			if e.total.Minor().Int64() != c.want || c.oracle != c.want {
				t.Fatalf("total %s, want %d (oracle %d)", e.total.MinorString(), c.want, c.oracle)
			}
		})
	}
}

// Python test_three_line_one_cent_allocation_has_deterministic_remainder: three lines L3, L2, L1 of
// 1 PCS each at 5 USD and a 0.01 USD freight by quantity; the total is 1501 cents and the cent goes
// to L1.
func TestPythonThreeLineOneCent(t *testing.T) {
	s := pythonFixture(t)
	s.demand, s.allocations = nil, nil
	for i, id := range []string{"L3", "L2", "L1"} {
		s.demand = append(s.demand, DemandLine{Path: fmt.Sprintf("/context/lines/%d", i), LineID: id, SKU: "sku.1.profile@1", Quantity: qty(t, "1", "PCS")})
		a := baseAllocation(t, "1")
		a.Path, a.LineID = fmt.Sprintf("/allocations/%d", i), id
		s.allocations = append(s.allocations, a)
	}
	s.conditions[0].CoveredLines = []string{"L3", "L2", "L1"}
	s.conditions[0].MOQ.Quantity = qty(t, "1", "PCS")
	s.fees[0].Billing.Amount = dec("0.01")
	s.fees[0].Allocation.Targets = []string{"L3", "L2", "L1"}
	e, err := evaluate(s)
	ok(t, err)
	if e.total.MinorString() != "1501" {
		t.Fatalf("total %s, want 1501", e.total.MinorString())
	}
	if got := amounts(e.shares["fee.1"]); got != "L1=0.01 L2=0.00 L3=0.00" {
		t.Fatalf("freight %s", got)
	}
	if got := amounts(e.shares["purchase.1"]); got != "L1=5.00 L2=5.00 L3=5.00" {
		t.Fatalf("goods %s", got)
	}
}

// T02 §11 首批验收样例 whose subject is calculation (rows 1–7), each against independent integer
// arithmetic in cents. Rows 8–10 (concurrency and replay, history and new use, assumptions and
// drafts) belong to the application layer.
func TestT02Examples(t *testing.T) {
	// 1 既有基准回归: 120 PCS × 5 USD and a separate 60 USD charge: 600 + 60 = 660 USD.
	e, err := evaluate(pythonFixture(t))
	ok(t, err)
	if e.total.Minor().Int64() != 120*500+6000 || e.components["purchase.1"].Amount() != "600.00" || e.components["fee.1"].Amount() != "60.00" {
		t.Fatalf("row 1: %s", e.total)
	}

	// 2 两家同口径比较: A at 5 per PCS with the same freight obligation included costs 600 and does
	// not add the 60; B at 480 per 100 PCS pays freight separately: 576 + 60 = 636. Both stay.
	a := pythonFixture(t)
	a.conditions[0].IncludedObligations = []string{"freight.1"}
	ea, err := evaluate(a)
	ok(t, err)
	b := pythonFixture(t)
	b.conditions[0].Price = flat(b.conditions[0].Path, "480")
	b.conditions[0].Price.BasisQuantity = dec("100")
	eb, err := evaluate(b)
	ok(t, err)
	if ea.total.Minor().Int64() != 120*500 || eb.total.Minor().Int64() != 120*48000/100+6000 {
		t.Fatalf("row 2: A %s, B %s", ea.total, eb.total)
	}
	if d, _ := ea.plan.Decision("fee.1"); d.Status != Included {
		t.Fatalf("row 2: A's freight %s", d.Status)
	}

	// 3 全量／累进阶梯: 150 PCS all_units 150 × 4 = 600, graduated 100 × 5 + 50 × 4 = 700.
	for mode, want := range map[PricingMode]int64{ModeAllUnits: 150 * 400, ModeGraduated: 100*500 + 50*400} {
		got, err := twoTiers(mode).Amount(dec("150"), "/q")
		ok(t, err)
		if got.ScaledHalfUp(2).Int64() != want {
			t.Fatalf("row 3 %s: %s", mode, got)
		}
	}

	// 4 包装及MOQ: 10 DOZEN × 60 = 600 USD for 120 PCS; an MOQ of 11 DOZEN blocks.
	d := pythonFixture(t)
	d.conditions, d.allocations = []Condition{dozenCondition(t, "10", dozenToPCS)}, []Allocation{dozenAllocation(t)}
	ed, err := evaluate(d)
	ok(t, err)
	if ed.components["purchase.1"].Minor().Int64() != 10*6000 {
		t.Fatalf("row 4: %s", ed.components["purchase.1"])
	}
	d.conditions = []Condition{dozenCondition(t, "11", dozenToPCS)}
	_, err = evaluate(d)
	rejected(t, err, CodeMOQNotMet, "")

	// 5 分组MOQ: covered by TestGroupMOQ (members 24 + 36 + 12 + 48 = 120 ≥ 100; L5 excluded).
	_, err = Purchase(groupScenario(t, "100", MOQScopeExplicitGroup))
	ok(t, err)

	// 6 费用尾差: 5 cents over three equal weights: ⌊5/3⌋ = 1 each, 2 left to the first two IDs.
	shares, err := Allocate("/a", money(t, usd, 5), BasisQuantity, equalWeights("L1", "L2", "L3"))
	ok(t, err)
	if q := int64(5 / 3); amounts(shares) != fmt.Sprintf("L1=0.0%d L2=0.0%d L3=0.0%d", q+1, q+1, q) {
		t.Fatalf("row 6: %s", amounts(shares))
	}

	// 7 跨币种: 100 USD × 7.123456 = 712.3456 → 712.35 CNY (TestFXCrossCurrency has the oracle);
	// without direction, stale or of quotation purpose it does not pass (TestPythonFXVectors).
	x := FX{Path: "/fx_refs", Target: cny, At: clock, MaxAge: 1, Rates: []FXRate{{Path: "/fx/0", From: usd, To: cny,
		Rate: dec("7.123456"), Purpose: PurposeCostEstimate, ObservedAt: clock, Validity: validity(t, "2026-10-22T00:00:00Z", "2026-10-23T00:00:00Z")}}}
	got, _, err := x.Convert(money(t, usd, 10000))
	ok(t, err)
	if got.Minor().Int64() != (10000*7123456+500000)/1000000 {
		t.Fatalf("row 7: %s", got)
	}
}

// CompareCashflow (Python test_different_supplier_payment_schedule_is_not_imputed_financing): a
// supplier paid 14 days later than the customer pays is different_cashflow, not a cost; the result
// names the cost categories it does not estimate.
func TestCashflow(t *testing.T) {
	sale := []PaymentStage{{Percent: dec("100"), Trigger: "before_shipment", Days: 0, DayBasis: "calendar", Calendar: "payment.calendar@1"}}
	supplier := []PaymentStage{{Percent: dec("100"), Trigger: "before_shipment", Days: 14, DayBasis: "calendar", Calendar: "payment.calendar@1"}}
	if CompareCashflow(sale, sale) != CashflowMatched || CompareCashflow(sale, supplier) != CashflowDifferent {
		t.Fatal("cash flow comparison")
	}
	split := []PaymentStage{
		{Percent: dec("70"), Trigger: "after_shipment", Days: 30, DayBasis: "calendar", Calendar: "c@1"},
		{Percent: dec("30"), Trigger: "order_confirmed", Days: 0, DayBasis: "calendar", Calendar: "c@1"},
	}
	reordered := []PaymentStage{split[1], split[0]}
	if CompareCashflow(split, reordered) != CashflowMatched {
		t.Fatal("stage order is not a difference")
	}
	if len(ExcludedCostCategories) != 3 || ExcludedCostCategories[0] != "imputed_financing_cost" {
		t.Fatal("excluded categories")
	}
}
