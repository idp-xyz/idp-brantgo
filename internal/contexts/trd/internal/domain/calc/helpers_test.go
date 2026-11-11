package calc

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The vectors of this package's tests come from the T03 contract specification (V0.1 §5.3
// synthetic cases), the TRD invariant register (TRD-INV-006–008), the reference acceptance tests
// (BC-AT04, BC-AT06, BC-AT07) and the Python V0.50 reference tests, each cited where it is used
// with its mapping ID (docs/08_ENGINEERING_READINESS/02_Python_Test_to_Go_Mapping_V0.1.csv). The
// Python tests run through Python's Core and SQLite; what they compute is ported here, what they
// persist, authorize or re-check at a later command is the application layer's. All prices,
// rates and dates are synthetic.

var (
	usd = kernel.MustCurrency("USD", 2)
	eur = kernel.MustCurrency("EUR", 2)
	jpy = kernel.MustCurrency("JPY", 0)

	at = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC) // the Python fixture's clock

	pcs   = Unit{Code: "PCS", Precision: 0}
	box   = Unit{Code: "BOX", Precision: 0}
	dozen = Unit{Code: "DOZEN", Precision: 0}

	tradeTerms   = Ref{Kind: "trade_terms", ID: "terms.test", Revision: 1}
	paymentTerms = Ref{Kind: "payment_terms", ID: "payment.test", Revision: 1}
	port         = Ref{Kind: "location", ID: "port.test", Revision: 1}
	policyRef    = Ref{Kind: "pricing_policy", ID: "policy.1", Revision: 1}
	delivery     = Delivery{Kind: "fixed", Date: "2026-11-15", Milestone: "handover", Place: port, TimeZone: "Asia/Shanghai"}
)

func dec(s string) kernel.Decimal { return kernel.MustDecimal(s) }

func decp(s string) *kernel.Decimal {
	d := dec(s)
	return &d
}

