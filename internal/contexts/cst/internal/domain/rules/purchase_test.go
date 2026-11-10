package rules

import (
	"fmt"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var offer1 = SourceKey{Kind: "offer", ID: "offer.1", Revision: 1}

// baseCondition is the Python costing fixture's supplier offer line OL1 (tests/fixtures/
// costing_inputs.json): sku.1, PCS (precision 0), flat 5 USD per 1 PCS, MOQ 100 PCS per SKU.
func baseCondition(t *testing.T) Condition {
	return Condition{
		Path: "/supplier_offer/offer.1/lines/0", Source: offer1, LineID: "OL1", SKU: "sku.1.profile@1",
		CoveredLines: []string{"L1"}, PurchaseUnit: "PCS", PurchasePrecision: 0, PriceUnit: "PCS", TierUnit: "PCS",
		Price: flat("/supplier_offer/offer.1/lines/0", "5"),
		MOQ:   MOQ{Quantity: qty(t, "100", "PCS"), Scope: MOQScopeSKU},
	}
}

func baseDemand(t *testing.T) []DemandLine {
	return []DemandLine{{Path: "/context/lines/0", LineID: "L1", SKU: "sku.1.profile@1", Quantity: qty(t, "120", "PCS")}}
}

func baseAllocation(t *testing.T, q string) Allocation {
	return Allocation{Path: "/allocations/0", LineID: "L1", Source: offer1, ConditionLineID: "OL1", Quantity: qty(t, q, "PCS")}
}

// T02 §11 既有基准回归 and Python test_costing.py test_actual_database_inputs_evaluate_freeze_and_
// read_after_restart (goods part of 66000): 120 PCS × 5 USD = 600.
func TestPurchaseBaseline(t *testing.T) {
	groups, err := Purchase(baseDemand(t), []Allocation{baseAllocation(t, "120")}, []Condition{baseCondition(t)})
	ok(t, err)
	if len(groups) != 1 || groups[0].Amount.String() != "600" || groups[0].Quantity.String() != "120 PCS" {
		t.Fatalf("groups = %+v", groups)
	}
	if m := groups[0].Members; len(m) != 1 || m[0].LineID != "L1" || m[0].Quantity.String() != "120" {
		t.Fatalf("members = %+v", m)
	}
}

// Python test_costing_domains.py vectors of the purchase rules, each refused before any amount.
func TestPurchaseRefusals(t *testing.T) {
	cases := []struct {
		name       string
		edit       func(c *Condition, a *Allocation)
		code, path string
	}{
		// test_fractional_piece_quantity_is_rejected: 119.5 PCS of a precision-0 unit.
		{"fractional piece", func(_ *Condition, a *Allocation) { a.Quantity = qty(t, "119.5", "PCS") },
			kernel.CodeInvalidKnownValue, "/allocations/0/purchase_quantity"},
		// test_purchase_quantity_must_cover_original_demand: 119 bought for 120 demanded.
		{"under demand", func(_ *Condition, a *Allocation) { a.Quantity = qty(t, "119", "PCS") },
			CodeComparisonBasisMismatch, "/context/lines/0/quantity"},
		// The base scenario never over-buys either (T02 §5).
		{"over demand", func(_ *Condition, a *Allocation) { a.Quantity = qty(t, "121", "PCS") },
			CodeComparisonBasisMismatch, "/context/lines/0/quantity"},
		// test_moq_is_not_bypassed_by_demand_equality: MOQ 121 with 120 demanded and bought.
		{"MOQ above demand", func(c *Condition, _ *Allocation) { c.MOQ.Quantity = qty(t, "121", "PCS") },
			CodeMOQNotMet, "/supplier_offer/offer.1/lines/0/moq/quantity"},
		// test_order_multiple_is_checked_after_quantity_aggregation: 120 is not a multiple of 7.
		{"order multiple", func(c *Condition, _ *Allocation) { c.OrderMultiple = dec("7") },
			CodeOrderMultipleInvalid, "/supplier_offer/offer.1/lines/0/order_multiple"},
		{"unknown demand line", func(_ *Condition, a *Allocation) { a.LineID = "L9" },
			kernel.CodeReferenceInvalid, "/allocations/0/rfq_line_id"},
		{"unknown condition line", func(_ *Condition, a *Allocation) { a.ConditionLineID = "OL9" },
			kernel.CodeReferenceInvalid, "/allocations/0/source"},
		{"line the offer does not answer", func(c *Condition, _ *Allocation) { c.CoveredLines = []string{"L2"} },
			kernel.CodeReferenceInvalid, "/allocations/0/rfq_line_id"},
		{"other SKU", func(c *Condition, _ *Allocation) { c.SKU = "sku.2.profile@1" },
			kernel.CodeReferenceInvalid, "/allocations/0/source"},
		{"purchase unit other than the condition's", func(_ *Condition, a *Allocation) { a.Quantity = qty(t, "10", "DOZEN") },
			CodeDependencyNotSupported, "/allocations/0/purchase_unit_ref"},
		// Python purchase_groups: the first path prices in the purchase unit only.
		{"price unit other than the purchase unit", func(c *Condition, _ *Allocation) { c.PriceUnit = "DOZEN" },
			CodeDependencyNotSupported, "/supplier_offer/offer.1/lines/0/price_unit_ref"},
		{"quantity above 10^6", func(_ *Condition, a *Allocation) { a.Quantity = qty(t, "1000001", "PCS") },
			CodeCalculationLimitExceeded, "/allocations/0/purchase_quantity"},
		{"MOQ in another unit", func(c *Condition, _ *Allocation) { c.MOQ.Quantity = qty(t, "1", "DOZEN") },
			kernel.CodeReferenceInvalid, "/supplier_offer/offer.1/lines/0/moq/unit_ref"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cond, alloc := baseCondition(t), baseAllocation(t, "120")
			c.edit(&cond, &alloc)
			_, err := Purchase(baseDemand(t), []Allocation{alloc}, []Condition{cond})
			rejected(t, err, c.code, c.path)
		})
	}
}

