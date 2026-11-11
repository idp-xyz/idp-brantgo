package document

import (
	"fmt"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The fixture is the synthetic quote of package quote's suite (quote/fixture_test.go: draft, facts,
// calculator), sealed by quote.SealDraft with the server calculation the application runs on it:
// BRC-001's RFQ line L1, 120 PCS of sku.1 at 8 USD, the 660.00 USD cost snapshot under policy.1,
// delivered fixed on 2027-01-31 at DEHAM, a customer with a buyer and a billing address, Chinese
// customer notes with a line break. One addition: the payment terms count their one stage in
// calendar days on payment.calendar r1, as Python's example does (examples/commercial.json), so
// that Python's renderer prints the same revision (testdata vector "go fixture mirror").

var issuedAt = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

const (
	resultHash   = "2a809a7b878bb2bedba3eed7534ecd55d43952b41bad144babe3c2d289cc677f"
	evidenceHash = "3668d7dc21b908c3ef5b25ffab99b94193b51c785d0f66a41950fed5107d221b"
	linesHash    = "3ee92f1118c480ab32896c9bbb35186fa05687b9e44c43f0e1544527267c868f"
	sourceHash   = "0397250103796959c07c27f59f03da066db3e42b27d81da397a18d2f2b936a61"
	profileHash  = "9572be119f04ad3072f7a8f98a14a6946efeec50764cb45d43bce99deafe8090"
)

var (
	company     = quote.Ref{Kind: quote.KindCompanyProfile, ID: "company.live", Revision: 1}
	usd         = quote.Ref{Kind: quote.KindCurrency, ID: "USD", Revision: 1}
	pcs         = quote.Ref{Kind: quote.KindUnit, ID: "PCS", Revision: 1}
	sale        = quote.Ref{Kind: quote.KindTaxApplication, ID: "tax.sale", Revision: 1}
	zero        = quote.Ref{Kind: quote.KindTaxProfile, ID: "tax.zero", Revision: 1}
	policy      = quote.Ref{Kind: quote.KindPricingPolicy, ID: "policy.1", Revision: 1}
	trade       = quote.Ref{Kind: quote.KindTradeTerms, ID: "terms.test", Revision: 1}
	payment     = quote.Ref{Kind: quote.KindPaymentTerms, ID: "payment.test", Revision: 1}
	templateRef = quote.Ref{Kind: quote.KindQuoteDocumentTemplate, ID: "quote.template", Revision: 1}
	sequence    = quote.Ref{Kind: quote.KindNumberSequence, ID: "quote.sequence", Revision: 1}
	freight     = quote.Ref{Kind: quote.KindFeeCategory, ID: "freight.sale", Revision: 1}
	chargeTax   = quote.Ref{Kind: quote.KindTaxApplication, ID: "tax.charge", Revision: 1}
	calendar    = quote.Ref{Kind: quote.KindCalendar, ID: "payment.calendar", Revision: 1}
	proof       = []quote.Evidence{{SourceID: "source.1", Locator: "合成核实依据"}}
	sku1        = quote.SkuRef{SkuID: "sku.1", SpecRevision: 1}
	buyer       = quote.AddressRef{PartyID: "customer.1", PartyRevision: 2, AddressID: "hq"}
	billing     = quote.AddressRef{PartyID: "customer.1", PartyRevision: 2, AddressID: "ap"}
	deham       = quote.BusinessRef{Kind: "location", ID: "DEHAM", Revision: 1}
	handover    = quote.Delivery{Kind: "fixed", Date: "2027-01-31", Milestone: "handover", Place: deham, TimeZone: "Asia/Shanghai"}
	dollars     = kernel.MustCurrency("USD", 2)
)

func hashOf(s string) string { return quote.Hash([]byte(s)) }

func draft() quote.Data {
	notes := "客户可见说明：合成样例\nKeep this line break."
	place, delivery := deham, handover
	return quote.Data{Issuer: &company, Customer: &quote.PartyRef{PartyID: "customer.1", Revision: 2},
		Contact: &quote.ContactRef{ContactID: "contact.1", RowVersion: 2}, Profile: &quote.ProfileRef{PartyID: "customer.1", Revision: 1},
		Addresses: []quote.AddressRef{buyer}, Rfq: &quote.RfqRef{RfqID: "rfq.1", RowVersion: 2},
		Snapshot: &quote.SnapshotRef{SnapshotID: "snapshot.1", ResultHash: resultHash},
		Lines: []quote.Line{{LineID: "L1", RfqLineID: "L1", Sku: sku1, Quantity: "120.000000", Unit: pcs, UnitPrice: "8.0000",
			PriceBasisQuantity: "1.0", DiscountRatio: "0.00", TaxApplication: sale, Evidence: proof}},
		Currency: &usd, PricingPolicy: &policy, TradeTerms: &trade, NamedPlace: &place, PaymentTerms: &payment, Delivery: &delivery,
		DocumentTemplate: &templateRef, Sequence: &sequence, ExpiresAt: "2026-10-28T08:00:00.000000Z", CustomerNotes: &notes, Evidence: proof}
}

// freezeFacts are package quote's facts of the fixture, the payment terms naming their calendar.
func freezeFacts() quote.Facts {
	return quote.Facts{Company: "company.live", Party: &quote.Party{Confirmed: true, Revision: 2, Customer: true},
		Contact: &quote.Contact{PartyID: "customer.1", RowVersion: 2, Confirmed: true},
		Profile: &quote.Profile{ContentHash: profileHash, PartyRevision: 2, PaymentTerms: payment, TradeTerms: trade,
			Contacts: []quote.ContactRef{{ContactID: "contact.1", RowVersion: 2}}},
		Addresses: map[quote.AddressRef]quote.Address{buyer: {Role: "buyer", Address: "Synthetic buyer address\n上海"},
			billing: {Role: "billing", Address: "Synthetic billing address"}},
		Rfq:   &quote.Rfq{RowVersion: 2, Ready: true, LinesHash: linesHash},
		Specs: map[quote.SkuRef]quote.Verdict{sku1: nil},
		Configs: map[quote.Ref]quote.Config{
			company: {ContentHash: hashOf("company"), Company: &quote.IssuerSnapshot{LegalName: "SYNTHETIC Exporter — no actual trade",
				RegistrationNo: "SYNTHETIC-NOT-REAL", Country: "CN", Address: "Synthetic Shanghai address"}},
			usd:         {ContentHash: hashOf("usd")},
			pcs:         {ContentHash: hashOf("pcs")},
			sale:        {ContentHash: hashOf("sale"), Nested: []quote.Ref{zero}},
			chargeTax:   {ContentHash: hashOf("charge"), Nested: []quote.Ref{zero}},
			zero:        {ContentHash: hashOf("zero")},
			policy:      {ContentHash: hashOf("policy"), Nested: []quote.Ref{zero}, Policy: &quote.Policy{QuoteValidDays: 7}},
			trade:       {ContentHash: hashOf("trade")},
			payment:     {ContentHash: hashOf("payment"), Nested: []quote.Ref{calendar}},
			calendar:    {ContentHash: hashOf("calendar")},
			templateRef: {ContentHash: hashOf("template"), Renderer: quote.QuoteRendererV010},
			sequence:    {ContentHash: hashOf("sequence"), SequenceType: quote.DocumentTypeQuote},
			freight:     {ContentHash: hashOf("freight")},
		},
		Adopted:  []quote.Ref{usd, pcs},
		Snapshot: &quote.Snapshot{RfqLinesHash: linesHash, EvidenceHash: evidenceHash},
		Sources:  map[string]string{"source.1": sourceHash},
	}
}

// calcEdit changes the calculation's inputs of a test before it runs, and what the payment terms
// say of each stage.
type calcEdit func(q *calc.Quote, f *calc.Facts, stages map[string]quote.Stage)

// calculator is package quote's server calculation of draft d (quote/fixture_test.go calculator,
// as application.ServerCalculator resolves it): POL's USD (2 minor digits) and PCS, the zero tax
// profile, the sales charge fee category, policy.1 with minimum margin 0.1, the RFQ of customer.1
// and contact.1, the 660.00 USD snapshot of an actual costing at DEHAM, and payment.test's one
// stage, 100 before shipment in 0 calendar days on payment.calendar r1.
func calculator(d quote.Data, edits ...calcEdit) quote.Calculate {
	return func() (*quote.Calculation, error) {
		zeroTax := calc.TaxProfile{Ref: quote.CalcRef(zero), Rate: kernel.MustDecimal("0"), PriceBasis: calc.TaxExclusive,
			Recovery: calc.Nonrecoverable}
		q := calc.Quote{Path: "/args/data", Rfq: calc.RfqRef{ID: d.Rfq.RfqID, RowVersion: d.Rfq.RowVersion}, Customer: d.Customer.PartyID,
			Contact: d.Contact.ContactID, Currency: dollars, TradeTerms: quote.CalcRef(*d.TradeTerms), PaymentTerms: quote.CalcRef(*d.PaymentTerms),
			NamedPlace: quote.CalcPlace(*d.NamedPlace), Delivery: quote.CalcDelivery(*d.Delivery)}
		for i, l := range d.Lines {
			q.Lines = append(q.Lines, calc.Line{Path: fmt.Sprintf("/args/data/lines/%d", i), LineID: l.LineID, RfqLineID: l.RfqLineID,
				SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision, Quantity: kernel.MustDecimal(l.Quantity), Unit: calc.Unit{Code: l.Unit.ID},
				UnitPrice: kernel.MustDecimal(l.UnitPrice), PriceBasisQuantity: kernel.MustDecimal(l.PriceBasisQuantity),
				Discount: kernel.MustDecimal(l.DiscountRatio), Tax: &calc.TaxApplication{Ref: quote.CalcRef(l.TaxApplication), Side: calc.SideSales,
					SubjectKind: calc.SubjectSku, SubjectID: l.Sku.SkuID, Profile: zeroTax}})
		}
		for i, c := range d.Charges {
			q.Charges = append(q.Charges, calc.Charge{Path: fmt.Sprintf("/args/data/charges/%d", i), ChargeID: c.ChargeID,
				ObligationKey: c.ObligationKey, Fee: &calc.FeeCategory{Ref: quote.CalcRef(c.FeeCategory), Side: calc.FeeSalesCharge,
					Inclusion: c.Inclusion}, Amount: kernel.MustDecimal(c.Amount), Inclusion: c.Inclusion, IncludedLineID: c.IncludedLineID,
				Evidence: []calc.Evidence{{SourceID: "source.1", Locator: "合成核实依据"}},
				Tax: &calc.TaxApplication{Ref: quote.CalcRef(c.TaxApplication), Side: calc.SideSales, SubjectKind: calc.SubjectFeeCategory,
					SubjectID: c.FeeCategory.ID, Profile: zeroTax}})
		}
		total, _ := kernel.NewMoney(dollars, 66000)
		f := calc.Facts{At: issuedAt,
			Rfq: calc.Rfq{Customer: "customer.1", Contact: "contact.1", Lines: []calc.RfqLine{{LineID: "L1", SkuID: "sku.1",
				Quantity: kernel.MustDecimal("120"), UOM: "PCS"}}},
			Profile: calc.Profile{TradeTerms: quote.CalcRef(trade), PaymentTerms: quote.CalcRef(payment)},
			Cost: calc.Cost{ContextKind: calc.ContextActual, CostBasis: calc.CostBasisNominalDirect, Rfq: calc.RfqRef{ID: "rfq.1", RowVersion: 2},
				PricingPolicy: quote.CalcRef(policy), TradeTerms: quote.CalcRef(trade), PaymentTerms: quote.CalcRef(payment),
				NamedPlace: quote.CalcPlace(deham), Delivery: quote.CalcDelivery(handover), Lines: []calc.CostLine{{LineID: "L1", SkuID: "sku.1",
					SpecRevision: 1, Quantity: kernel.MustDecimal("120"), Unit: calc.Unit{Code: "PCS"}}}, Total: total,
				ExcludedCostCategories: []string{"unrealized_fx_hedge", "imputed_financing_cost", "expected_credit_loss"}},
			Policy: calc.Policy{Ref: quote.CalcRef(policy), CalculationCurrency: dollars, MinimumMargin: kernel.MustDecimal("0.1"), MaxFXAge: time.Hour},
			Stages: []calc.Stage{{StageID: "P1", Percent: kernel.MustDecimal("100.00")}}}
		cal := calendar
		stages := map[string]quote.Stage{"P1": {Trigger: "before_shipment", DayBasis: "calendar", Calendar: &cal}}
		for _, edit := range edits {
			edit(&q, &f, stages)
		}
		c, err := calc.Calculate(q, f)
		if err != nil {
			return nil, err
		}
		return quote.NewCalculation(c, usd, stages), nil
	}
}

// seal freezes the fixture, its draft and facts changed by edit and its calculation by edits, as
// revision 1 of quote.1 at issuedAt (quote.SealDraft).
func seal(t *testing.T, edit func(d *quote.Data, f *quote.Facts), edits ...calcEdit) Revision {
	t.Helper()
	d, f := draft(), freezeFacts()
	if edit != nil {
		edit(&d, &f)
	}
	s, err := quote.SealDraft("quote.1", 1, d, f, calculator(d, edits...), issuedAt)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return Revision{Commercial: s.Commercial, BusinessHash: s.BusinessHash}
}

func template() Template {
	return Template{Ref: templateRef, ContentHash: hashOf("template"), Language: LanguageEn, RendererVersion: RendererVersion,
		Title: "SYNTHETIC QUOTATION — CONTRACT EXAMPLE ONLY", Sections: Sections(),
		FooterNote: "Synthetic data. Not an actual offer or customer acceptance."}
}

func facts() Facts {
	return Facts{
		Customer: Party{Ref: quote.PartyRef{PartyID: "customer.1", Revision: 2}, Name: "Synthetic Buyer Ltd"},
		Contact:  Contact{Ref: quote.ContactRef{ContactID: "contact.1", RowVersion: 2}, Name: "Synthetic Contact 联系人"},
		Items:    map[quote.SkuRef]Item{sku1: {Name: "合成商品", Specification: "SPEC-1"}},
		Currency: Currency{Ref: usd, ContentHash: hashOf("usd"), MinorUnits: 2},
		TradeTerms: TradeTerms{Ref: trade, ContentHash: hashOf("trade"), Incoterm: "FCA", Edition: "2020",
			ResponsibilityNote: "Synthetic negotiated terms; not legal guidance", NamedPlace: "合成地点"},
	}
}

// code returns "CODE at path" of a rejection, "OK" for nil.
func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code + " at " + rej.Fields[0].Path
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return "OK"
}
