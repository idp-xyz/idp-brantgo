package document

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The fixture is the synthetic quote of the S3-01 suite (package quote, quote_test.go): BRC-001's
// RFQ line L1, 120 PCS of sku.1 at 8 USD, the 660.00 USD cost snapshot, a customer with a buyer
// and a billing address, Chinese customer notes with a line break, sealed by quote.SealDraft with
// the server calculation of Python's example (96000 minor units, one stage on payment.calendar).

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
)

func hashOf(s string) string { return quote.Hash([]byte(s)) }

func draft() quote.Data {
	notes := "客户可见说明：合成样例\nKeep this line break."
	return quote.Data{Issuer: &company, Customer: &quote.PartyRef{PartyID: "customer.1", Revision: 2},
		Contact: &quote.ContactRef{ContactID: "contact.1", RowVersion: 2}, Profile: &quote.ProfileRef{PartyID: "customer.1", Revision: 1},
		Addresses: []quote.AddressRef{buyer}, Rfq: &quote.RfqRef{RfqID: "rfq.1", RowVersion: 2},
		Snapshot: &quote.SnapshotRef{SnapshotID: "snapshot.1", ResultHash: resultHash},
		Lines: []quote.Line{{LineID: "L1", RfqLineID: "L1", Sku: sku1, Quantity: "120.000000", Unit: pcs, UnitPrice: "8.0000",
			PriceBasisQuantity: "1.0", DiscountRatio: "0.00", TaxApplication: sale, Evidence: proof}},
		Currency: &usd, PricingPolicy: &policy, TradeTerms: &trade, PaymentTerms: &payment, DocumentTemplate: &templateRef,
		Sequence: &sequence, ExpiresAt: "2026-10-28T08:00:00.000000Z", CustomerNotes: &notes, Evidence: proof}
}

func freezeFacts() quote.Facts {
	return quote.Facts{Company: "company.live", Party: &quote.Party{Confirmed: true, Revision: 2, Customer: true},
		Contact: &quote.Contact{PartyID: "customer.1", RowVersion: 2, Confirmed: true},
		Profile: &quote.Profile{ContentHash: profileHash, PartyRevision: 2, PaymentTerms: payment, TradeTerms: trade,
			Contacts: []quote.ContactRef{{ContactID: "contact.1", RowVersion: 2}}},
		Addresses: map[quote.AddressRef]quote.Address{buyer: {Role: "buyer", Address: "Synthetic buyer address\n上海"},
			billing: {Role: "billing", Address: "Synthetic billing address"}},
		Rfq: &quote.Rfq{RowVersion: 2, Ready: true, LinesHash: linesHash, Lines: []quote.RfqLine{{LineID: "L1", SkuID: "sku.1",
			SpecRevision: 1, Quantity: "120", UOM: "PCS"}}},
		Specs: map[quote.SkuRef]quote.Verdict{sku1: nil},
		Configs: map[quote.Ref]quote.Config{
			company: {ContentHash: hashOf("company"), Company: &quote.IssuerSnapshot{LegalName: "SYNTHETIC Exporter — no actual trade",
				RegistrationNo: "SYNTHETIC-NOT-REAL", Country: "CN", Address: "Synthetic Shanghai address"}},
			usd: {ContentHash: hashOf("usd"), CurrencyCode: "USD"},
			pcs: {ContentHash: hashOf("pcs"), UnitCode: "PCS"},
			sale: {ContentHash: hashOf("sale"), Nested: []quote.Ref{zero}, TaxApplication: &quote.TaxApplication{Side: quote.SideSales,
				SubjectKind: quote.SubjectSKU, SubjectID: "sku.1"}},
			chargeTax: {ContentHash: hashOf("charge"), Nested: []quote.Ref{zero}, TaxApplication: &quote.TaxApplication{Side: quote.SideSales,
				SubjectKind: quote.SubjectFeeCategory, SubjectID: "freight.sale"}},
			zero:        {ContentHash: hashOf("zero")},
			policy:      {ContentHash: hashOf("policy"), Nested: []quote.Ref{zero}, Policy: &quote.Policy{CalculationCurrency: "USD", QuoteValidDays: 7}},
			trade:       {ContentHash: hashOf("trade")},
			payment:     {ContentHash: hashOf("payment")},
			templateRef: {ContentHash: hashOf("template"), Renderer: quote.QuoteRendererV010},
			sequence:    {ContentHash: hashOf("sequence"), SequenceType: quote.DocumentTypeQuote},
			freight:     {ContentHash: hashOf("freight"), FeeSide: quote.SideSalesCharge},
		},
		Snapshot: &quote.Snapshot{RfqID: "rfq.1", RfqVersion: 2, RfqLinesHash: linesHash, Currency: "USD", EvidenceHash: evidenceHash,
			LineIDs: []string{"L1"}},
		Sources: map[string]string{"source.1": sourceHash},
	}
}

func calculation() *quote.Calculation {
	return &quote.Calculation{Lines: []quote.AmountLine{{ID: "L1", NetMinor: "96000", TaxMinor: "0", PayableMinor: "96000"}},
		Charges: []quote.AmountLine{}, NetMinor: "96000", TaxMinor: "0", PayableMinor: "96000", ComparisonCurrency: usd,
		ComparisonRevenueMinor: "96000", CostMinor: "66000", ProfitMinor: "30000", MarginNumerator: "30000", MarginDenominator: "96000",
		MarginRule: quote.MarginPass, CostBasis: quote.CostBasisNominalDirect,
		ExcludedCostCategories: []string{"imputed_financing_cost", "expected_credit_loss", "unrealized_fx_hedge"},
		Payments: []quote.Payment{{StageID: "P1", Percent: "100.00", Trigger: "before_shipment", Days: 0, DayBasis: "calendar",
			Calendar: &calendar, AmountMinor: "96000"}}}
}

// seal freezes the fixture, changed by edit, as revision 1 of quote.1 at issuedAt.
func seal(t *testing.T, edit func(d *quote.Data, f *quote.Facts, c *quote.Calculation)) Revision {
	t.Helper()
	d, f, c := draft(), freezeFacts(), calculation()
	if edit != nil {
		edit(&d, &f, c)
	}
	return sealed(t, d, f, c)
}

// sealed freezes d as revision 1 of quote.1 at issuedAt with calculation c (nil: none).
func sealed(t *testing.T, d quote.Data, f quote.Facts, c *quote.Calculation) Revision {
	t.Helper()
	s, err := quote.SealDraft("quote.1", 1, d, f, c, issuedAt)
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
		Delivery: Delivery{Kind: DeliveryFixedDate, Date: "2026-11-15", Milestone: "handover", TimeZone: "Asia/Shanghai"},
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
