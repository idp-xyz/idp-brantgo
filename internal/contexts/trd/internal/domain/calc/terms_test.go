package calc

import (
	"testing"
)

// withCharges returns quote() with the charges, renumbered in document order.
func withCharges(charges ...Charge) Quote {
	q := quote()
	for i := range charges {
		charges[i].Path = index("/args/data", "charges", i)
	}
	q.Charges = charges
	return q
}

func included(fee, amount string) Charge {
	c := salesCharge(fee, InclusionIncluded, amount)
	c.IncludedLineID = "L1"
	return c
}

// T03 §5.1 and Python t03_sales_terms.bind: tax applicability of lines and charges, and what a sales
// charge may be. Codes are Python's except where noted.
func TestBindTermsRefusals(t *testing.T) {
	tax13 := profile("sales.tax.13", "0.13", TaxExclusive)
	for _, c := range []struct {
		name   string
		source string
		quote  func() Quote
		code   string
		path   string
	}{
		{"a purchase tax application, even at rate 0", "test_purchase_tax_application_cannot_authorize_sales_even_at_zero_rate (PYT-0298)",
			func() Quote { q := quote(); q.Lines[0].Tax.Side = SidePurchase; return q }, CodeTaxPolicyUnconfirmed, "/args/data/lines/0/tax_application_ref"},
		{"a separate fee claimed as included", "test_payload_cannot_claim_separate_fee_is_included (PYT-0299)",
			func() Quote {
				c := salesCharge("sales.freight", InclusionSeparate, "10")
				c.Inclusion, c.IncludedLineID = InclusionIncluded, "L1"
				return withCharges(c)
			}, CodeChargeScopeUnresolved, "/args/data/charges/0/inclusion"},
		{"one charge ID for two obligations", "test_duplicate_charge_identity_cannot_use_distinct_obligations (PYT-0300)",
			func() Quote {
				a, b := salesCharge("sales.first", InclusionSeparate, "10"), salesCharge("sales.second", InclusionSeparate, "10")
				b.ChargeID = a.ChargeID
				return withCharges(a, b)
			}, CodeChargeScopeUnresolved, "/args/data/charges/1/charge_id"},
		{"one obligation under two charge IDs (Python: CHARGE_SCOPE_UNRESOLVED)", "test_duplicate_key_is_rejected_even_when_charge_id_changes (PYT-0304); catalog CHARGE_DOUBLE_COUNT",
			func() Quote {
				a, b := salesCharge("sales.one", InclusionSeparate, "10"), salesCharge("sales.two", InclusionSeparate, "10")
				b.ObligationKey = a.ObligationKey
				return withCharges(a, b)
			}, CodeChargeDoubleCount, "/args/data/charges/1/obligation_key"},
		{"one fee category under a renamed obligation", "test_reusing_fee_under_renamed_key_remains_unsupported (PYT-0305)",
			func() Quote {
				a, b := salesCharge("sales.freight", InclusionSeparate, "10"), salesCharge("sales.freight", InclusionSeparate, "10")
				b.ChargeID, b.ObligationKey = "renamed.charge", "renamed.obligation"
				return withCharges(a, b)
			}, CodeChargeScopeUnresolved, "/args/data/charges/1/fee_category_ref"},
		{"no obligation key", "test_missing_obligation_key_cannot_fall_back_to_charge_id (PYT-0306)",
			func() Quote {
				c := salesCharge("sales.freight", InclusionSeparate, "10")
				c.ObligationKey = ""
				return withCharges(c)
			},
			CodeChargeScopeUnresolved, "/args/data/charges/0/obligation_key"},
		{"no inclusion", "test_missing_inclusion_cannot_default_to_separate (PYT-0307)",
			func() Quote {
				c := salesCharge("sales.freight", InclusionSeparate, "10")
				c.Inclusion = ""
				return withCharges(c)
			},
			CodeChargeScopeUnresolved, "/args/data/charges/0/inclusion"},
		{"a purchase fee as seller income", "test_purchase_fee_cannot_be_recognized_as_seller_income (PYT-0308)",
			func() Quote {
				c := salesCharge("freight", InclusionSeparate, "10")
				c.Fee.Side = FeePurchaseCost
				return withCharges(c)
			},
			CodeChargeScopeUnresolved, "/args/data/charges/0/fee_category_ref"},
		{"a separate fee naming a line", "test_separate_fee_cannot_carry_an_included_line (PYT-0309)",
			func() Quote {
				c := salesCharge("sales.freight", InclusionSeparate, "10")
				c.IncludedLineID = "L1"
				return withCharges(c)
			},
			CodeChargeScopeUnresolved, "/args/data/charges/0/included_line_id"},
		{"an included fee naming no line of the quote", "test_included_fee_requires_existing_sales_line (PYT-0310)",
			func() Quote {
				c := included("sales.freight", "10")
				c.IncludedLineID = "missing"
				return withCharges(c)
			},
			CodeChargeScopeUnresolved, "/args/data/charges/0/included_line_id"},
		{"an included fee above the unrounded line base", "test_included_fee_cannot_exceed_covered_unrounded_line_base (PYT-0311)",
			func() Quote { return withCharges(included("sales.freight", "960.01")) }, CodeChargeScopeUnresolved, "/args/data/charges/0/amount"},
		{"an included fee hiding another tax profile", "test_included_fee_cannot_hide_different_tax_profile (PYT-0312)",
			func() Quote {
				c := included("sales.freight", "10")
				c.Tax = feeTax("sales.freight", tax13)
				return withCharges(c)
			},
			CodeChargeScopeUnresolved, "/args/data/charges/0/tax_application_ref"},
		{"a zero charge", "test_zero_charge_amount_is_rejected (PYT-0313)",
			func() Quote { return withCharges(salesCharge("sales.freight", InclusionSeparate, "0")) }, "INVALID_KNOWN_VALUE", "/args/data/charges/0/amount"},
		{"a charge without evidence", "test_charge_requires_current_verified_source_evidence (PYT-0314), the presence part",
			func() Quote {
				c := salesCharge("sales.freight", InclusionSeparate, "10")
				c.Evidence = nil
				return withCharges(c)
			},
			CodeChargeScopeUnresolved, "/args/data/charges/0/evidence"},
		{"evidence without a locator", "t03_sales_terms.bind evidence locator",
			func() Quote {
				c := salesCharge("sales.freight", InclusionSeparate, "10")
				c.Evidence[0].Locator = " "
				return withCharges(c)
			}, "REFERENCE_INVALID", "/args/data/charges/0/evidence/0"},
		{"a goods tax application for a fee", "test_goods_sales_tax_cannot_be_reused_for_a_fee_subject (PYT-0315)",
			func() Quote {
				c := salesCharge("sales.freight", InclusionSeparate, "10")
				c.Tax = goodsTax(zeroTax)
				return withCharges(c)
			},
			CodeTaxPolicyUnconfirmed, "/args/data/charges/0/tax_application_ref"},
		{"a purchase tax application for a charge", "test_purchase_application_cannot_authorize_sales_charge (PYT-0318)",
			func() Quote {
				c := salesCharge("sales.freight", InclusionSeparate, "10")
				c.Tax.Side = SidePurchase
				return withCharges(c)
			},
			CodeTaxPolicyUnconfirmed, "/args/data/charges/0/tax_application_ref"},
		{"a negative charge", "test_negative_charge_amount_is_rejected (PYT-0324)",
			func() Quote { return withCharges(salesCharge("sales.freight", InclusionSeparate, "-0.01")) }, "INVALID_KNOWN_VALUE", "/args/data/charges/0/amount"},
		{"included charges together above the line base", "test_included_obligations_together_cannot_exceed_line_base (PYT-0325)",
			func() Quote { return withCharges(included("sales.first", "500"), included("sales.second", "500")) },
			CodeChargeScopeUnresolved, "/args/data/charges/1/amount"},
		{"an unmapped additional charge", "test_unmapped_additional_charge_is_rejected_before_freeze (PYT-0447)",
			func() Quote { return withCharges(Charge{ChargeID: "freight", Amount: dec("1")}) }, CodeChargeScopeUnresolved, "/args/data/charges/0/obligation_key"},
		{"a fee the line price includes, charged again", "BC-AT07 (CHARGE_DOUBLE_COUNT); Python has one inclusion check",
			func() Quote {
				c := salesCharge("sales.packing", InclusionIncluded, "10")
				c.Inclusion = InclusionSeparate
				return withCharges(c)
			}, CodeChargeDoubleCount, "/args/data/charges/0/inclusion"},
		{"no tax application on a line", "_server_calculation; T03 §5.1 a missing dependency is never zero tax",
			func() Quote { q := quote(); q.Lines[0].Tax = nil; return q }, CodeTaxPolicyUnconfirmed, "/args/data/lines/0/tax_application_ref"},
		{"a tax application for another SKU", "test_goods_sales_tax_cannot_be_reused_for_a_fee_subject (PYT-0315), the subject rule",
			func() Quote { q := quote(); q.Lines[0].Tax.SubjectID = "sku.2"; return q }, CodeTaxPolicyUnconfirmed, "/args/data/lines/0/tax_application_ref"},
		{"a tax application for another customer", "_bind application customer_ref",
			func() Quote { q := quote(); q.Lines[0].Tax.PartyID = "customer.2"; return q }, CodeTaxPolicyUnconfirmed, "/args/data/lines/0/tax_application_ref"},
		{"an unknown recovery treatment", "_bind recovery_treatment != unknown",
			func() Quote { q := quote(); q.Lines[0].Tax.Profile.Recovery = RecoveryUnknown; return q }, CodeTaxPolicyUnconfirmed, "/args/data/lines/0/tax_application_ref"},
		{"a rate where no tax applies", "_bind not_applicable ⇒ rate 0",
			func() Quote { q := quote(); q.Lines[0].Tax.Profile = profile("t", "0.13", TaxNotApplicable); return q }, CodeTaxPolicyUnconfirmed, "/args/data/lines/0/tax_application_ref"},
		{"a rate above 1", "_bind 0 ≤ rate ≤ 1",
			func() Quote { q := quote(); q.Lines[0].Tax.Profile = profile("t", "1.5", TaxExclusive); return q }, CodeTaxPolicyUnconfirmed, "/args/data/lines/0/tax_application_ref"},
		{"more charges than a quote has", "T03 §10: at most 100 sales charges",
			func() Quote { q := quote(); q.Charges = make([]Charge, MaxCharges+1); return q }, CodeCalculationLimitExceeded, "/args/data/charges"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := BindTerms(c.quote())
			rejected(t, err, c.code, c.path)
		})
	}
}

