package calc

import (
	"slices"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Python test_freeze_ignores_client_calculation_and_persists_server_result (PYT-0442) and
// t03_candidate/verify.py "960/660: net, payable, profit, ratio": the BRC-001 baseline, 120 PCS ×
// 8.00 USD on the 660.00 USD snapshot. Calculate takes no client amount at all.
func TestCalculateTheBaseline(t *testing.T) {
	c, err := Calculate(quote(), facts(t))
	ok(t, err)
	switch {
	case c.Totals.Net.MinorString() != "96000" || c.Totals.Tax.MinorString() != "0" || c.Totals.Payable.MinorString() != "96000":
		t.Fatalf("totals %+v", c.Totals)
	case c.Cost.MinorString() != "66000" || c.Margin.Profit.MinorString() != "30000" || c.Margin.Verdict != MarginPass || !c.Margin.Rate.Equal(dec("0.3125")):
		t.Fatalf("margin %+v", c.Margin)
	case c.Comparison.Currency != usd || c.Comparison.FX != nil || c.Comparison.Revenue.MinorString() != "96000":
		t.Fatalf("comparison %+v", c.Comparison)
	case len(c.Payments) != 1 || c.Payments[0].StageID != "P1" || c.Payments[0].Amount.MinorString() != "96000":
		t.Fatalf("payments %+v", c.Payments)
	case c.CostBasis != CostBasisNominalDirect || !slices.Equal(c.ExcludedCostCategories, []string{"imputed_financing_cost", "expected_credit_loss", "unrealized_fx_hedge"}):
		t.Fatalf("cost basis %s %v", c.CostBasis, c.ExcludedCostCategories)
	case len(c.Lines) != 1 || c.Lines[0].ID != "L1" || len(c.Scope.Lines) != 1 || len(c.Terms.Taxes) != 1 || c.Currency != usd:
		t.Fatalf("calculation %+v", c)
	}
}

// End-to-end vectors of the Python freeze tests: what the stored calculation must say.
func TestCalculateVectors(t *testing.T) {
	inc13 := profile("sales.tax.inc", "0.13", TaxInclusive)
	exc13 := profile("sales.tax.13", "0.13", TaxExclusive)
	eurQuote := func(q *Quote, f *Facts) { q.Currency, f.FX = eur, eurRate() }
	for _, c := range []struct {
		name, source                        string
		edit                                func(*Quote, *Facts)
		net, tax, payable, revenue, verdict string
		payments                            []string
	}{
		{"a EUR quote compares in USD", "test_sales_fx_maps_eur_quote_to_usd_comparison_currency (PYT-0448)",
			eurQuote, "96000", "0", "96000", "105600", MarginPass, []string{"96000"}},
		{"a separate sales charge is added once", "test_supported_sales_charge_is_added_once_with_independent_obligation (PYT-0449); test_separate_fixed_sales_charge_is_added_once (PYT-0302)",
			func(q *Quote, _ *Facts) { q.Charges = []Charge{salesCharge("sales.freight", InclusionSeparate, "10")} },
			"97000", "0", "97000", "97000", MarginPass, []string{"97000"}},
		{"an included fee is bound but not added again", "test_included_fee_is_explicitly_bound_but_not_added_again (PYT-0303)",
			func(q *Quote, _ *Facts) { q.Charges = []Charge{included("sales.freight", "10")} },
			"96000", "0", "96000", "96000", MarginPass, []string{"96000"}},
		{"exclusive 0.13 sales tax", "test_exclusive_sales_tax_uses_dedicated_reviewed_application (PYT-0319)",
			func(q *Quote, _ *Facts) { q.Lines[0].Tax = goodsTax(exc13) },
			"96000", "12480", "108480", "96000", MarginPass, []string{"108480"}},
		{"inclusive 0.13 sales tax keeps the payable and judges the margin on the net", "test_inclusive_sales_tax_preserves_payable_and_margin_base (PYT-0320); test_inclusive_tax_is_split_from_line_payable_before_margin (PYT-0451)",
			func(q *Quote, _ *Facts) { q.Lines[0].Tax = goodsTax(inc13) },
			"84956", "11044", "96000", "84956", MarginPass, []string{"96000"}},
		{"a separate exclusive fee has its own tax split", "test_separate_exclusive_fee_has_its_own_tax_split (PYT-0321)",
			func(q *Quote, _ *Facts) {
				ch := salesCharge("sales.freight", InclusionSeparate, "10")
				ch.Tax = feeTax("sales.freight", exc13)
				q.Charges = []Charge{ch}
			}, "97000", "130", "97130", "97000", MarginPass, []string{"97130"}},
		{"a separate inclusive fee has its own tax split", "test_separate_inclusive_fee_has_its_own_tax_split (PYT-0322)",
			func(q *Quote, _ *Facts) {
				ch := salesCharge("sales.freight", InclusionSeparate, "11.30")
				ch.Tax = feeTax("sales.freight", inc13)
				q.Charges = []Charge{ch}
			}, "97000", "130", "97130", "97000", MarginPass, []string{"97130"}},
		{"an included taxed fee adds no second tax", "test_included_taxed_fee_does_not_add_a_second_tax_amount (PYT-0323)",
			func(q *Quote, _ *Facts) {
				q.Lines[0].Tax = goodsTax(inc13)
				ch := included("sales.freight", "11.30")
				ch.Tax = feeTax("sales.freight", inc13)
				q.Charges = []Charge{ch}
			}, "84956", "11044", "96000", "84956", MarginPass, []string{"96000"}},
		{"a EUR quote with an inclusive fee keeps payment and margin units apart", "test_foreign_currency_taxed_fee_composition_keeps_payment_and_margin_units (PYT-0347)",
			func(q *Quote, f *Facts) {
				eurQuote(q, f)
				f.FX.ObservedAt = f.FX.ObservedAt.Add(time.Second) // Python observed it at 08:00:01
				ch := salesCharge("sales.freight", InclusionSeparate, "11.30")
				ch.Tax = feeTax("sales.freight", inc13)
				q.Charges = []Charge{ch}
			}, "97000", "130", "97130", "106700", MarginPass, []string{"97130"}},
		{"7.00 per piece needs a margin exception", "T03 §5.3 (120 × 7, cost 660, minimum 0.25); BC-AT06",
			func(q *Quote, f *Facts) { q.Lines[0].UnitPrice, f.Policy.MinimumMargin = dec("7"), dec("0.25") },
			"84000", "0", "84000", "84000", MarginRequiresApproval, []string{"84000"}},
		{"7.33333333 per piece rounds to 880.00 and passes 0.25", "t03_candidate/verify.py margin threshold at price 7.33333333",
			func(q *Quote, f *Facts) {
				q.Lines[0].UnitPrice, f.Policy.MinimumMargin = dec("7.33333333"), dec("0.25")
			},
			"88000", "0", "88000", "88000", MarginPass, []string{"88000"}},
		{"7.33325 per piece is 879.99 and does not", "t03_candidate/verify.py margin threshold at price 7.33325",
			func(q *Quote, f *Facts) { q.Lines[0].UnitPrice, f.Policy.MinimumMargin = dec("7.33325"), dec("0.25") },
			"87999", "0", "87999", "87999", MarginRequiresApproval, []string{"87999"}},
		{"30/70 payment terms", "T03 §5.3 (288.00 + 672.00)",
			func(q *Quote, f *Facts) {
				q.PaymentTerms = Ref{Kind: "payment_terms", ID: "payment.30-70", Revision: 1}
				f.Cost.PaymentTerms, f.Profile.PaymentTerms = q.PaymentTerms, q.PaymentTerms
				f.Stages = stages("deposit", "30", "balance", "70")
			}, "96000", "0", "96000", "96000", MarginPass, []string{"28800", "67200"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, f := quote(), facts(t)
			c.edit(&q, &f)
			got, err := Calculate(q, f)
			ok(t, err)
			if got.Totals.Net.MinorString() != c.net || got.Totals.Tax.MinorString() != c.tax || got.Totals.Payable.MinorString() != c.payable {
				t.Fatalf("totals %s + %s = %s, want %s + %s = %s", got.Totals.Net.MinorString(), got.Totals.Tax.MinorString(), got.Totals.Payable.MinorString(),
					c.net, c.tax, c.payable)
			}
			if got.Comparison.Revenue.MinorString() != c.revenue || got.Comparison.Revenue.Currency() != usd || got.Margin.Verdict != c.verdict {
				t.Fatalf("comparison %s %s, verdict %s", got.Comparison.Revenue.MinorString(), got.Comparison.Revenue.Currency(), got.Margin.Verdict)
			}
			var payments []string
			for _, p := range got.Payments {
				payments = append(payments, p.Amount.MinorString())
				if p.Amount.Currency() != q.Currency {
					t.Fatalf("the customer pays in %s, the plan says %s", q.Currency, p.Amount.Currency())
				}
			}
			if !slices.Equal(payments, c.payments) {
				t.Fatalf("payments %v, want %v", payments, c.payments)
			}
		})
	}
}

// Python test_payment_tail_uses_remainder_order_and_zero_stage_is_rejected (PYT-0452): 1 × 0.05 USD
// over 33.33/33.33/33.34 is 0.02, 0.01, 0.02; at 0.01 a stage would get 0 and the freeze is refused.
func TestCalculatePaymentTail(t *testing.T) {
	q, f := quote(), facts(t)
	q.Lines[0].Quantity, q.Lines[0].UnitPrice = dec("1"), dec("0.05")
	recost(q, &f)
	f.Stages = stages("P1", "33.33", "P2", "33.33", "P3", "33.34")
	f.Cost.Total, f.Policy.MinimumMargin = minor(t, usd, 1), dec("0")
	got, err := Calculate(q, f)
	ok(t, err)
	if s := amountsOf(got.Payments); s != "2 1 2" {
		t.Fatalf("got %s", s)
	}
	q.Lines[0].UnitPrice = dec("0.01")
	_, err = Calculate(q, f)
	rejected(t, err, CodePaymentTotalMismatch, "/args/data/payment_terms_ref")
}

// T03 §5.3 and TRD-INV-008: two lines of 1 × 0.005 USD are one cent each and two in total, never a
// merged unrounded 0.01 that rounds to one cent.
func TestCalculateRoundsEachLineBeforeSumming(t *testing.T) {
	q, f := quote(), facts(t)
	second := line()
	second.Path, second.LineID, second.RfqLineID = "/args/data/lines/1", "L2", "L2"
	q.Lines = append(q.Lines, second)
	for i := range q.Lines {
		q.Lines[i].Quantity, q.Lines[i].UnitPrice = dec("1"), dec("0.005")
	}
	recost(q, &f)
	f.Cost.Total, f.Policy.MinimumMargin = minor(t, usd, 0), dec("0")
	got, err := Calculate(q, f)
	ok(t, err)
	if got.Lines[0].Payable.MinorString() != "1" || got.Lines[1].Payable.MinorString() != "1" || got.Totals.Payable.MinorString() != "2" {
		t.Fatalf("lines %+v, total %s", got.Lines, got.Totals.Payable)
	}
}

// TRD-INV-007 and T03 §5.2 (reject > requires_approval > pass): a quote below the minimum that also
// breaks a rule is refused, so no margin exception can ever reach it.
func TestARefusalOutranksTheMarginVerdict(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*Quote, *Facts)
		code string
	}{
		{"scope", func(q *Quote, _ *Facts) { q.Lines[0].Quantity = dec("119") }, CodeComparisonBasisMismatch},
		{"tax", func(q *Quote, _ *Facts) { q.Lines[0].Tax.Side = SidePurchase }, CodeTaxPolicyUnconfirmed},
		{"exchange rate", func(q *Quote, _ *Facts) { q.Currency = eur }, CodeFXScopeMismatch},
		{"payment terms", func(_ *Quote, f *Facts) { f.Stages = stages("P1", "99") }, CodePaymentTotalMismatch},
		{"no currency", func(q *Quote, _ *Facts) { q.Currency = kernel.Currency{} }, "DRAFT_NOT_READY"},
		{"a negative cost", func(_ *Quote, f *Facts) { f.Cost.Total = minor(t, usd, -1) }, "INVALID_KNOWN_VALUE"},
		{"a purchase fee as a sales charge", func(q *Quote, _ *Facts) {
			q.Charges = []Charge{salesCharge("freight", InclusionSeparate, "10")}
			q.Charges[0].Fee.Side = FeePurchaseCost
		}, CodeChargeScopeUnresolved},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, f := quote(), facts(t)
			q.Lines[0].UnitPrice, f.Policy.MinimumMargin = dec("7"), dec("0.25")
			c.edit(&q, &f)
			_, err := Calculate(q, f)
			rejected(t, err, c.code, "")
		})
	}
}
