package calc

import (
	"testing"
)

// Python test_t03_a27_cost_scope.py::test_complete_freeze_records_server_scope_binding (PYT-0265):
// the binding names the RFQ revision and binds the line to its RFQ line, in the cost unit.
func TestBindScopeRecordsTheCompleteScope(t *testing.T) {
	b, err := BindScope(quote(), facts(t))
	ok(t, err)
	if b.Rfq != (RfqRef{ID: "rfq.1", RowVersion: 2}) || b.PricingPolicy != policyRef || b.Currency != "USD" || b.CostBasis != CostBasisNominalDirect ||
		b.TradeTerms != tradeTerms || b.PaymentTerms != paymentTerms || b.NamedPlace != port || b.Delivery != delivery {
		t.Fatalf("binding %+v", b)
	}
	if len(b.Lines) != 1 {
		t.Fatalf("lines %+v", b.Lines)
	}
	l := b.Lines[0]
	if l.LineID != "L1" || l.RfqLineID != "L1" || l.SkuID != "sku.1" || l.SpecRevision != 1 || !l.NormalizedQuantity.Equal(dec("120")) ||
		l.Unit != "PCS" || l.CostUnit != "PCS" || !l.Conversion.IsZero() {
		t.Fatalf("line binding %+v", l)
	}
}

