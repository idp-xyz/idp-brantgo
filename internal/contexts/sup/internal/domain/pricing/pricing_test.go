package pricing

import (
	"fmt"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The vectors of these tests are CST's (cst/internal/domain/rules: tiers_test.go
// TestScheduleRefusals, purchase_test.go TestConditionCheck, billing_test.go TestBilling and
// TestChargeableScopes, allocation_test.go TestAllocationRules, charges_test.go TestCheckFees), with
// the same inputs, codes and paths: SUP verifies what CST evaluates, and the two must answer alike.
// A vector changed on one side is changed on the other. They come from T02 §5, §6, §11 and §13
// and from Python V0.50 tests, cited where they are used.

func dec(s string) kernel.Decimal { return kernel.MustDecimal(s) }

func qty(t *testing.T, v string, unit kernel.UnitCode) kernel.Quantity {
	t.Helper()
	q, err := kernel.ParseQuantity(v, unit)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func rejected(t *testing.T, err error, code, path string) {
	t.Helper()
	r, ok := kernel.AsRejection(err)
	if !ok {
		t.Fatalf("got %v, want rejection %s at %q", err, code, path)
	}
	if r.Code != code || (path != "" && r.Fields[0].Path != path) {
		t.Fatalf("got %s at %s (%s), want %s at %q", r.Code, r.Fields[0].Path, r.Fields[0].Reason, code, path)
	}
	want := Categories[code]
	if want == "" {
		want = kernel.CategoryInvalid
	}
	if r.Category != want {
		t.Fatalf("%s has category %s", code, r.Category)
	}
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func flat(path, price string) Schedule {
	return Schedule{Path: path, Mode: ModeFlat, Scope: TierScopeSKU, BasisQuantity: dec("1"),
		Tiers: []Tier{{Lower: dec("0"), UnitPrice: dec(price)}}}
}

// twoTiers is T02 §11's [0,100) at 5 and [100,∞) at 4.
func twoTiers(mode PricingMode) Schedule {
	return Schedule{Path: "/lines/0", Mode: mode, Scope: TierScopeSKU, BasisQuantity: dec("1"), Tiers: []Tier{
		{Lower: dec("0"), Upper: dec("100"), Bounded: true, UnitPrice: dec("5")},
		{Lower: dec("100"), UnitPrice: dec("4")},
	}}
}

// SUP-INV-002 (CST TestScheduleRefusals): tiers start at 0, are contiguous without overlap, and only
// the last is open-ended; a mode the offer does not determine stays pending (PRICE_TIER_AMBIGUOUS);
// group tiers are UNSUPPORTED_PRICING_MODE (T02 §13.1, §13.9).
func TestScheduleRefusals(t *testing.T) {
	gap := twoTiers(ModeGraduated)
	gap.Tiers[1].Lower = dec("101") // Python test_costing_domains.py test_gap_in_price_tiers_cannot_be_verified
	overlap := twoTiers(ModeGraduated)
	overlap.Tiers[1].Lower = dec("90")
	firstNotZero := twoTiers(ModeGraduated)
	firstNotZero.Tiers[0].Lower = dec("1")
	openMiddle := twoTiers(ModeGraduated)
	openMiddle.Tiers[0].Bounded = false
	empty := twoTiers(ModeGraduated)
	empty.Tiers[1].Upper, empty.Tiers[1].Bounded = dec("100"), true
	flatTwo := twoTiers(ModeFlat)
	pending := twoTiers(ModeUndetermined)
	group := twoTiers(ModeGraduated)
	group.Scope = TierScopeExplicitGroup
	unknownScope := twoTiers(ModeGraduated)
	unknownScope.Scope = "region"
	unknownMode := twoTiers("volume_discount")
	zero := flat("/lines/0", "0") // Python test_costing_domains.py test_unexplained_zero_unit_price_is_rejected
	explained := flat("/lines/0", "0")
	explained.Tiers[0].ZeroPriceReason = "verified free sample"
	negative := flat("/lines/0", "0")
	negative.Tiers[0].UnitPrice = dec("-1")
	tooPrecise := flat("/lines/0", "0.000000001")
	tooLarge := flat("/lines/0", "1")
	tooLarge.Tiers[0].UnitPrice = kernel.DecimalFromInt(1_000_000_000_000).Add(dec("0.01"))
	noBasis := flat("/lines/0", "5")
	noBasis.BasisQuantity = dec("0")
	noTiers := flat("/lines/0", "5")
	noTiers.Tiers = nil
	many := Schedule{Path: "/lines/0", Mode: ModeGraduated, BasisQuantity: dec("1")}
	for i := range MaxTiers + 1 {
		many.Tiers = append(many.Tiers, Tier{Lower: dec(fmt.Sprint(i)), Upper: dec(fmt.Sprint(i + 1)), Bounded: true, UnitPrice: dec("1")})
	}

	for _, c := range []struct {
		name       string
		s          Schedule
		code, path string
	}{
		{"gap", gap, CodePriceTierAmbiguous, "/lines/0/tiers/1/lower_inclusive"},
		{"overlap", overlap, CodePriceTierAmbiguous, "/lines/0/tiers/1/lower_inclusive"},
		{"first tier above 0", firstNotZero, CodePriceTierAmbiguous, "/lines/0/tiers/0/lower_inclusive"},
		{"open-ended middle tier", openMiddle, CodePriceTierAmbiguous, "/lines/0/tiers/0/upper_exclusive"},
		{"empty band", empty, CodePriceTierAmbiguous, "/lines/0/tiers/1/upper_exclusive"},
		{"flat with two tiers", flatTwo, CodePriceTierAmbiguous, "/lines/0/tiers"},
		{"undetermined mode stays pending", pending, CodePriceTierAmbiguous, "/lines/0/pricing_mode"},
		{"unknown mode", unknownMode, CodeUnsupportedPricingMode, "/lines/0/pricing_mode"},
		{"group tiers", group, CodeUnsupportedPricingMode, "/lines/0/price_tier_scope"},
		{"unknown scope", unknownScope, kernel.CodeSchemaInvalid, "/lines/0/price_tier_scope"},
		{"zero price without reason", zero, CodeCostInputNotReady, "/lines/0/tiers/0/zero_price_reason"},
		{"negative price", negative, kernel.CodeInvalidKnownValue, "/lines/0/tiers/0/unit_price"},
		{"nine fractional digits", tooPrecise, kernel.CodeSchemaInvalid, "/lines/0/tiers/0/unit_price"},
		{"price above 10^12", tooLarge, CodeCalculationLimitExceeded, "/lines/0/tiers/0/unit_price"},
		{"zero basis", noBasis, kernel.CodeInvalidKnownValue, "/lines/0/price_basis_quantity"},
		{"no tiers", noTiers, CodePriceTierAmbiguous, "/lines/0/tiers"},
		{"51 tiers", many, CodeCalculationLimitExceeded, "/lines/0/tiers"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rejected(t, c.s.Check(), c.code, c.path)
		})
	}
	ok(t, explained.Check())
	ok(t, twoTiers(ModeAllUnits).Check())
	ok(t, twoTiers(ModeGraduated).Check())
	ok(t, flat("/lines/0", "5").Check())
}

// baseCondition is the Python costing fixture's supplier offer line OL1 (tests/fixtures/
// costing_inputs.json), as CST's purchase_test.go states it: PCS (precision 0), flat 5 USD per 1
// PCS, MOQ 100 PCS per SKU.
func baseCondition(t *testing.T) Condition {
	return Condition{
		Path: "/supplier_offer/offer.1/lines/0", LineID: "OL1", PurchaseUnit: "PCS", PurchasePrecision: 0, PriceUnit: "PCS", TierUnit: "PCS",
		Price: flat("/supplier_offer/offer.1/lines/0", "5"),
		MOQ:   MOQ{Quantity: qty(t, "100", "PCS"), Scope: MOQScopeSKU},
	}
}

// CST TestConditionCheck: the verify-time rules of a condition line.
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
		{"MOQ above 10^6", func(c *Condition) { c.MOQ.Quantity = qty(t, "1000001", "PCS") }, CodeCalculationLimitExceeded, "/supplier_offer/offer.1/lines/0/moq/quantity"},
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

	// T02 §11 分组MOQ: one rule per explicit group, stated alike on every line of it.
	group := func(path, id, quantity string) Condition {
		c := baseCondition(t)
		c.Path, c.LineID = path, id
		c.MOQ = MOQ{Quantity: qty(t, quantity, "PCS"), Scope: MOQScopeExplicitGroup, GroupID: "G1", Members: []string{"L1", "L2"}}
		return c
	}
	ok(t, CheckConditions([]Condition{group("/lines/0", "OL1", "100"), group("/lines/1", "OL2", "100")}, []string{"L1", "L2"}))
	rejected(t, CheckConditions([]Condition{group("/lines/0", "OL1", "100"), group("/lines/1", "OL2", "120")}, []string{"L1", "L2"}),
		kernel.CodeReferenceInvalid, "/lines/1/moq")
}

// CST TestBilling and TestChargeableScopes: tariffs with their chargeable rule; T02 §13.8's
// maximum and rounding at one level and an explicit divisor.
func TestBillingAndChargeable(t *testing.T) {
	fixed := ChargeableRule{Path: "/fees/0/chargeable", Kind: ChargeFixed}
	weight := ChargeableRule{Path: "/fees/0/chargeable", Kind: ChargeActualWeight, BillingIncrement: dec("1"), Rounding: RoundCeil, RoundingScope: Shipment}
	freight := Billing{Path: "/fees/0/billing", Kind: BillFixed, Amount: dec("60")}
	perKG := Billing{Path: "/fees/0/billing", Kind: BillPerBasis, Rate: dec("2.5"), BasisQuantity: dec("10"), Minimum: dec("30"), Unit: "KG"}
	tiered := Billing{Path: "/fees/0/billing", Kind: BillTiered, BasisQuantity: dec("1"), Minimum: dec("0"), Unit: "KG", Tiers: twoTiers(ModeGraduated)}
	ok(t, freight.Check(fixed))
	ok(t, perKG.Check(weight))
	ok(t, tiered.Check(weight))
	zero := freight
	zero.Amount = dec("0")
	explained := zero
	explained.ZeroPriceReason = "included in the annual contract, verified"
	ok(t, explained.Check(fixed))
	volumetric := func(scope RoundingScope) ChargeableRule {
		return ChargeableRule{Path: "/fees/0/chargeable", Kind: ChargeMaxActualVolumetric, BillingIncrement: dec("1"),
			Rounding: RoundCeil, RoundingScope: scope, MaxScope: scope, DivisorM3PerKG: dec("0.005")}
	}
	mixed := volumetric(PerPackage)
	mixed.MaxScope = Shipment
	noDivisor := volumetric(Shipment)
	noDivisor.DivisorM3PerKG = dec("0")
	ok(t, volumetric(PerPackage).Check())
	for _, c := range []struct {
		name       string
		b          Billing
		rule       ChargeableRule
		code, path string
	}{
		{"zero fixed charge", zero, fixed, CodeCostInputNotReady, "/fees/0/billing/zero_price_reason"},
		{"fixed basis billed per kg", perKG, fixed, CodeUnsupportedPricingMode, "/fees/0/billing/kind"},
		{"measured basis billed fixed", freight, weight, CodeUnsupportedPricingMode, "/fees/0/billing/kind"},
		{"weight billed in M3", Billing{Path: "/fees/0/billing", Kind: BillPerBasis, Rate: dec("1"), BasisQuantity: dec("1"), Minimum: dec("0"), Unit: "M3"}, weight,
			kernel.CodeReferenceInvalid, "/fees/0/billing/unit_ref"},
		{"zero rate", Billing{Path: "/fees/0/billing", Kind: BillPerBasis, Rate: dec("0"), BasisQuantity: dec("1"), Minimum: dec("0"), Unit: "KG"}, weight,
			CodeCostInputNotReady, "/fees/0/billing/zero_price_reason"},
		{"negative charge is not a discount", Billing{Path: "/fees/0/billing", Kind: BillFixed, Amount: dec("-5")}, fixed,
			kernel.CodeInvalidKnownValue, "/fees/0/billing/amount"},
		{"unknown tariff", Billing{Path: "/fees/0/billing", Kind: "zone_matrix"}, weight, CodeUnsupportedPricingMode, "/fees/0/billing/kind"},
		{"unknown chargeable basis", freight, ChargeableRule{Path: "/fees/0/chargeable", Kind: "per_container"}, CodeUnsupportedPricingMode, "/fees/0/chargeable/kind"},
		{"maximum and rounding at different levels", tiered, mixed, CodeUnsupportedPricingMode, "/fees/0/chargeable/max_scope"},
		{"no volumetric divisor", tiered, noDivisor, kernel.CodeInvalidKnownValue, "/fees/0/chargeable/divisor_m3_per_kg"},
		{"no billing increment", perKG, ChargeableRule{Path: "/fees/0/chargeable", Kind: ChargeActualWeight, Rounding: RoundCeil, RoundingScope: Shipment},
			kernel.CodeInvalidKnownValue, "/fees/0/chargeable/billing_increment"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rejected(t, c.b.Check(c.rule), c.code, c.path)
		})
	}
	// CST's ChargeableRule.Quantity(nil) refuses a measured charge without its measurement.
	rejected(t, volumetric(Shipment).CheckMeasured(), CodeCostInputNotReady, "/fees/0/chargeable/measurement_ref")
	measured := volumetric(Shipment)
	measured.Measured = true
	ok(t, measured.CheckMeasured())
	ok(t, fixed.CheckMeasured())
}