// MOQ_NOT_MET names the condition's MOQ and every allocation that counted toward it.
func TestMOQRejectionNamesAllocations(t *testing.T) {
	cond := baseCondition(t)
	cond.MOQ.Quantity = qty(t, "121", "PCS")
	_, err := Purchase(baseDemand(t), []Allocation{baseAllocation(t, "120")}, []Condition{cond})
	r, _ := kernel.AsRejection(err)
	if r == nil || len(r.Fields) != 2 || r.Fields[1].Path != "/allocations/0/purchase_quantity" {
		t.Fatalf("got %v", err)
	}
}

// dozenCondition is T02 §11 包装及MOQ and Python dozen_offer: priced per DOZEN at 60 USD, bought in
// DOZEN (precision 0) for a demand of 120 PCS, converted by a published SKU conversion 1 DOZEN = 12
// PCS.
func dozenCondition(t *testing.T, moq string, conversions ...UnitConversion) Condition {
	c := baseCondition(t)
	c.PurchaseUnit, c.PriceUnit, c.TierUnit = "DOZEN", "DOZEN", "DOZEN"
	c.Price = flat(c.Path, "60")
	c.MOQ = MOQ{Quantity: qty(t, moq, "DOZEN"), Scope: MOQScopeSKU}
	c.Conversions = conversions
	return c
}

var dozenToPCS = UnitConversion{SKU: "sku.1.profile@1", From: "DOZEN", To: "PCS", Factor: dec("12"), ToPrecision: 0}

func dozenAllocation(t *testing.T) Allocation {
	a := baseAllocation(t, "10")
	a.Quantity = qty(t, "10", "DOZEN")
	return a
}