// TRD-INV-006: what a quote may and may not include relative to the cost snapshot, the RFQ and the
// customer's trading profile.
func TestBindScopeRefusals(t *testing.T) {
	box10 := func(q *Quote, factor string) {
		q.Lines[0].Quantity, q.Lines[0].Unit = dec("10"), box
		q.Lines[0].Conversion = &Conversion{Ref: Ref{Kind: "unit_conversion", ID: "box.to.pcs", Revision: 1}, SkuID: "sku.1", SpecRevision: 1,
			From: "BOX", To: "PCS", Factor: dec(factor)}
	}
	for _, c := range []struct {
		name   string
		source string // the Python test or archive rule it ports
		edit   func(*Quote, *Facts)
		code   string
		path   string
	}{
		{"quantity below the RFQ and the costing", "test_quantity_must_match_full_cost_and_rfq_scope (PYT-0262)",
			func(q *Quote, _ *Facts) { q.Lines[0].Quantity = dec("60") }, CodeComparisonBasisMismatch, "/args/data/lines/0/quantity"},
		{"delivery changed after the cost freeze", "test_delivery_cannot_be_changed_after_cost_freeze (PYT-0263)",
			func(q *Quote, _ *Facts) { q.Delivery.Date = "2026-11-14" }, CodeComparisonBasisMismatch, "/args/data/delivery"},
		{"other trade terms (Go has no trade_scope; the terms carry the sale)", "test_scope_cannot_switch_from_export_sale_to_purchase (PYT-0264)",
			func(q *Quote, _ *Facts) { q.TradeTerms.ID = "terms.purchase" }, CodeComparisonBasisMismatch, "/args/data/trade_terms_ref"},
		{"no RFQ", "test_missing_rfq_is_not_a_way_to_skip_scope_checks (PYT-0266), test_public_missing_rfq_fails_closed (PYT-0293)",
			func(q *Quote, _ *Facts) { q.Rfq = RfqRef{} }, "DRAFT_NOT_READY", "/args/data/rfq_ref"},
		{"no RFQ line mapping", "test_missing_line_mapping_is_rejected (PYT-0267)",
			func(q *Quote, _ *Facts) { q.Lines[0].RfqLineID = "" }, "DRAFT_NOT_READY", "/args/data/lines/0/rfq_line_id"},
		{"unknown RFQ line", "test_unknown_and_duplicate_rfq_line_mapping_are_rejected (PYT-0268)",
			func(q *Quote, _ *Facts) { q.Lines[0].RfqLineID = "unknown" }, CodeComparisonBasisMismatch, "/args/data/lines/0/rfq_line_id"},
		{"one RFQ line split over two lines", "test_same_rfq_line_cannot_be_split_across_two_sales_lines (PYT-0269)",
			func(q *Quote, _ *Facts) {
				l := line()
				l.Path, l.LineID = "/args/data/lines/1", "L2"
				q.Lines = append(q.Lines, l)
			}, CodeComparisonBasisMismatch, "/args/data/lines/1/rfq_line_id"},
		{"repeated line ID", "test_duplicate_sales_identity_is_rejected (PYT-0270); test_multiple_lines_round_before_aggregation_and_reject_duplicate_ids (PYT-0450)",
			func(q *Quote, _ *Facts) {
				l := line()
				l.Path, l.UnitPrice = "/args/data/lines/1", dec("1")
				q.Lines = append(q.Lines, l)
			}, "DUPLICATE_LINE_ID", "/args/data/lines/1/line_id"},
		{"another RFQ revision", "test_wrong_rfq_revision_is_rejected (PYT-0271)",
			func(q *Quote, _ *Facts) { q.Rfq.RowVersion = 1 }, CodeComparisonBasisMismatch, "/args/data/rfq_ref"},
		{"another customer", "test_wrong_customer_cannot_adopt_the_snapshot (PYT-0272)",
			func(q *Quote, _ *Facts) { q.Customer = "supplier.1" }, "REFERENCE_INVALID", "/args/data/customer_ref"},
		{"another contact", "test_wrong_contact_revision_is_rejected (PYT-0273), the identity part",
			func(q *Quote, _ *Facts) { q.Contact = "contact.2" }, "REFERENCE_INVALID", "/args/data/contact_ref"},
		{"trade terms with an equal label", "test_different_trade_terms_cannot_use_equal_labels (PYT-0274)",
			func(q *Quote, f *Facts) {
				q.TradeTerms.ID, f.Profile.TradeTerms.ID = "other.same-label", "other.same-label"
			},
			CodeComparisonBasisMismatch, "/args/data/trade_terms_ref"},
		{"other payment terms", "test_different_payment_terms_require_recosting (PYT-0275)",
			func(q *Quote, f *Facts) {
				q.PaymentTerms.ID, f.Profile.PaymentTerms.ID = "other.same-schedule", "other.same-schedule"
			},
			CodeComparisonBasisMismatch, "/args/data/payment_terms_ref"},
		{"delivery milestone", "test_delivery_milestone_is_part_of_the_comparison (PYT-0276)",
			func(q *Quote, _ *Facts) { q.Delivery.Milestone = "arrival" }, CodeComparisonBasisMismatch, "/args/data/delivery"},
		{"delivery time zone", "test_delivery_timezone_is_part_of_the_comparison (PYT-0277)",
			func(q *Quote, _ *Facts) { q.Delivery.TimeZone = "UTC" }, CodeComparisonBasisMismatch, "/args/data/delivery"},
		{"named place", "test_named_place_must_match_exact_cost_reference (PYT-0278)",
			func(q *Quote, _ *Facts) { q.NamedPlace.ID = "origin.test" }, CodeComparisonBasisMismatch, "/args/data/named_place_ref"},
		{"specification revision", "test_sku_profile_revision_must_match_cost_manifest (PYT-0279)",
			func(q *Quote, _ *Facts) { q.Lines[0].SpecRevision = 2 }, CodeComparisonBasisMismatch, "/args/data/lines/0/sku_id"},
		{"unit precision before any rounding", "test_unit_precision_is_checked_before_amount_rounding (PYT-0280)",
			func(q *Quote, _ *Facts) { q.Lines[0].Quantity = dec("120.5") }, "INVALID_KNOWN_VALUE", "/args/data/lines/0/quantity"},
		{"another unit without a conversion", "test_unknown_unit_cannot_be_guessed_as_a_package (PYT-0282)",
			func(q *Quote, _ *Facts) { q.Lines[0].Quantity, q.Lines[0].Unit = dec("10"), box }, CodeDependencyNotSupported, "/args/data/lines/0/conversion_ref"},
		{"a wrong conversion factor", "test_wrong_conversion_factor_does_not_allow_partial_cost_scope (PYT-0283)",
			func(q *Quote, _ *Facts) { box10(q, "10") }, CodeComparisonBasisMismatch, "/args/data/lines/0/quantity"},
		{"a redundant conversion", "test_redundant_conversion_is_rejected (PYT-0284)",
			func(q *Quote, _ *Facts) {
				q.Lines[0].Conversion = &Conversion{Ref: Ref{Kind: "unit_conversion", ID: "redundant", Revision: 1}, SkuID: "sku.1", SpecRevision: 1, From: "PCS", To: "PCS", Factor: dec("1")}
			}, "REFERENCE_INVALID", "/args/data/lines/0/conversion_ref"},
		{"a conversion of another SKU", "t03_cost_scope.bind conversion scope",
			func(q *Quote, _ *Facts) { box10(q, "12"); q.Lines[0].Conversion.SkuID = "sku.2" }, "REFERENCE_INVALID", "/args/data/lines/0/conversion_ref"},
		{"a conversion with no positive factor", "t03_cost_scope.bind factor > 0",
			func(q *Quote, _ *Facts) { box10(q, "0") }, "REFERENCE_INVALID", "/args/data/lines/0/conversion_ref"},
		{"a what_if costing (Python: COST_SNAPSHOT_INELIGIBLE)", "test_actual_context_is_required_even_with_matching_manifest (PYT-0290); TRD-INV-006",
			func(_ *Quote, f *Facts) { f.Cost.ContextKind = ContextWhatIf }, CodeCostSnapshotNotEligible, "/args/data/cost_snapshot_ref"},
		{"another cost basis", "t03_cost_scope.bind cost_basis",
			func(_ *Quote, f *Facts) { f.Cost.CostBasis = "full_cost" }, CodeComparisonBasisMismatch, "/args/data/cost_snapshot_ref"},
		{"another pricing policy", "t03_cost_scope.bind pricing_policy_ref",
			func(_ *Quote, f *Facts) { f.Policy.Ref.Revision = 2 }, CodeComparisonBasisMismatch, "/args/data/pricing_policy_ref"},
		{"the policy's calculation currency is not the snapshot's", "_server_calculation snapshot currency; T03 §5.2",
			func(_ *Quote, f *Facts) { f.Policy.CalculationCurrency = eur }, CodeComparisonBasisMismatch, "/args/data/pricing_policy_ref"},
		{"not the trading profile's trade terms",
			"test_t03_a29_trade_parties.py test_profile_must_match_customer_contact_terms_and_supported_address_role (PYT-0349)",
			func(_ *Quote, f *Facts) { f.Profile.TradeTerms.ID = "terms.other" }, CodeComparisonBasisMismatch, "/args/data/trade_terms_ref"},
		{"not the trading profile's payment terms",
			"test_t03_a29_trade_parties.py test_profile_must_match_customer_contact_terms_and_supported_address_role (PYT-0349)",
			func(_ *Quote, f *Facts) { f.Profile.PaymentTerms.Revision = 2 }, CodeComparisonBasisMismatch, "/args/data/payment_terms_ref"},
		{"the costing misses an RFQ line", "t03_cost_scope.bind set(covered) == set(demand)",
			func(_ *Quote, f *Facts) {
				f.Rfq.Lines = append(f.Rfq.Lines, RfqLine{LineID: "L2", SkuID: "sku.1", Quantity: dec("1"), UOM: "PCS"})
			}, CodeComparisonBasisMismatch, "/args/data/cost_snapshot_ref"},
		{"the RFQ line asks another SKU", "t03_cost_scope.bind sku_id",
			func(_ *Quote, f *Facts) { f.Rfq.Lines[0].SkuID = "sku.2" }, CodeComparisonBasisMismatch, "/args/data/lines/0/sku_id"},
		{"the costing's unit is not the RFQ's", "t03_cost_scope.bind standard unit",
			func(_ *Quote, f *Facts) { f.Rfq.Lines[0].UOM = "DOZEN" }, CodeDependencyNotSupported, "/args/data/lines/0/unit_ref"},
		{"more lines than an RFQ has", "T03 §10: at most 200 lines",
			func(q *Quote, _ *Facts) { q.Lines = make([]Line, MaxLines+1) }, CodeCalculationLimitExceeded, "/args/data/lines"},
		{"no lines", "t03_cost_scope.bind 0 < len(lines)",
			func(q *Quote, _ *Facts) { q.Lines = nil }, "DRAFT_NOT_READY", "/args/data/lines"},
		{"no delivery", "t03_cost_scope.bind FIELDS",
			func(q *Quote, _ *Facts) { q.Delivery = Delivery{} }, "DRAFT_NOT_READY", "/args/data/delivery"},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, f := quote(), facts(t)
			c.edit(&q, &f)
			_, err := BindScope(q, f)
			rejected(t, err, c.code, c.path)
		})
	}
}

