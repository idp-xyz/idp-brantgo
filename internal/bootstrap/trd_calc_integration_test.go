//go:build integration

package bootstrap_test

import (
	"encoding/json"
	"testing"
	"time"

	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
)

// The server calculation in the freeze (WBS S3-02 wired into S3-01) on PostgreSQL 16: the amounts
// come from TRD's calculation on what INQ, PTY, POL and CST answer, never from the client; the
// commercial document seals them, and the business hash covers them.

// Python test_t03_server_calculation.py test_freeze_ignores_client_calculation_and_persists_server
// _result (PYT-0442; no command has a calculation member, so a client cannot send one),
// test_t03_a27_cost_scope.py test_complete_freeze_records_server_scope_binding (PYT-0265) and
// test_t03_a28_sales_terms.py test_server_sales_binding_is_persisted_for_supported_quote
// (PYT-0301): 120 × 8.00 USD against the 660.00 USD snapshot is 960.00 net and payable, a profit of
// 300.00 above policy.1's minimum of 0.1, one stage of 960.00; the frozen document records the cost
// scope binding (brant.quote-cost-scope/0.1.0 on rfq.1 at its row version, line L1 bound to RFQ
// line L1) and the sales terms binding (brant.quote-sales-terms/0.1.0), the comparison currency the
// costing context names, and the calculation version, all under the business hash.
func TestFrozenRevisionCarriesTheServerCalculation(t *testing.T) {
	q := newTrdWorld(t)
	f := q.frozenQuote("quote.1")
	v := q.getQuote("quote.1")
	c := v.Revisions[0].Calculation
	if c == nil || c.NetMinor != "96000" || c.TaxMinor != "0" || c.PayableMinor != "96000" || c.CostMinor != "66000" || c.ProfitMinor != "30000" ||
		c.MarginRule != "pass" || c.MinimumMargin != "0.1" || c.MarginShortfall != "0" || c.ComparisonCurrencyRef != tref(q.usd) ||
		c.PricingPolicyRef != tref(q.policy) || c.CalculationVersion != "brant.quote-calculation/0.1.0" || len(c.Payments) != 1 ||
		c.Payments[0].StageID != "all" || c.Payments[0].AmountMinor != "96000" {
		t.Fatalf("calculation %+v", c)
	}
	var doc struct {
		Calculation struct {
			CostScope struct {
				Contract string `json:"contract"`
				Rfq      struct {
					ID         string `json:"rfq_id"`
					RowVersion int64  `json:"row_version"`
				} `json:"rfq_ref"`
				NamedPlace struct {
					ID string `json:"id"`
				} `json:"named_place_ref"`
				Lines []struct {
					RfqLineID          string `json:"rfq_line_id"`
					NormalizedQuantity string `json:"normalized_quantity"`
					CostUnit           string `json:"cost_unit"`
				} `json:"line_bindings"`
			} `json:"cost_scope_binding"`
			SalesTerms struct {
				Contract string `json:"contract"`
				Taxes    []struct {
					Role string `json:"role"`
				} `json:"tax_bindings"`
			} `json:"sales_terms_binding"`
			Excluded []string `json:"excluded_cost_categories"`
		} `json:"calculation"`
	}
	q.must(json.Unmarshal([]byte(v.Revisions[0].Commercial), &doc))
	s := doc.Calculation
	if s.CostScope.Contract != "brant.quote-cost-scope/0.1.0" || s.CostScope.Rfq.ID != "rfq.1" || s.CostScope.Rfq.RowVersion != q.rfqVersion ||
		s.CostScope.NamedPlace.ID != "DEHAM" || len(s.CostScope.Lines) != 1 || s.CostScope.Lines[0].RfqLineID != "L1" ||
		s.CostScope.Lines[0].NormalizedQuantity != "120" || s.CostScope.Lines[0].CostUnit != "PCS" ||
		s.SalesTerms.Contract != "brant.quote-sales-terms/0.1.0" || len(s.SalesTerms.Taxes) != 1 || s.SalesTerms.Taxes[0].Role != "line:L1" ||
		len(s.Excluded) != 3 {
		t.Fatalf("bindings %+v", s)
	}
	if sha256Hex(v.Revisions[0].Commercial) != f.BusinessHash {
		t.Fatal("the business hash covers the calculation")
	}
}