// T02 §11 包装及MOQ: 10 DOZEN cover 120 PCS and cost 10 × 60 = 600 USD with a minimum of 10 DOZEN;
// a minimum of 11 DOZEN blocks instead of buying 132 PCS. Python test_explicit_sku_conversion_
// prices_ten_dozen_as_120_pieces (600 + 60 freight = 66000 cents, the freight in
// TestPythonFixtureTotals), test_missing_sku_conversion_is_not_inferred_from_unit_name and
// test_reverse_unit_conversion_is_not_automatically_inverted.
func TestDozenPurchase(t *testing.T) {
	groups, err := Purchase(baseDemand(t), []Allocation{dozenAllocation(t)}, []Condition{dozenCondition(t, "10", dozenToPCS)})
	ok(t, err)
	if groups[0].Amount.String() != "600" || groups[0].Quantity.String() != "10 DOZEN" {
		t.Fatalf("group = %+v", groups[0])
	}

	_, err = Purchase(baseDemand(t), []Allocation{dozenAllocation(t)}, []Condition{dozenCondition(t, "11", dozenToPCS)})
	rejected(t, err, CodeMOQNotMet, "/supplier_offer/offer.1/lines/0/moq/quantity")

	_, err = Purchase(baseDemand(t), []Allocation{dozenAllocation(t)}, []Condition{dozenCondition(t, "10")})
	rejected(t, err, kernel.CodeReferenceInvalid, "/supplier_offer/offer.1/lines/0/conversion_refs")

	reverse := UnitConversion{SKU: "sku.1.profile@1", From: "PCS", To: "DOZEN", Factor: dec("0.083333"), ToPrecision: 0}
	_, err = Purchase(baseDemand(t), []Allocation{dozenAllocation(t)}, []Condition{dozenCondition(t, "10", reverse)})
	rejected(t, err, kernel.CodeReferenceInvalid, "/supplier_offer/offer.1/lines/0/conversion_refs")

	otherSKU := dozenToPCS
	otherSKU.SKU = "sku.2.profile@1"
	_, err = Purchase(baseDemand(t), []Allocation{dozenAllocation(t)}, []Condition{dozenCondition(t, "10", otherSKU)})
	rejected(t, err, kernel.CodeReferenceInvalid, "/supplier_offer/offer.1/lines/0/conversion_refs/0")

	twice := []UnitConversion{dozenToPCS, dozenToPCS}
	_, err = Purchase(baseDemand(t), []Allocation{dozenAllocation(t)}, []Condition{dozenCondition(t, "10", twice...)})
	rejected(t, err, kernel.CodeReferenceInvalid, "/supplier_offer/offer.1/lines/0/conversion_refs")

	inexact := dozenToPCS
	inexact.Factor = dec("12.5") // 10 DOZEN would be 125 PCS; 0.5 DOZEN of a PCS demand never rounds
	cond := dozenCondition(t, "1", inexact)
	alloc := dozenAllocation(t)
	alloc.Quantity = qty(t, "9", "DOZEN") // 112.5 PCS
	_, err = Purchase(baseDemand(t), []Allocation{alloc}, []Condition{cond})
	rejected(t, err, kernel.CodeInvalidKnownValue, "/allocations/0/purchase_quantity")
}

// groupScenario is T02 §11 分组MOQ and CST-INV-005: four SKUs of one specification family in an
// explicit group of the same offer (24 + 36 + 12 + 48 = 120 PCS, the BRC-001 lines) with an MOQ of
// 100, and an unrelated SKU L5 of 30 PCS under its own SKU MOQ of 10.
func groupScenario(t *testing.T, groupMOQ string, scope MOQScope) ([]DemandLine, []Allocation, []Condition) {
	var demand []DemandLine
	var allocs []Allocation
	var conds []Condition
	quantities := []string{"24", "36", "12", "48", "30"}
	members := []string{"L1", "L2", "L3", "L4"}
	for i, q := range quantities {
		line := fmt.Sprintf("L%d", i+1)
		sku := fmt.Sprintf("sku.%d@1", i+1)
		demand = append(demand, DemandLine{Path: fmt.Sprintf("/context/lines/%d", i), LineID: line, SKU: sku, Quantity: qty(t, q, "PCS")})
		c := baseCondition(t)
		c.Path, c.LineID, c.SKU, c.CoveredLines = fmt.Sprintf("/supplier_offer/offer.1/lines/%d", i), "OL"+line, sku, nil
		c.MOQ = MOQ{Quantity: qty(t, groupMOQ, "PCS"), Scope: scope}
		if scope == MOQScopeExplicitGroup {
			c.MOQ.GroupID, c.MOQ.Members = "G1", members
		}
		if i == 4 {
			c.MOQ = MOQ{Quantity: qty(t, "10", "PCS"), Scope: MOQScopeSKU}
		}
		conds = append(conds, c)
		allocs = append(allocs, Allocation{Path: fmt.Sprintf("/allocations/%d", i), LineID: line, Source: offer1,
			ConditionLineID: "OL" + line, Quantity: qty(t, q, "PCS")})
	}
	return demand, allocs, conds
}