// Python test_explicit_conversion_preserves_normalized_scope (PYT-0281): 10 BOX at 96 USD with a
// published conversion of 12 PCS per BOX bind as 120 PCS and price 960.00 USD.
func TestExplicitConversionPreservesTheNormalizedScope(t *testing.T) {
	q, f := quote(), facts(t)
	conv := Ref{Kind: "unit_conversion", ID: "box.to.pcs", Revision: 1}
	q.Lines[0].Quantity, q.Lines[0].Unit, q.Lines[0].UnitPrice = dec("10"), box, dec("96")
	q.Lines[0].Conversion = &Conversion{Ref: conv, SkuID: "sku.1", SpecRevision: 1, From: "BOX", To: "PCS", Factor: dec("12")}
	calc, err := Calculate(q, f)
	ok(t, err)
	l := calc.Scope.Lines[0]
	if !l.NormalizedQuantity.Equal(dec("120")) || !l.Quantity.Equal(dec("10")) || l.Unit != "BOX" || l.Conversion != conv {
		t.Fatalf("binding %+v", l)
	}
	if calc.Totals.Net.MinorString() != "96000" {
		t.Fatalf("net %s", calc.Totals.Net)
	}
}

// BC-AT04: red S quoted as 2 DOZEN at 96.00 USD per DOZEN binds 24 PCS and prices 192.00 USD; a
// stated base quantity of 2 is BASE_QUANTITY_MISMATCH.
func TestBaseQuantityIsTheConvertedQuantity(t *testing.T) {
	q, f := quote(), facts(t)
	q.Lines[0].Quantity, q.Lines[0].Unit, q.Lines[0].UnitPrice = dec("2"), dozen, dec("96")
	q.Lines[0].Conversion = &Conversion{Ref: Ref{Kind: "unit_conversion", ID: "dozen.to.pcs", Revision: 1}, SkuID: "sku.1", SpecRevision: 1,
		From: "DOZEN", To: "PCS", Factor: dec("12")}
	q.Lines[0].BaseQuantity = decp("24")
	f.Rfq.Lines[0].Quantity, f.Cost.Lines[0].Quantity = dec("24"), dec("24")
	calc, err := Calculate(q, f)
	ok(t, err)
	if !calc.Scope.Lines[0].NormalizedQuantity.Equal(dec("24")) || calc.Lines[0].Net.Amount() != "192.00" {
		t.Fatalf("got %s PCS, %s", calc.Scope.Lines[0].NormalizedQuantity, calc.Lines[0].Net)
	}
	q.Lines[0].BaseQuantity = decp("2")
	_, err = BindScope(q, f)
	rejected(t, err, CodeBaseQuantityMismatch, "/args/data/lines/0/base_quantity")
}