// Python test_t03_server_calculation.py test_supported_sales_charge_is_added_once_with_independent
// _obligation (PYT-0449) and test_sales_fx_maps_eur_quote_to_usd_comparison_currency (PYT-0448): a
// separate sales charge of 10 USD of a sales fee category is added once (970.00 net, the charge
// 10.00); a quote of 960.00 EUR is compared in the costing's USD through the quotation rate 1.1 it
// names (1056.00 USD), converted once.
func TestFreezeCalculatesChargesAndSalesFX(t *testing.T) {
	q := newTrdWorld(t)
	fee := q.configure(q.author, q.reviewer, polapi.KindFeeCategory, "freight.sale", 0, q.win.from, time.Time{}, polapi.ConfigData{
		FeeCategory: &polapi.FeeCategory{Name: "销售运费", Category: "freight", Side: polapi.FeeSideSalesCharge, Inclusion: polapi.InclusionSeparate,
			AllocationBasis: polapi.AllocationQuantity, PolicyBasis: "Synthetic reviewed sales charge"}}, true)
	app := q.configure(q.author, q.reviewer, polapi.KindTaxApplication, "tax.freight.sale", 0, q.win.from, time.Time{}, polapi.ConfigData{
		TaxApplication: &polapi.TaxApplication{Name: "sales freight", Side: polapi.TaxSideSales, SubjectKind: polapi.SubjectFeeCategory,
			SubjectID: "freight.sale", TaxProfile: q.tax, Jurisdiction: "CN", BasisNote: "synthetic"}}, true)
	d := q.data()
	d.Charges = []trdapi.Charge{{ChargeID: "freight.sale", FeeCategoryRef: tref(fee), ObligationKey: "freight.sale.1", Amount: "10",
		Inclusion: trdapi.InclusionSeparate, TaxApplicationRef: tref(app), Evidence: q.proof}}
	_, err := q.saveQuote(q.author, "quote.charge", 0, d)
	q.must(err)
	_, err = q.freezeQuote(q.author, "quote.charge", 1, 1)
	q.must(err)
	if c := q.getQuote("quote.charge").Revisions[0].Calculation; c.NetMinor != "97000" || c.PayableMinor != "97000" || c.ProfitMinor != "31000" {
		t.Fatalf("with a sales charge %+v", c)
	}
	var doc struct {
		Calculation struct {
			Charges []struct {
				ID            string `json:"id"`
				ObligationKey string `json:"obligation_key"`
				NetMinor      string `json:"net_minor"`
			} `json:"charges"`
		} `json:"calculation"`
	}
	q.must(json.Unmarshal([]byte(q.getQuote("quote.charge").Revisions[0].Commercial), &doc))
	if ch := doc.Calculation.Charges; len(ch) != 1 || ch[0].ID != "freight.sale" || ch[0].ObligationKey != "freight.sale.1" || ch[0].NetMinor != "1000" {
		t.Fatalf("charges %+v", ch)
	}

	eur := q.euro()
	fx := q.fxRate("EUR", "USD", "1.1", polapi.PurposeQuotation, time.Now().Add(-10*time.Minute))
	d = q.data()
	d.CurrencyRef, d.SalesFXRef = trefp(eur), &trdapi.ConfigRef{Kind: fx.Kind, ID: fx.ID, Revision: fx.Revision}
	_, err = q.saveQuote(q.author, "quote.eur", 0, d)
	q.must(err)
	_, err = q.freezeQuote(q.author, "quote.eur", 1, 1)
	q.must(err)
	c := q.getQuote("quote.eur").Revisions[0].Calculation
	if c.NetMinor != "96000" || c.ComparisonRevenueMinor != "105600" || c.ComparisonCurrencyRef != tref(q.usd) || c.SalesFXRef == nil ||
		c.SalesFXRef.ID != fx.ID || c.ProfitMinor != "39600" {
		t.Fatalf("in EUR %+v", c)
	}
	// Without the rate the EUR quote is not frozen (FX_SCOPE_MISMATCH: a missing rate is never 1);
	// with a rate POL does not know, neither (REFERENCE_INVALID, Python
	// test_sales_fx_missing_reference_is_rejected, PYT-0446); a charge of a purchase-side fee
	// category is no sales charge (CHARGE_SCOPE_UNRESOLVED, Python
	// test_unmapped_additional_charge_is_rejected_before_freeze, PYT-0447). Each revision stays
	// working.
	unknown := trdapi.ConfigRef{Kind: polapi.KindFXRate, ID: "fx.unknown", Revision: 1}
	purchase := q.data()
	purchase.Charges = []trdapi.Charge{{ChargeID: "freight", FeeCategoryRef: tref(q.freight), ObligationKey: "freight.1", Amount: "1",
		Inclusion: trdapi.InclusionSeparate, TaxApplicationRef: tref(app), Evidence: q.proof}}
	for id, c := range map[string]struct {
		edit func(d *trdapi.QuoteData)
		code string
		path string
	}{
		"quote.eur.norate":  {func(d *trdapi.QuoteData) { d.SalesFXRef = nil }, trdapi.CodeFXScopeMismatch, "/args/data/sales_fx_ref"},
		"quote.eur.unknown": {func(d *trdapi.QuoteData) { d.SalesFXRef = &unknown }, "REFERENCE_INVALID", "/args/data/sales_fx_ref"},
		"quote.purchase.fee": {func(d *trdapi.QuoteData) { *d = purchase }, trdapi.CodeChargeScopeUnresolved,
			"/args/data/charges/0/fee_category_ref"},
	} {
		e := d
		c.edit(&e)
		_, err = q.saveQuote(q.author, id, 0, e)
		q.must(err)
		_, err = q.freezeQuote(q.author, id, 1, 1)
		if code(err) != c.code || path(err) != c.path {
			t.Errorf("%s: %s at %s, want %s at %s", id, code(err), path(err), c.code, c.path)
		}
		if v := q.getQuote(id); v.Revisions[0].Status != trdapi.StatusWorking {
			t.Errorf("%s: %+v", id, v.Revisions[0])
		}
	}
}