// T02 §11 分组MOQ: only explicit members aggregate (120 ≥ 100); the unrelated SKU does not fill the
// group, and the group MOQ is not applied to each SKU. CST-INV-005 negative: under SKU scope each
// of the four lines is below 100 (MOQ_NOT_MET).
func TestGroupMOQ(t *testing.T) {
	groups, err := Purchase(groupScenario(t, "100", MOQScopeExplicitGroup))
	ok(t, err)
	if len(groups) != 5 {
		t.Fatalf("%d groups, want one per condition line", len(groups))
	}
	total := kernel.DecimalFromInt(0)
	for _, g := range groups {
		total = total.Add(g.Amount)
	}
	if total.String() != "750" { // 150 PCS × 5
		t.Fatalf("total %s, want 750", total)
	}

	// 120 members < 150 even though L5's 30 PCS of the same offer would make 150.
	_, err = Purchase(groupScenario(t, "150", MOQScopeExplicitGroup))
	rejected(t, err, CodeMOQNotMet, "/supplier_offer/offer.1/lines/0/moq/quantity")
	r, _ := kernel.AsRejection(err)
	if len(r.Fields) != 5 { // the MOQ and the four member allocations
		t.Fatalf("fields = %+v", r.Fields)
	}

	_, err = Purchase(groupScenario(t, "100", MOQScopeSKU))
	rejected(t, err, CodeMOQNotMet, "")

	// A line outside the group cannot be bought under a condition of the group.
	demand, allocs, conds := groupScenario(t, "100", MOQScopeExplicitGroup)
	conds[0].MOQ.Members = []string{"L2", "L3", "L4"}
	_, err = Purchase(demand, allocs, conds)
	rejected(t, err, kernel.CodeReferenceInvalid, "/supplier_offer/offer.1/lines/0/moq/member_line_ids")

	// Lines of one group stating different rules are refused, never merged.
	demand, allocs, conds = groupScenario(t, "100", MOQScopeExplicitGroup)
	conds[1].MOQ.Quantity = qty(t, "90", "PCS")
	_, err = Purchase(demand, allocs, conds)
	rejected(t, err, kernel.CodeReferenceInvalid, "/supplier_offer/offer.1/lines/1/moq")

	// The same group ID under another offer revision is another group.
	demand, allocs, conds = groupScenario(t, "100", MOQScopeExplicitGroup)
	other := SourceKey{Kind: "offer", ID: "offer.1", Revision: 2}
	conds[3].Source, allocs[3].Source = other, other
	_, err = Purchase(demand, allocs, conds)
	rejected(t, err, CodeMOQNotMet, "") // 72 and 48, each below 100
}

// T02 §13.3: one condition line serving several demand lines is aggregated and priced once; the
// order multiple applies to the aggregate (Python test_order_multiple_is_checked_after_quantity_
// aggregation). Two lines of 60 under an order multiple of 40 pass because 120 is a multiple.
func TestAggregationBeforeMultipleAndPrice(t *testing.T) {
	demand := []DemandLine{
		{Path: "/context/lines/0", LineID: "L1", SKU: "sku.1.profile@1", Quantity: qty(t, "60", "PCS")},
		{Path: "/context/lines/1", LineID: "L2", SKU: "sku.1.profile@1", Quantity: qty(t, "60", "PCS")},
	}
	cond := baseCondition(t)
	cond.CoveredLines = []string{"L1", "L2"}
	cond.OrderMultiple = dec("40")
	cond.Price = twoTiers(ModeGraduated)
	cond.Price.Path = cond.Path
	allocs := []Allocation{baseAllocation(t, "60"), baseAllocation(t, "60")}
	allocs[1].LineID, allocs[1].Path = "L2", "/allocations/1"
	groups, err := Purchase(demand, allocs, []Condition{cond})
	ok(t, err)
	if len(groups) != 1 || groups[0].Amount.String() != "580" || len(groups[0].Members) != 2 {
		t.Fatalf("groups = %+v; want one group of 100×5 + 20×4 = 580, never 2 × 300", groups)
	}
	shares, err := groups[0].Allocate(money(t, usd, 58000))
	ok(t, err)
	if shares[0].Amount.Amount() != "290.00" || shares[1].Amount.Amount() != "290.00" {
		t.Fatalf("shares = %+v", shares)
	}
}