// Python test_numeric_equivalent_quantity_has_one_normalized_scope (PYT-0285): 120.000000 is 120,
// and both bind the same scope.
func TestNumericEquivalentQuantitiesBindOneScope(t *testing.T) {
	q, f := quote(), facts(t)
	plain, err := BindScope(q, f)
	ok(t, err)
	q.Lines[0].Quantity = dec("120.000000")
	padded, err := BindScope(q, f)
	ok(t, err)
	if !plain.Equal(padded) {
		t.Fatalf("%+v != %+v", plain, padded)
	}
}

// Python test_complete_multiline_and_missing_line (PYT-0287): two RFQ lines of 60 each; a quote of
// one of them is partial, a quote of both binds in line ID order.
func TestTheQuoteCoversTheCompleteRfq(t *testing.T) {
	q, f := quote(), facts(t)
	q.Lines[0].Quantity = dec("60")
	other := line()
	other.Path, other.LineID, other.RfqLineID, other.Quantity = "/args/data/lines/1", "sale.B", "L2", dec("60")
	q.Lines = append(q.Lines, other)
	recost(q, &f)

	partial := q
	partial.Lines = q.Lines[:1]
	_, err := BindScope(partial, f)
	rejected(t, err, CodeComparisonBasisMismatch, "/args/data/lines")

	b, err := BindScope(q, f)
	ok(t, err)
	if len(b.Lines) != 2 || b.Lines[0].LineID != "L1" || b.Lines[1].LineID != "sale.B" || b.Lines[1].RfqLineID != "L2" {
		t.Fatalf("lines %+v", b.Lines)
	}
}

// Python test_t03_a27_cost_scope.py test_current_scope_failure_does_not_rewrite_committed_quote_history
// (PYT-0296), the rule quote_ready applies: binding the current RFQ again, a changed RFQ line fails it
// (COMPARISON_BASIS_MISMATCH) or yields another binding than the frozen one. (Activation does not
// rebind: RfqGuard v1 rechecks the RFQ's row version there.)
func TestRebindingTheCurrentRfqDetectsAChange(t *testing.T) {
	q, f := quote(), facts(t)
	frozen, err := BindScope(q, f)
	ok(t, err)
	f.Rfq.Lines[0].Quantity = dec("60")
	_, err = BindScope(q, f)
	rejected(t, err, CodeComparisonBasisMismatch, "/args/data/lines/0/quantity")

	f = facts(t)
	q.Lines[0].Quantity, f.Rfq.Lines[0].Quantity, f.Cost.Lines[0].Quantity = dec("60"), dec("60"), dec("60")
	current, err := BindScope(q, f)
	ok(t, err)
	if frozen.Equal(current) {
		t.Fatal("a binding of other quantities is another binding")
	}
}