// Python test_included_fee_is_explicitly_bound_but_not_added_again (PYT-0303) and
// test_public_sales_tax_and_charge_freeze_has_no_client_certificate (PYT-0328), the binding: the line
// and the charge each have a tax binding, the charge binding names the line that includes it.
func TestBindTermsBindsTaxesAndCharges(t *testing.T) {
	q := withCharges(salesCharge("sales.freight", InclusionSeparate, "10"), included("sales.packing", "5"))
	b, err := BindTerms(q)
	ok(t, err)
	roles := []string{"charge:sales.freight.obligation", "charge:sales.packing.obligation", "line:L1"}
	if len(b.Taxes) != len(roles) {
		t.Fatalf("taxes %+v", b.Taxes)
	}
	for i, role := range roles {
		if b.Taxes[i].Role != role || b.Taxes[i].Profile != zeroTax.Ref || b.Taxes[i].PriceBasis != TaxExclusive || !b.Taxes[i].Rate.IsZero() {
			t.Errorf("tax %d: %+v", i, b.Taxes[i])
		}
	}
	if len(b.Charges) != 2 || b.Charges[0].ChargeID != "sales.freight.charge" || b.Charges[1].IncludedLineID != "L1" ||
		b.Charges[1].Inclusion != InclusionIncluded || !b.Charges[0].Amount.Equal(dec("10")) || len(b.Charges[0].Evidence) != 1 {
		t.Fatalf("charges %+v", b.Charges)
	}
	again, err := BindTerms(withCharges(salesCharge("sales.freight", InclusionSeparate, "10"), included("sales.packing", "5")))
	ok(t, err)
	if !b.Equal(again) {
		t.Fatal("the same terms bind the same way")
	}
	other, err := BindTerms(withCharges(salesCharge("sales.freight", InclusionSeparate, "11"), included("sales.packing", "5")))
	ok(t, err)
	if b.Equal(other) {
		t.Fatal("another amount is another binding")
	}
}

// Included charges may use up the line base exactly: 480 + 480 of a 960.00 line.
func TestIncludedChargesMayReachTheLineBase(t *testing.T) {
	_, err := BindTerms(withCharges(included("sales.first", "480"), included("sales.second", "480")))
	ok(t, err)
}