// Verify-time checks of a condition line (supplier_offer.verify / purchase_terms.publish).
func TestConditionCheck(t *testing.T) {
	ok(t, CheckConditions([]Condition{baseCondition(t)}, []string{"L1"}))
	for _, c := range []struct {
		name       string
		edit       func(c *Condition)
		code, path string
	}{
		{"price unit", func(c *Condition) { c.PriceUnit = "DOZEN" }, CodeUnsupportedPricingMode, "/supplier_offer/offer.1/lines/0/price_unit_ref"},
		{"tier unit", func(c *Condition) { c.TierUnit = "DOZEN" }, CodeUnsupportedPricingMode, "/supplier_offer/offer.1/lines/0/tier_unit_ref"},
		{"MOQ unit", func(c *Condition) { c.MOQ.Quantity = qty(t, "1", "DOZEN") }, kernel.CodeReferenceInvalid, "/supplier_offer/offer.1/lines/0/moq/unit_ref"},
		{"fractional MOQ", func(c *Condition) { c.MOQ.Quantity = qty(t, "0.5", "PCS") }, kernel.CodeInvalidKnownValue, "/supplier_offer/offer.1/lines/0/moq/quantity"},
		{"fractional multiple", func(c *Condition) { c.OrderMultiple = dec("2.5") }, kernel.CodeInvalidKnownValue, "/supplier_offer/offer.1/lines/0/order_multiple"},
		{"group without members", func(c *Condition) { c.MOQ.Scope = MOQScopeExplicitGroup; c.MOQ.GroupID = "G1" }, kernel.CodeReferenceInvalid, "/supplier_offer/offer.1/lines/0/moq"},
		{"group member outside the request", func(c *Condition) {
			c.MOQ = MOQ{Quantity: c.MOQ.Quantity, Scope: MOQScopeExplicitGroup, GroupID: "G1", Members: []string{"L1", "L7"}}
		}, kernel.CodeReferenceInvalid, "/supplier_offer/offer.1/lines/0/moq/member_line_ids/1"},
		{"group member twice", func(c *Condition) {
			c.MOQ = MOQ{Quantity: c.MOQ.Quantity, Scope: MOQScopeExplicitGroup, GroupID: "G1", Members: []string{"L1", "L1"}}
		}, kernel.CodeDuplicateLineID, "/supplier_offer/offer.1/lines/0/moq/member_line_ids/1"},
		{"included obligation twice", func(c *Condition) { c.IncludedObligations = []string{"freight.1", "freight.1"} },
			kernel.CodeDuplicateLineID, "/supplier_offer/offer.1/lines/0/included_obligation_ids/1"},
		{"undetermined tiers", func(c *Condition) { c.Price.Mode = ModeUndetermined }, CodePriceTierAmbiguous, "/supplier_offer/offer.1/lines/0/pricing_mode"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cond := baseCondition(t)
			c.edit(&cond)
			rejected(t, CheckConditions([]Condition{cond}, []string{"L1"}), c.code, c.path)
		})
	}
	a, b := baseCondition(t), baseCondition(t)
	b.Path = "/supplier_offer/offer.1/lines/1"
	rejected(t, CheckConditions([]Condition{a, b}, []string{"L1"}), kernel.CodeDuplicateLineID, "/supplier_offer/offer.1/lines/1/id")
}