// CST TestAllocationRules: Python test_costing_domains.py test_zero_explicit_allocation_weights_are_rejected
// and test_weight_allocation_requires_measurement.
func TestAllocationRules(t *testing.T) {
	zero := AllocationRule{Path: "/fees/0/allocation", Basis: BasisExplicit, Weights: []ExplicitWeight{{LineID: "L1", Weight: 0}}}
	rejected(t, zero.Check(), CodeAllocationInvalid, "/fees/0/allocation/weights")
	weight := AllocationRule{Path: "/fees/0/allocation", Basis: BasisWeight, Targets: []string{"L1"}}
	rejected(t, weight.Check(), CodeCostInputNotReady, "/fees/0/allocation/measurement_ref")
	weight.Measured = true
	ok(t, weight.Check())
	unpurchased := AllocationRule{Path: "/fees/0/allocation", Basis: BasisExplicit, Weights: []ExplicitWeight{{LineID: "not.purchased", Weight: 1}}}
	ok(t, unpurchased.Check()) // which lines a scenario buys is CST's to decide
	tooHeavy := AllocationRule{Path: "/fees/0/allocation", Basis: BasisExplicit, Weights: []ExplicitWeight{{LineID: "L1", Weight: MaxExplicitWeight + 1}}}
	rejected(t, tooHeavy.Check(), CodeAllocationInvalid, "/fees/0/allocation/weights/0/weight")
	twice := AllocationRule{Path: "/fees/0/allocation", Basis: BasisExplicit, Weights: []ExplicitWeight{{LineID: "L1", Weight: 1}, {LineID: "L1", Weight: 2}}}
	rejected(t, twice.Check(), kernel.CodeDuplicateLineID, "/fees/0/allocation/weights/1/line_id")
	unknown := AllocationRule{Path: "/fees/0/allocation", Basis: "revenue", Targets: []string{"L1"}}
	rejected(t, unknown.Check(), kernel.CodeSchemaInvalid, "/fees/0/allocation/basis")
	noTargets := AllocationRule{Path: "/fees/0/allocation", Basis: BasisQuantity}
	rejected(t, noTargets.Check(), CodeAllocationInvalid, "/fees/0/allocation/target_line_ids")
}

