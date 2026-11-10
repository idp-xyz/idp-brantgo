package rules

import (
	"fmt"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// fixtureFee is the Python costing fixture's logistics offer log.1 fee F1: obligation freight.1,
// fee type freight, borne by the seller, 60 USD fixed, allocated by quantity to L1.
func fixtureFee() Fee {
	return Fee{Path: "/logistics_offer/log.1/fees/0", Offer: "logistics_offer:log.1@1", ID: "F1", ComponentID: "fee.1",
		ObligationID: "freight.1", FeeType: "freight@1", Party: Seller,
		Chargeable: ChargeableRule{Path: "/logistics_offer/log.1/fees/0/chargeable", Kind: ChargeFixed},
		Billing:    Billing{Path: "/logistics_offer/log.1/fees/0/billing", Kind: BillFixed, Amount: dec("60")},
		Allocation: AllocationRule{Path: "/logistics_offer/log.1/fees/0/allocation", Basis: BasisQuantity, Targets: []string{"L1"}},
	}
}

func fixturePurchase() PurchaseCharge {
	return PurchaseCharge{Path: "/supplier_offer/offer.1/lines/0", ComponentID: "purchase.1", ObligationID: "goods.1"}
}

// The export sale scope scope.sale: the seller bears freight.1; the basis requires freight.
var (
	saleFreight     = []Obligation{{Path: "/sales_trade_scope/obligations/0", ID: "freight.1", FeeType: "freight@1", Bearer: Seller}}
	requiredFreight = []RequiredFee{{Path: "/context/comparison_basis/required_fee_type_refs/0", FeeType: "freight@1"}}
)

func status(t *testing.T, plan ChargePlan, id string) ChargeStatus {
	t.Helper()
	d, found := plan.Decision(id)
	if !found {
		t.Fatalf("no decision on %s", id)
	}
	return d.Status
}

// The fixture: goods and freight are both payable (600 + 60 = 660, T02 §11 既有基准回归).
func TestChargePlanBaseline(t *testing.T) {
	plan, err := PlanCharges([]PurchaseCharge{fixturePurchase()}, []Fee{fixtureFee()}, saleFreight, requiredFreight)
	ok(t, err)
	if status(t, plan, "purchase.1") != Payable || status(t, plan, "fee.1") != Payable || len(plan.Differences) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
}

// CST-INV-003 positive and T02 §11 两家同口径比较: supplier A's price already includes the freight
// obligation, so the 60 USD freight fee is shown as included and not added again; supplier B pays
// it. A costs 600, B 576 + 60 = 636 (amounts in TestT02Examples).
func TestIncludedFreightIsNotAddedAgain(t *testing.T) {
	a := fixturePurchase()
	a.Includes = []string{"freight.1"}
	plan, err := PlanCharges([]PurchaseCharge{a}, []Fee{fixtureFee()}, saleFreight, requiredFreight)
	ok(t, err)
	if d, _ := plan.Decision("fee.1"); d.Status != Included || d.IncludedIn != "purchase.1" || plan.InclusionEdges != 1 {
		t.Fatalf("fee.1 = %+v", d)
	}
	// Without the freight fee at all, A's inclusion still covers the seller's obligation.
	_, err = PlanCharges([]PurchaseCharge{a}, nil, saleFreight, requiredFreight)
	ok(t, err)
}

// CST-INV-003 negatives from Python test_costing_domains.py.
func TestChargeRefusals(t *testing.T) {
	// test_duplicate_fee_obligation_cannot_be_hidden_by_new_id: F1 copied as "different.name".
	copied := fixtureFee()
	copied.Path, copied.ID, copied.ComponentID = "/logistics_offer/log.1/fees/1", "different.name", "fee.2"
	rejected(t, CheckFees([]Fee{fixtureFee(), copied}), CodeChargeDoubleCount, "/logistics_offer/log.1/fees/1/obligation_id")
	_, err := PlanCharges([]PurchaseCharge{fixturePurchase()}, []Fee{fixtureFee(), copied}, saleFreight, requiredFreight)
	rejected(t, err, CodeChargeDoubleCount, "/logistics_offer/log.1/fees/1/obligation_id")
	// The same obligation from another logistics offer is still the same obligation.
	other := copied
	other.Offer, other.ID = "logistics_offer:log.2@1", "F1"
	_, err = PlanCharges([]PurchaseCharge{fixturePurchase()}, []Fee{fixtureFee(), other}, saleFreight, requiredFreight)
	rejected(t, err, CodeChargeDoubleCount, "/logistics_offer/log.1/fees/1/obligation_id")

	// test_nonexistent_fee_inclusion_parent_is_not_silently_ignored.
	orphan := fixtureFee()
	orphan.IncludedIn = "unknown.parent"
	_, err = PlanCharges([]PurchaseCharge{fixturePurchase()}, []Fee{orphan}, saleFreight, requiredFreight)
	rejected(t, err, CodeChargeScopeUnresolved, "/logistics_offer/log.1/fees/0/included_in_component_ref")

	// test_fee_inclusion_cycle_cannot_remove_its_own_cost: F1 included in F1.
	self := fixtureFee()
	self.IncludedIn = "F1"
	_, err = PlanCharges([]PurchaseCharge{fixturePurchase()}, []Fee{self}, saleFreight, requiredFreight)
	rejected(t, err, CodeChargeDoubleCount, "/logistics_offer/log.1/fees/0/included_in_component_ref")

	// test_missing_required_freight_is_not_zero_cost: no logistics offer, the seller bears freight.
	_, err = PlanCharges([]PurchaseCharge{fixturePurchase()}, nil, saleFreight, requiredFreight)
	rejected(t, err, CodeCostInputNotReady, "/sales_trade_scope/obligations/0")
	// Without a sale obligation, the required fee type still is not accounted for.
	_, err = PlanCharges([]PurchaseCharge{fixturePurchase()}, nil, nil, requiredFreight)
	rejected(t, err, CodeCostInputNotReady, "/context/comparison_basis/required_fee_type_refs/0")
}

// The rest of the inclusion graph: a two-fee cycle, a fee included in a fee of the other bearer, a
// fee whose obligation one price includes while it names another parent, an obligation included in
// two prices, a fee repeating a purchase component's obligation, a supplier-borne obligation the
// price does not list, and an inclusion chain longer than 8.
func TestInclusionGraph(t *testing.T) {
	f1, f2 := fixtureFee(), fixtureFee()
	f2.Path, f2.ID, f2.ComponentID, f2.ObligationID = "/logistics_offer/log.1/fees/1", "F2", "fee.2", "handling.1"
	f1.IncludedIn, f2.IncludedIn = "F2", "F1"
	_, err := PlanCharges(nil, []Fee{f1, f2}, nil, nil)
	rejected(t, err, CodeChargeDoubleCount, "/logistics_offer/log.1/fees/0/included_in_component_ref")

	f1, f2 = fixtureFee(), fixtureFee()
	f2.Path, f2.ID, f2.ComponentID, f2.ObligationID, f2.Party = "/logistics_offer/log.1/fees/1", "F2", "fee.2", "duty.1", Buyer
	f1.IncludedIn = "F2"
	_, err = PlanCharges(nil, []Fee{f1, f2}, nil, nil)
	rejected(t, err, CodeChargeScopeUnresolved, "/logistics_offer/log.1/fees/0/included_in_component_ref")

	a := fixturePurchase()
	a.Includes = []string{"freight.1"}
	f1, f2 = fixtureFee(), fixtureFee()
	f2.Path, f2.ID, f2.ComponentID, f2.ObligationID = "/logistics_offer/log.1/fees/1", "F2", "fee.2", "handling.1"
	f1.IncludedIn = "F2"
	_, err = PlanCharges([]PurchaseCharge{a}, []Fee{f1, f2}, nil, nil)
	rejected(t, err, CodeChargeScopeUnresolved, "/logistics_offer/log.1/fees/0/included_in_component_ref")

	b := fixturePurchase()
	b.Path, b.ComponentID, b.ObligationID, b.Includes = "/supplier_offer/offer.2/lines/0", "purchase.2", "goods.2", []string{"freight.1"}
	_, err = PlanCharges([]PurchaseCharge{a, b}, nil, nil, nil)
	rejected(t, err, CodeChargeScopeUnresolved, "/supplier_offer/offer.2/lines/0/included_obligation_ids/0")

	goods := fixtureFee()
	goods.ObligationID = "goods.1"
	_, err = PlanCharges([]PurchaseCharge{fixturePurchase()}, []Fee{goods}, nil, nil)
	rejected(t, err, CodeChargeDoubleCount, "/logistics_offer/log.1/fees/0/obligation_id")

	bears := fixturePurchase()
	bears.SupplierBears = []string{"loading.1"}
	_, err = PlanCharges([]PurchaseCharge{bears}, nil, nil, nil)
	rejected(t, err, CodeCostInputNotReady, "/supplier_offer/offer.1/lines/0/included_obligation_ids")

	var chain []Fee
	for i := range MaxInclusionDepth + 2 {
		f := fixtureFee()
		f.Path, f.ID, f.ComponentID, f.ObligationID = fmt.Sprintf("/fees/%d", i), fmt.Sprintf("F%d", i), fmt.Sprintf("fee.%d", i), fmt.Sprintf("o.%d", i)
		if i > 0 {
			f.IncludedIn = fmt.Sprintf("F%d", i-1)
		}
		chain = append(chain, f)
	}
	_, err = PlanCharges(nil, chain, nil, nil)
	rejected(t, err, CodeCalculationLimitExceeded, "/fees/9/included_in_component_ref")
	plan, err := PlanCharges(nil, chain[:MaxInclusionDepth+1], nil, nil)
	ok(t, err)
	if status(t, plan, "fee.0") != Payable || status(t, plan, "fee.8") != Included {
		t.Fatalf("chain of 8: %+v", plan.Decisions)
	}
}

// T02 §16.2: a customer-borne charge is excluded from the exporter's cost, never priced at zero; a
// customer-borne charge bundled into another price, or a customer-borne obligation inside a
// purchase price, keeps the result not comparable; a charge contradicting the sale scope's bearer
// is a difference too.
func TestCustomerBorneCharges(t *testing.T) {
	duty := fixtureFee()
	duty.Path, duty.ID, duty.ComponentID, duty.ObligationID, duty.FeeType, duty.Party = "/logistics_offer/log.1/fees/1", "F2", "fee.2", "duty.1", "duty@1", Buyer
	sale := append([]Obligation{{Path: "/sales_trade_scope/obligations/1", ID: "duty.1", FeeType: "duty@1", Bearer: Buyer}}, saleFreight...)
	plan, err := PlanCharges([]PurchaseCharge{fixturePurchase()}, []Fee{fixtureFee(), duty}, sale, requiredFreight)
	ok(t, err)
	if status(t, plan, "fee.2") != CustomerBorne || len(plan.Differences) != 0 {
		t.Fatalf("plan = %+v", plan)
	}

	bundled := duty
	bundled.IncludedIn = "purchase.1"
	plan, err = PlanCharges([]PurchaseCharge{fixturePurchase()}, []Fee{fixtureFee(), bundled}, sale, requiredFreight)
	ok(t, err)
	if status(t, plan, "fee.2") != CustomerBorne || len(plan.Differences) != 1 || plan.Differences[0].Path != "/logistics_offer/log.1/fees/1" {
		t.Fatalf("bundled: %+v", plan)
	}

	inPrice := fixturePurchase()
	inPrice.Includes = []string{"duty.1"}
	plan, err = PlanCharges([]PurchaseCharge{inPrice}, []Fee{fixtureFee()}, sale, requiredFreight)
	ok(t, err)
	if len(plan.Differences) != 1 || plan.Differences[0].Path != "/sales_trade_scope/obligations/1" {
		t.Fatalf("in price: %+v", plan.Differences)
	}

	wrongParty := fixtureFee()
	wrongParty.Party = Buyer
	_, err = PlanCharges([]PurchaseCharge{fixturePurchase()}, []Fee{wrongParty}, saleFreight, requiredFreight)
	rejected(t, err, CodeCostInputNotReady, "/sales_trade_scope/obligations/0") // the seller's freight is uncovered
	plan, err = PlanCharges([]PurchaseCharge{fixturePurchase()}, []Fee{wrongParty}, nil, requiredFreight)
	ok(t, err)
	if status(t, plan, "fee.1") != CustomerBorne {
		t.Fatalf("plan = %+v", plan)
	}
}

// Verify-time checks of a logistics offer's fees.
func TestCheckFees(t *testing.T) {
	ok(t, CheckFees([]Fee{fixtureFee()}))
	dup := fixtureFee()
	dup.Path, dup.ObligationID = "/logistics_offer/log.1/fees/1", "other.1"
	rejected(t, CheckFees([]Fee{fixtureFee(), dup}), kernel.CodeDuplicateLineID, "/logistics_offer/log.1/fees/1/id")
	noParty := fixtureFee()
	noParty.Party = ""
	rejected(t, CheckFees([]Fee{noParty}), kernel.CodeSchemaInvalid, "/logistics_offer/log.1/fees/0/responsible_party")
	zeroWeights := fixtureFee() // Python test_zero_explicit_allocation_weights_are_rejected
	zeroWeights.Allocation = AllocationRule{Path: "/logistics_offer/log.1/fees/0/allocation", Basis: BasisExplicit, Weights: []ExplicitWeight{{LineID: "L1"}}}
	rejected(t, CheckFees([]Fee{zeroWeights}), CodeAllocationInvalid, "/logistics_offer/log.1/fees/0/allocation/weights")
	byWeight := fixtureFee() // Python test_weight_allocation_requires_measurement
	byWeight.Allocation = AllocationRule{Path: "/logistics_offer/log.1/fees/0/allocation", Basis: BasisWeight, Targets: []string{"L1"}}
	rejected(t, CheckFees([]Fee{byWeight}), CodeCostInputNotReady, "/logistics_offer/log.1/fees/0/allocation/measurement_ref")
}