func minor(t *testing.T, c kernel.Currency, v int64) kernel.Money {
	t.Helper()
	m, err := kernel.NewMoney(c, v)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func profile(id, rate string, basis PriceBasis) TaxProfile {
	return TaxProfile{Ref: Ref{Kind: "tax_profile", ID: id, Revision: 1}, Rate: dec(rate), PriceBasis: basis, Recovery: Nonrecoverable}
}

// zeroTax is the Python fixture's tax.zero: rate 0, exclusive, nonrecoverable.
var zeroTax = profile("tax.zero", "0", TaxExclusive)

// goodsTax is a sales tax application for sku.1 (Python sales.goods.default) taxed by p.
func goodsTax(p TaxProfile) *TaxApplication {
	return &TaxApplication{Ref: Ref{Kind: "tax_application", ID: "sales.goods." + p.Ref.ID, Revision: 1}, Side: SideSales,
		SubjectKind: SubjectSku, SubjectID: "sku.1", Profile: p}
}

// feeTax is a sales tax application for fee category fee taxed by p.
func feeTax(fee string, p TaxProfile) *TaxApplication {
	return &TaxApplication{Ref: Ref{Kind: "tax_application", ID: fee + ".tax", Revision: 1}, Side: SideSales,
		SubjectKind: SubjectFeeCategory, SubjectID: fee, Profile: p}
}

// salesCharge is Python t03_sales_fixture.sales_charge: a fixed sales fee of amount, taxed at 0.
func salesCharge(fee, inclusion, amount string) Charge {
	return Charge{
		Path: "/args/data/charges/0", ChargeID: fee + ".charge", ObligationKey: fee + ".obligation",
		Fee:    &FeeCategory{Ref: Ref{Kind: "fee_category", ID: fee, Revision: 1}, Side: FeeSalesCharge, Inclusion: inclusion},
		Amount: dec(amount), Inclusion: inclusion,
		Evidence: []Evidence{{SourceID: "source.1", Locator: "Synthetic seller income"}},
		Tax:      feeTax(fee, zeroTax),
	}
}

// line is the Python fixture's sales line L1: 120 PCS of sku.1 at 8 USD, taxed at 0.
func line() Line {
	return Line{
		Path: "/args/data/lines/0", LineID: "L1", RfqLineID: "L1", SkuID: "sku.1", SpecRevision: 1,
		Quantity: dec("120"), Unit: pcs, UnitPrice: dec("8"), PriceBasisQuantity: dec("1"), Discount: dec("0"),
		Tax: goodsTax(zeroTax),
	}
}

// quote is the T03 baseline (BRC-001; Python ServerCalculationTests.quote_data): one line of 120 PCS
// × 8.00 USD on the frozen 660.00 USD snapshot.
func quote() Quote {
	return Quote{
		Path: "/args/data", Rfq: RfqRef{ID: "rfq.1", RowVersion: 2}, Customer: "customer.1", Contact: "contact.1",
		Currency: usd, TradeTerms: tradeTerms, PaymentTerms: paymentTerms, NamedPlace: port, Delivery: delivery,
		Lines: []Line{line()},
	}
}

// facts are the upstream answers for quote(): the RFQ, the trading profile, the snapshot costing
// 120 PCS at 660.00 USD, policy.1 (minimum margin 0.1, rates at most an hour old) and one stage of 100.
func facts(t *testing.T) Facts {
	return Facts{
		At:      at,
		Rfq:     Rfq{Customer: "customer.1", Contact: "contact.1", Lines: []RfqLine{{LineID: "L1", SkuID: "sku.1", Quantity: dec("120"), UOM: "PCS"}}},
		Profile: Profile{TradeTerms: tradeTerms, PaymentTerms: paymentTerms},
		Cost: Cost{
			ContextKind: ContextActual, CostBasis: CostBasisNominalDirect, Rfq: RfqRef{ID: "rfq.1", RowVersion: 2},
			PricingPolicy: policyRef, TradeTerms: tradeTerms, PaymentTerms: paymentTerms, NamedPlace: port, Delivery: delivery,
			Lines:                  []CostLine{{LineID: "L1", SkuID: "sku.1", SpecRevision: 1, Quantity: dec("120"), Unit: pcs}},
			Total:                  minor(t, usd, 66000),
			ExcludedCostCategories: []string{"imputed_financing_cost", "expected_credit_loss", "unrealized_fx_hedge"},
		},
		Policy: Policy{Ref: policyRef, CalculationCurrency: usd, MinimumMargin: dec("0.1"), MaxFXAge: time.Hour},
		Stages: []Stage{{StageID: "P1", Percent: dec("100")}},
	}
}

// recost makes the RFQ and the snapshot's context cover exactly the quote's lines at their
// quantities, as Python t03_scope_fixture.recost re-costs a matching snapshot.
func recost(q Quote, f *Facts) {
	f.Rfq.Lines, f.Cost.Lines = nil, nil
	for _, l := range q.Lines {
		f.Rfq.Lines = append(f.Rfq.Lines, RfqLine{LineID: l.RfqLineID, SkuID: l.SkuID, Quantity: l.Quantity, UOM: "PCS"})
		f.Cost.Lines = append(f.Cost.Lines, CostLine{LineID: l.RfqLineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision, Quantity: l.Quantity, Unit: pcs})
	}
}

// eurRate is Python's fx.sales: 1 EUR is 1.1 USD for quotation, observed an hour before the clock.
func eurRate() *FXRate {
	return &FXRate{Ref: Ref{Kind: "fx_rate", ID: "fx.sales", Revision: 1}, From: eur, To: usd, Rate: dec("1.1"),
		Purpose: PurposeQuotation, ObservedAt: at.Add(-time.Hour)}
}

// rejected fails the test unless err is a rejection with code whose first field is at path (any
// path when path is empty) and whose category is the code's.
func rejected(t *testing.T, err error, code, path string) {
	t.Helper()
	r, ok := kernel.AsRejection(err)
	if !ok {
		t.Fatalf("got %v, want rejection %s at %q", err, code, path)
	}
	if r.Code != code || (path != "" && r.Fields[0].Path != path) {
		t.Fatalf("got %s at %s (%s), want %s at %q", r.Code, r.Fields[0].Path, r.Fields[0].Reason, code, path)
	}
	if r.Category != kernel.CategoryInvalid {
		t.Fatalf("%s has category %s", code, r.Category)
	}
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