// fixtureFee is the Python costing fixture's logistics offer log.1 fee F1, as CST's charges_test.go
// states it.
func fixtureFee() Fee {
	return Fee{Path: "/logistics_offer/log.1/fees/0", ID: "F1", ObligationID: "freight.1", Party: Seller,
		Chargeable: ChargeableRule{Path: "/logistics_offer/log.1/fees/0/chargeable", Kind: ChargeFixed},
		Billing:    Billing{Path: "/logistics_offer/log.1/fees/0/billing", Kind: BillFixed, Amount: dec("60")},
		Allocation: AllocationRule{Path: "/logistics_offer/log.1/fees/0/allocation", Basis: BasisQuantity, Targets: []string{"L1"}},
	}
}

// CST TestCheckFees: Python test_costing_domains.py test_duplicate_fee_obligation_cannot_be_hidden_by_new_id.
func TestCheckFees(t *testing.T) {
	ok(t, CheckFees([]Fee{fixtureFee()}))
	dup := fixtureFee()
	dup.Path, dup.ObligationID = "/logistics_offer/log.1/fees/1", "other.1"
	rejected(t, CheckFees([]Fee{fixtureFee(), dup}), kernel.CodeDuplicateLineID, "/logistics_offer/log.1/fees/1/id")
	renamed := fixtureFee()
	renamed.Path, renamed.ID = "/logistics_offer/log.1/fees/1", "different.name"
	rejected(t, CheckFees([]Fee{fixtureFee(), renamed}), CodeChargeDoubleCount, "/logistics_offer/log.1/fees/1/obligation_id")
	noParty := fixtureFee()
	noParty.Party = ""
	rejected(t, CheckFees([]Fee{noParty}), kernel.CodeSchemaInvalid, "/logistics_offer/log.1/fees/0/responsible_party")
	zeroWeights := fixtureFee()
	zeroWeights.Allocation = AllocationRule{Path: "/logistics_offer/log.1/fees/0/allocation", Basis: BasisExplicit, Weights: []ExplicitWeight{{LineID: "L1"}}}
	rejected(t, CheckFees([]Fee{zeroWeights}), CodeAllocationInvalid, "/logistics_offer/log.1/fees/0/allocation/weights")
	byWeight := fixtureFee()
	byWeight.Allocation = AllocationRule{Path: "/logistics_offer/log.1/fees/0/allocation", Basis: BasisWeight, Targets: []string{"L1"}}
	rejected(t, CheckFees([]Fee{byWeight}), CodeCostInputNotReady, "/logistics_offer/log.1/fees/0/allocation/measurement_ref")
	many := make([]Fee, MaxComponents+1)
	for i := range many {
		many[i] = fixtureFee()
		many[i].Path, many[i].ID, many[i].ObligationID = fmt.Sprintf("/fees/%d", i), fmt.Sprint("F", i), fmt.Sprint("o", i)
	}
	rejected(t, CheckFees(many), CodeCalculationLimitExceeded, "/fees/500")
}

func TestHelpers(t *testing.T) {
	rejected(t, CheckQuantity(dec("0.0000001"), "/q"), kernel.CodeSchemaInvalid, "/q")
	rejected(t, CheckQuantity(dec("0"), "/q"), kernel.CodeInvalidKnownValue, "/q")
	rejected(t, CheckPrecision(dec("1.5"), 0, "/q"), kernel.CodeInvalidKnownValue, "/q")
	ok(t, CheckPrecision(dec("1.5"), 1, "/q"))
	ok(t, Unique([]string{"a", "b"}, "/ids"))
	rejected(t, Unique([]string{"a", "b", "a"}, "/ids"), kernel.CodeDuplicateLineID, "/ids/2")
}
