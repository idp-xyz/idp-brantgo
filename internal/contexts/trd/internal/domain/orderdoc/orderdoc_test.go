package orderdoc

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func ref(kind, id string) quote.Ref { return quote.Ref{Kind: kind, ID: id, Revision: 1} }

var (
	generatedAt = time.Date(2026, 11, 2, 3, 4, 5, 6789, time.UTC)
	usd         = ref(quote.KindCurrency, "USD")
	trade       = ref(quote.KindTradeTerms, "terms.fca")
	scSequence  = ref(quote.KindNumberSequence, "sc.sequence")
	piSequence  = ref(quote.KindNumberSequence, "pi.sequence")
	customer    = quote.PartyRef{PartyID: "customer.1", Revision: 2}
	contact     = quote.ContactRef{ContactID: "contact.1", RowVersion: 2}
	clerk       = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "clerk"}
)

// revisionOne is BRC-001's order at revision 1 (orderrev's fixture reads the same from a sealed
// quote's snapshot): L1 red S 24, L2 red M 36, L3 blue S 12, L4 blue M 48 PCS at 8 USD, 960.00 paid
// 30 % on order confirmation and 70 % 30 days after shipment, FCA DEHAM on 2027-01-31.
func revisionOne() *orderrev.Revision {
	tax := orderrev.Tax{Application: ref(quote.KindTaxApplication, "tax.sale"), Profile: ref(quote.KindTaxProfile, "tax.zero"),
		PriceBasis: "exclusive", Rate: "0"}
	deham := quote.BusinessRef{Kind: "location", ID: "DEHAM", Revision: 1}
	r := &orderrev.Revision{Contract: orderrev.SnapshotContract, Scope: orderrev.Scope{Tenant: "tenant.1", Company: "company.live"},
		OrderID: "order.1", Number: 1, Hash: quote.Hash([]byte("order.1 snapshot")),
		Source: orderrev.Source{QuoteID: "quote.1", Revision: 1, BusinessHash: quote.Hash([]byte("quote.1 r1")), Number: "QT-2026-00001",
			DocumentSHA256: quote.Hash([]byte("QT-2026-00001.txt")), AcceptanceID: "acceptance.1"},
		Parties: quote.PartySnapshots{Issuer: quote.IssuerSnapshot{CompanyID: "company.live", LegalName: "SYNTHETIC Exporter — no actual trade",
			RegistrationNo: "SYNTHETIC-NOT-REAL", Country: "CN", Address: "Synthetic Shanghai address"},
			Customer: quote.CustomerSnapshot{PartyID: "customer.1", PartyRevision: 2, Addresses: []quote.AddressSnapshot{
				{AddressID: "ap", Role: "billing", Address: "Synthetic billing address"},
				{AddressID: "hq", Role: "buyer", Address: "Synthetic buyer address\nHamburg"}}}},
		Customer: customer, Contact: contact, Currency: usd,
		Terms: orderrev.Terms{TradeTerms: trade, NamedPlace: deham, PaymentTerms: ref(quote.KindPaymentTerms, "payment.30-70"),
			Delivery: quote.Delivery{Kind: "fixed", Date: "2027-01-31", Milestone: "handover", Place: deham, TimeZone: "Asia/Shanghai"}},
		Amounts: orderrev.Amounts{Charges: []quote.AmountLine{}, Net: "96000", Tax: "0", Payable: "96000",
			Payments: []quote.Payment{{StageID: "P1", Percent: "30", Trigger: "order_confirmed", AmountMinor: "28800"},
				{StageID: "P2", Percent: "70", Trigger: "after_shipment", Days: 30, AmountMinor: "67200"}}},
		Charges: []orderrev.Charge{}}
	for _, l := range []struct{ id, sku, qty, net string }{{"L1", "ST01.red.S", "24", "19200"}, {"L2", "ST01.red.M", "36", "28800"},
		{"L3", "ST01.blue.S", "12", "9600"}, {"L4", "ST01.blue.M", "48", "38400"}} {
		r.Lines = append(r.Lines, orderrev.Line{LineID: l.id, RfqLineID: l.id, Sku: quote.SkuRef{SkuID: l.sku, SpecRevision: 1}, Quantity: l.qty,
			Unit: ref(quote.KindUnit, "PCS"), NormalizedQuantity: l.qty, CostUnit: "PCS", UnitPrice: "8", PriceBasisQuantity: "1", DiscountRatio: "0",
			Tax: tax})
		r.Amounts.Lines = append(r.Amounts.Lines, quote.AmountLine{ID: l.id, NetMinor: l.net, TaxMinor: "0", PayableMinor: l.net})
	}
	return r
}

// revisionTwo is TD-AT01's revision 2: blue M at 44, by amendment.1.
func revisionTwo(t *testing.T) *orderrev.Revision {
	t.Helper()
	r1 := revisionOne()
	p := orderrev.ProposalOf(r1)
	p.Lines[3].Quantity = "44"
	r2, _, err := orderrev.Next(r1, orderrev.AmendmentRef{ID: "amendment.1", Hash: quote.Hash([]byte("amendment.1"))}, p,
		orderrev.Fulfilment{Source: orderrev.NotWired}, orderrev.Facts{Currency: kernel.MustCurrency("USD", 2), Units: map[string]calc.Unit{"PCS": {Code: "PCS"}}},
		"/args/proposal")
	if err != nil {
		t.Fatal(err)
	}
	return r2
}

func contractTemplate() Template {
	return Template{Ref: ref(KindOrderDocumentTemplate, "sc.en"), ContentHash: quote.Hash([]byte("sc.en")), DocumentKind: KindSalesContract,
		Language: LanguageEn, RendererVersion: RendererVersion, Title: "SYNTHETIC SALES CONTRACT — EXAMPLE ONLY",
		Sections: RequiredSections(KindSalesContract),
		Clauses: []string{"Packing: export cartons, 12 pieces each.", "Inspection: the seller's QC before shipment; the buyer may inspect at its cost.",
			"Force majeure: as agreed in writing."},
		FooterNote: "Synthetic data. Not an actual contract."}
}

func invoiceTemplate() Template {
	return Template{Ref: ref(KindOrderDocumentTemplate, "pi.en"), ContentHash: quote.Hash([]byte("pi.en")), DocumentKind: KindProformaInvoice,
		Language: LanguageEn, RendererVersion: RendererVersion, Title: "SYNTHETIC PROFORMA INVOICE",
		Sections: RequiredSections(KindProformaInvoice), FooterNote: "Synthetic data. Pay against the order, not this document."}
}

func facts(kind string) Facts {
	sequence := Sequence{Ref: scSequence, DocumentType: DocumentTypeOrder}
	if kind == KindProformaInvoice {
		sequence = Sequence{Ref: piSequence, DocumentType: DocumentTypeProformaInvoice}
	}
	items := map[quote.SkuRef]Item{}
	for _, s := range []string{"ST01.red.S", "ST01.red.M", "ST01.blue.S", "ST01.blue.M", "ST01.navy.M"} {
		parts := strings.Split(s, ".")
		items[quote.SkuRef{SkuID: s, SpecRevision: 1}] = Item{Name: "ST01 衬衫 " + parts[1] + " " + parts[2], Specification: s + " r1"}
	}
	return Facts{Customer: Party{Ref: customer, Name: "Synthetic Buyer GmbH"}, Contact: Contact{Ref: contact, Name: "Synthetic Contact 联系人"},
		Items: items, Currency: Currency{Ref: usd, MinorUnits: 2},
		TradeTerms: TradeTerms{Ref: trade, Incoterm: "FCA", Edition: "Incoterms 2020", ResponsibilityNote: "Synthetic negotiated terms; not legal guidance",
			NamedPlace: "Hamburg (DEHAM)"},
		Sequence: sequence}
}

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code + " at " + rej.Fields[0].Path
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return "OK"
}

// render prepares and generates a document.
func render(t *testing.T, r *orderrev.Revision, tpl Template, f Facts, number string) *Generated {
	t.Helper()
	p, err := Prepare(r, tpl, f, generatedAt)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	g, err := p.Generate(number)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return g
}

// The S/C of BRC-001's order at revision 1, written out by hand from the layout in doc.go: the
// independent oracle of order-text/0.1.0 (the golden files below come from the renderer itself).
const contractR1 = `SYNTHETIC SALES CONTRACT — EXAMPLE ONLY

Seller: SYNTHETIC Exporter — no actual trade
Synthetic Shanghai address
Registration: SYNTHETIC-NOT-REAL | Country: CN

Buyer: Synthetic Buyer GmbH
Synthetic buyer address
Hamburg
Contact: Synthetic Contact 联系人

Number: SC-2026-00007
Order: order.1 r1
Order SHA-256: 5c3d0b9c0d56d7e6e1f5a6ee5ac4b6b2f2e7d0f0a5b4c3d2e1f0a9b8c7d6e5f4
Quote: QT-2026-00001 (quote.1 r1) | Acceptance: acceptance.1
Date: 2026-11-02T03:04:05.000006Z

Lines (amounts in USD):
L1 | ST01 衬衫 red S | ST01.red.S r1 | 24 PCS | price 8 per 1 PCS | discount 0 | net 192.00 | tax 0.00 | total 192.00
L2 | ST01 衬衫 red M | ST01.red.M r1 | 36 PCS | price 8 per 1 PCS | discount 0 | net 288.00 | tax 0.00 | total 288.00
L3 | ST01 衬衫 blue S | ST01.blue.S r1 | 12 PCS | price 8 per 1 PCS | discount 0 | net 96.00 | tax 0.00 | total 96.00
L4 | ST01 衬衫 blue M | ST01.blue.M r1 | 48 PCS | price 8 per 1 PCS | discount 0 | net 384.00 | tax 0.00 | total 384.00

Currency: USD
Net: 960.00 | Tax: 0.00 | Total: 960.00

Terms: FCA Incoterms 2020 | Synthetic negotiated terms; not legal guidance
Named place: Hamburg (DEHAM) | Trade terms: terms.fca r1

Delivery: 2027-01-31 | handover | Asia/Shanghai

Payment plan (planned amounts, not receipts):
P1 | 30% | 288.00 | order_confirmed | 0 calendar days
P2 | 70% | 672.00 | after_shipment | 30 calendar days

Clauses:
1. Packing: export cartons, 12 pieces each.
2. Inspection: the seller's QC before shipment; the buyer may inspect at its cost.
3. Force majeure: as agreed in writing.

Signatures (this generated copy is unsigned; signed copies are recorded as evidence):
Seller: SYNTHETIC Exporter — no actual trade
Buyer: Synthetic Buyer GmbH

Synthetic data. Not an actual contract.
`

func TestSalesContractOfRevisionOne(t *testing.T) {
	r1 := revisionOne()
	r1.Hash = "5c3d0b9c0d56d7e6e1f5a6ee5ac4b6b2f2e7d0f0a5b4c3d2e1f0a9b8c7d6e5f4"
	p, err := Prepare(r1, contractTemplate(), facts(KindSalesContract), generatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if p.Number != (NumberRequest{Sequence: scSequence, DocumentType: DocumentTypeOrder, BusinessRef: "order.1"}) || p.OrderRevision != 1 ||
		p.OrderHash != r1.Hash || p.DocumentKind != KindSalesContract || p.Template != contractTemplate().Ref || !p.GeneratedAt.Equal(kernel.Instant(generatedAt)) {
		t.Fatalf("plan: %+v", p)
	}
	g, err := p.Generate("SC-2026-00007")
	if err != nil {
		t.Fatal(err)
	}
	if string(g.File.Content) != contractR1 {
		t.Fatalf("S/C r1:\n%s\nwant:\n%s", g.File.Content, contractR1)
	}
	golden(t, "sc-brc001-r1.txt", g.File.Content)
	b := g.Binding
	if g.File.Name != "SC-2026-00007-r1.txt" || g.File.MimeType != MimeText || g.File.SHA256 != quote.Hash([]byte(contractR1)) ||
		g.File.Size != len(contractR1) || b.OrderID != "order.1" || b.OrderRevision != 1 || b.OrderHash != r1.Hash || b.Number != "SC-2026-00007" ||
		b.BusinessRef != "order.1" || b.RendererVersion != RendererVersion || b.GeneratedAt != "2026-11-02T03:04:05.000006Z" ||
		b.ContentSHA256 != g.File.SHA256 || b.SizeBytes != g.File.Size || b.TemplateHash != contractTemplate().ContentHash {
		t.Fatalf("file and binding: %+v %+v", g.File, b)
	}
	var names []string
	for _, e := range b.Entries() {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"order_id", "order_revision", "order_hash", "document_kind", "number", "sequence_ref", "business_ref",
		"template_ref", "template_content_hash", "renderer_version", "generated_at", "file_name", "mime_type", "content_sha256", "size_bytes"}) ||
		b.Entries()[5].Value != "number_sequence:sc.sequence@1" {
		t.Fatalf("entries: %q", names)
	}
}

// TRD-INV-020 and TD-AT01: the PI of revision 2 (blue M at 44, 928.00) names the amendment and is no
// commercial or tax invoice; revision 1's documents are stale for revision 2 and stay verifiable.
func TestProformaInvoiceOfRevisionTwo(t *testing.T) {
	r1, r2 := revisionOne(), revisionTwo(t)
	pi := render(t, r2, invoiceTemplate(), facts(KindProformaInvoice), "PI-2026-00003")
	golden(t, "pi-brc001-r2.txt", pi.File.Content)
	text := string(pi.File.Content)
	for _, want := range []string{"Order: order.1 r2\n", "Amendment: amendment.1 of r1\n", "Not a commercial invoice or a tax invoice.\n",
		"L4 | ST01 衬衫 blue M | ST01.blue.M r1 | 44 PCS | price 8 per 1 PCS | discount 0 | net 352.00 | tax 0.00 | total 352.00\n",
		"Net: 928.00 | Tax: 0.00 | Total: 928.00\n", "P1 | 30% | 278.40 | order_confirmed | 0 calendar days\n",
		"P2 | 70% | 649.60 | after_shipment | 30 calendar days\n", "Order SHA-256: " + r2.Hash + "\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("PI r2 lacks %q", want)
		}
	}
	if pi.File.Name != "PI-2026-00003-r2.txt" || strings.Contains(text, "Clauses") || strings.Contains(text, "Signatures") {
		t.Fatalf("PI r2: %s", pi.File.Name)
	}
	sc1 := render(t, r1, contractTemplate(), facts(KindSalesContract), "SC-2026-00007")
	if err := Current(&sc1.Binding, KindSalesContract, r1); err != nil {
		t.Fatalf("S/C r1 at r1: %v", err)
	}
	if err := Current(&sc1.Binding, KindSalesContract, r2); code(err) != "ORDER_DOCUMENT_STALE at /document_binding" {
		t.Fatalf("S/C r1 at r2: %s", code(err))
	}
	if err := Current(&pi.Binding, KindSalesContract, r2); code(err) != "DOCUMENT_BINDING_MISSING at /document_binding" {
		t.Fatalf("PI as an S/C: %s", code(err))
	}
	if err := Current(nil, KindProformaInvoice, r2); code(err) != "DOCUMENT_BINDING_MISSING at /document_binding" {
		t.Fatalf("no binding: %s", code(err))
	}
	// Stale documents stay readable and verified.
	if err := Verify(&sc1.Binding, sc1.File.Content); err != nil {
		t.Fatalf("verify stale S/C: %v", err)
	}
	broken := slices.Clone(sc1.File.Content)
	broken[0] ^= 1
	for name, content := range map[string][]byte{"altered": broken, "missing": nil, "truncated": sc1.File.Content[1:]} {
		if err := Verify(&sc1.Binding, content); code(err) != "DOCUMENT_INTEGRITY_FAILURE at /document" {
			t.Errorf("%s bytes: %s", name, code(err))
		}
	}
	if err := Verify(nil, sc1.File.Content); code(err) != "DOCUMENT_BINDING_MISSING at /document_binding" {
		t.Errorf("no binding: %s", code(err))
	}
	// The S/C of revision 2 keeps the order's contract number.
	sc2 := render(t, r2, contractTemplate(), facts(KindSalesContract), "SC-2026-00007")
	if sc2.File.Name != "SC-2026-00007-r2.txt" || sc2.Binding.BusinessRef != sc1.Binding.BusinessRef || sc2.File.SHA256 == sc1.File.SHA256 {
		t.Fatalf("S/C r2: %+v", sc2.Binding)
	}
}

// The variant exercises what the BRC-001 documents do not: a separate and an included charge, a
// lead-time delivery, payment stages in working days on a calendar, sections in another order, the
// law slot (S3-17's terms as the command will resolve them), a PI with clauses, and a footer note
// with an emoji and a line break.
func TestVariantDocument(t *testing.T) {
	r := revisionOne()
	cal := ref(quote.KindCalendar, "cn.workdays")
	anchor := quote.BusinessRef{Kind: "delivery_anchor", ID: "deposit.received", Revision: 1}
	r.Terms.Delivery = quote.Delivery{Kind: "lead_time", Anchor: &anchor, Days: 45, DayBasis: "working", Calendar: &cal, Milestone: "loaded_on_vessel",
		Place: r.Terms.NamedPlace, TimeZone: "Asia/Shanghai"}
	r.Amounts.Payments[1].DayBasis, r.Amounts.Payments[1].Calendar = "working", &cal
	r.Charges = []orderrev.Charge{
		{ChargeID: "C1", FeeCategory: ref(quote.KindFeeCategory, "freight.sale"), ObligationKey: "freight.main", Amount: "60", Inclusion: "separate"},
		{ChargeID: "C2", FeeCategory: ref(quote.KindFeeCategory, "packing.sale"), ObligationKey: "packing.L2", Amount: "5", Inclusion: "included_in_line",
			IncludedLineID: "L2"}}
	r.Amounts.Charges = []quote.AmountLine{{ID: "C1", ObligationKey: "freight.main", NetMinor: "6000", TaxMinor: "0", PayableMinor: "6000"},
		{ID: "C2", ObligationKey: "packing.L2", NetMinor: "0", TaxMinor: "0", PayableMinor: "0"}}
	r.Amounts.Net, r.Amounts.Payable = "102000", "102000"
	r.Amounts.Payments[0].AmountMinor, r.Amounts.Payments[1].AmountMinor = "30600", "71400"
	f := facts(KindSalesContract)
	f.Law = &Law{GoverningLaw: "Laws of the People's Republic of China; CISG applies", DisputeResolution: "CIETAC, Shanghai, in English"}
	tpl := contractTemplate()
	tpl.Sections = []string{SectionIdentity, SectionSeller, SectionBuyer, SectionLines, SectionTotals, SectionPayments, SectionDelivery, SectionTerms,
		SectionLaw, SectionClauses, SectionSignatures, SectionNotes}
	tpl.FooterNote = "Synthetic 🚢 data.\nSecond line."
	g := render(t, r, tpl, f, "SC-2026-00008")
	golden(t, "sc-variant.txt", g.File.Content)
	text := string(g.File.Content)
	for _, want := range []string{"charge C1 | freight.main | separate | net 60.00 | tax 0.00 | total 60.00\n",
		"charge C2 | packing.L2 | included_in_line | net 0.00 | tax 0.00 | total 0.00\n",
		"Delivery: 45 working days after deposit.received r1 | calendar cn.workdays r1 | loaded_on_vessel | Asia/Shanghai\n",
		"P2 | 70% | 714.00 | after_shipment | 30 working days | calendar cn.workdays r1\n",
		"Governing law: Laws of the People's Republic of China; CISG applies\nDispute resolution: CIETAC, Shanghai, in English\n\nClauses:",
		"Synthetic 🚢 data.\nSecond line.\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("variant lacks %q", want)
		}
	}
	if !strings.HasPrefix(text, "SYNTHETIC SALES CONTRACT — EXAMPLE ONLY\n\nNumber: SC-2026-00008\n") {
		t.Errorf("variant opens with %q", text[:80])
	}
	pi := invoiceTemplate()
	pi.Sections = append(pi.Sections, SectionClauses)
	pi.Clauses = []string{"Bank charges outside China are the buyer's."}
	if g := render(t, revisionOne(), pi, facts(KindProformaInvoice), "PI-2026-00004"); !strings.Contains(string(g.File.Content),
		"Clauses:\n1. Bank charges outside China are the buyer's.\n") {
		t.Errorf("PI with clauses: %s", g.File.Content)
	}
	// JPY has no minor digits.
	yen := revisionOne()
	yen.Currency = ref(quote.KindCurrency, "JPY")
	fy := facts(KindSalesContract)
	fy.Currency = Currency{Ref: yen.Currency, MinorUnits: 0}
	if g := render(t, yen, contractTemplate(), fy, "SC-2026-00009"); !strings.Contains(string(g.File.Content), "Net: 96000 | Tax: 0 | Total: 96000\n") {
		t.Errorf("JPY totals: %s", g.File.Content)
	}
}

// What a template, the facts or the revision may not be.
func TestPrepareRefusals(t *testing.T) {
	r := revisionOne()
	for name, c := range map[string]struct {
		tpl  func(t *Template)
		f    func(f *Facts)
		r    func(r *orderrev.Revision)
		want string
	}{
		"a quote template": {tpl: func(t *Template) { t.Ref.Kind = quote.KindQuoteDocumentTemplate }, want: "REFERENCE_INVALID at /template/ref"},
		"another renderer": {tpl: func(t *Template) { t.RendererVersion = "quote-text/0.1.0" }, want: "DEPENDENCY_NOT_SUPPORTED at /template/renderer_version"},
		"an unknown kind":  {tpl: func(t *Template) { t.DocumentKind = "commercial_invoice" }, want: "REFERENCE_INVALID at /template/document_kind"},
		"a blank title":    {tpl: func(t *Template) { t.Title = " " }, want: "REFERENCE_INVALID at /template/title"},
		"no signatures": {tpl: func(t *Template) {
			t.Sections = slices.DeleteFunc(t.Sections, func(s string) bool { return s == SectionSignatures })
		}, want: "REFERENCE_INVALID at /template/sections/10"},
		"a section twice":            {tpl: func(t *Template) { t.Sections[1] = SectionSeller }, want: "REFERENCE_INVALID at /template/sections/1"},
		"a quote section":            {tpl: func(t *Template) { t.Sections[0] = "issuer" }, want: "REFERENCE_INVALID at /template/sections/0"},
		"signatures on a PI":         {tpl: func(t *Template) { *t = invoiceTemplate(); t.Sections = append(t.Sections, SectionSignatures) }, f: func(f *Facts) { *f = facts(KindProformaInvoice) }, want: "REFERENCE_INVALID at /template/sections/9"},
		"no clauses for the section": {tpl: func(t *Template) { t.Clauses = nil }, want: "REFERENCE_INVALID at /template/clauses"},
		"clauses without a section":  {tpl: func(t *Template) { *t = invoiceTemplate(); t.Clauses = []string{"x"} }, f: func(f *Facts) { *f = facts(KindProformaInvoice) }, want: "REFERENCE_INVALID at /template/clauses"},
		"a blank clause":             {tpl: func(t *Template) { t.Clauses[1] = "  " }, want: "REFERENCE_INVALID at /template/clauses/1"},
		"zh-CN":                      {tpl: func(t *Template) { t.Language = "zh-CN" }, want: "DEPENDENCY_NOT_SUPPORTED at /template/language"},
		"a law section before S3-17": {tpl: func(t *Template) { t.Sections = append(t.Sections, SectionLaw) }, want: "DEPENDENCY_NOT_SUPPORTED at /template/sections"},
		"law terms left out": {f: func(f *Facts) { f.Law = &Law{GoverningLaw: "PRC law", DisputeResolution: "CIETAC"} },
			want: "REFERENCE_INVALID at /template/sections"},
		"half the law terms": {tpl: func(t *Template) { t.Sections = append(t.Sections, SectionLaw) }, f: func(f *Facts) { f.Law = &Law{GoverningLaw: "PRC law"} },
			want: "REFERENCE_INVALID at /facts/law"},
		"a PI sequence for an S/C":  {f: func(f *Facts) { f.Sequence = Sequence{Ref: piSequence, DocumentType: DocumentTypeProformaInvoice} }, want: "REFERENCE_INVALID at /facts/sequence"},
		"another customer":          {f: func(f *Facts) { f.Customer.Ref.Revision = 3 }, want: "REFERENCE_INVALID at /facts/customer"},
		"a nameless contact":        {f: func(f *Facts) { f.Contact.Name = "" }, want: "REFERENCE_INVALID at /facts/contact"},
		"an unnamed item":           {f: func(f *Facts) { delete(f.Items, quote.SkuRef{SkuID: "ST01.red.M", SpecRevision: 1}) }, want: "REFERENCE_INVALID at /facts/items/ST01.red.M"},
		"another currency revision": {f: func(f *Facts) { f.Currency.Ref.Revision = 2 }, want: "REFERENCE_INVALID at /facts/currency"},
		"five minor digits":         {f: func(f *Facts) { f.Currency.MinorUnits = 5 }, want: "REFERENCE_INVALID at /facts/currency"},
		"trade terms without edition": {f: func(f *Facts) { f.TradeTerms.Edition = "" },
			want: "REFERENCE_INVALID at /facts/trade_terms"},
		"no buyer address":    {r: func(r *orderrev.Revision) { r.Parties.Customer.Addresses = nil }, want: "REFERENCE_INVALID at /revision/parties/customer/addresses"},
		"no seller":           {r: func(r *orderrev.Revision) { r.Parties.Issuer.LegalName = "" }, want: "REFERENCE_INVALID at /revision/parties/issuer"},
		"a line's amounts":    {r: func(r *orderrev.Revision) { r.Amounts.Lines = r.Amounts.Lines[1:] }, want: "REFERENCE_INVALID at /revision/lines/0"},
		"a fractional cent":   {r: func(r *orderrev.Revision) { r.Amounts.Net = "1.5" }, want: "REFERENCE_INVALID at /revision/amounts"},
		"an unknown delivery": {r: func(r *orderrev.Revision) { r.Terms.Delivery.Kind = "asap" }, want: "REFERENCE_INVALID at /revision/terms/delivery"},
		"no revision hash":    {r: func(r *orderrev.Revision) { r.Hash = "" }, want: "REFERENCE_INVALID at /revision"},
		"a huge footer":       {tpl: func(t *Template) { t.FooterNote = strings.Repeat("注", MaxDocumentBytes/3) }, want: "CALCULATION_LIMIT_EXCEEDED at /"},
		"invalid UTF-8":       {f: func(f *Facts) { f.Customer.Name = "\xff" }, want: "REFERENCE_INVALID at /"},
	} {
		rev, tpl, f := revisionOne(), contractTemplate(), facts(KindSalesContract)
		if c.r != nil {
			c.r(rev)
		}
		if c.tpl != nil {
			c.tpl(&tpl)
		}
		if c.f != nil {
			c.f(&f)
		}
		if _, err := Prepare(rev, tpl, f, generatedAt); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
	if _, err := Prepare(nil, contractTemplate(), facts(KindSalesContract), generatedAt); code(err) != "REFERENCE_INVALID at /revision" {
		t.Errorf("no revision: %s", code(err))
	}
	if _, err := Prepare(r, contractTemplate(), facts(KindSalesContract), time.Time{}); code(err) != "REFERENCE_INVALID at /generated_at" {
		t.Errorf("no instant: %s", code(err))
	}
	p, err := Prepare(r, contractTemplate(), facts(KindSalesContract), generatedAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"", "SC:1", strings.Repeat("9", 65), "-SC"} {
		if _, err := p.Generate(n); code(err) != "REFERENCE_INVALID at /number" {
			t.Errorf("number %q: %s", n, code(err))
		}
	}
	// The 64-character number fits.
	if _, err := p.Generate(strings.Repeat("9", 64)); err != nil {
		t.Errorf("64-character number: %v", err)
	}
}

// Signed and countersigned copies: recorded once each for a current document, never the generated
// bytes, never for a stale document.
func TestSignedCopies(t *testing.T) {
	r1, r2 := revisionOne(), revisionTwo(t)
	sc := render(t, r1, contractTemplate(), facts(KindSalesContract), "SC-2026-00007")
	signed := Signature{Kind: Signed, FileID: "file.sc.signed", FileSHA256: quote.Hash([]byte("signed scan")), Recorded: quote.Act{By: clerk, At: generatedAt}}
	back := Signature{Kind: Countersigned, FileID: "file.sc.back", FileSHA256: quote.Hash([]byte("countersigned scan")), Recorded: quote.Act{By: clerk, At: generatedAt}}
	if replay, err := Sign(&sc.Binding, r1, signed, nil); err != nil || replay {
		t.Fatalf("sign: %v %v", replay, err)
	}
	if replay, err := Sign(&sc.Binding, r1, back, []Signature{signed}); err != nil || replay {
		t.Fatalf("countersign: %v %v", replay, err)
	}
	if replay, err := Sign(&sc.Binding, r1, back, []Signature{signed, back}); err != nil || !replay {
		t.Fatalf("countersign again: %v %v", replay, err)
	}
	other := back
	other.FileID, other.FileSHA256 = "file.sc.back2", quote.Hash([]byte("another scan"))
	ai := signed
	ai.Recorded.By = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent"}
	draft := signed
	draft.FileSHA256 = sc.File.SHA256
	odd := signed
	odd.Kind = "stamped"
	for name, c := range map[string]struct {
		b    *Binding
		r    *orderrev.Revision
		s    Signature
		have []Signature
		want string
	}{
		"another countersigned copy": {&sc.Binding, r1, other, []Signature{signed, back}, "INVALID_STATE at /args/kind"},
		"the unsigned draft":         {&sc.Binding, r1, draft, nil, "INVALID_KNOWN_VALUE at /args/file_id"},
		"a stale document":           {&sc.Binding, r2, signed, nil, "ORDER_DOCUMENT_STALE at /document_binding"},
		"no document":                {nil, r1, signed, nil, "DOCUMENT_BINDING_MISSING at /document_binding"},
		"an unknown kind":            {&sc.Binding, r1, odd, nil, "SCHEMA_INVALID at /args/kind"},
		"an AI recorder":             {&sc.Binding, r1, ai, nil, "PERMISSION_DENIED at /"},
	} {
		if _, err := Sign(c.b, c.r, c.s, c.have); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
}

// The section sets of the two kinds.
func TestSections(t *testing.T) {
	if len(RequiredSections(KindSalesContract)) != 11 || len(RequiredSections(KindProformaInvoice)) != 9 ||
		!slices.Equal(OptionalSections(KindProformaInvoice), []string{SectionClauses, SectionLaw}) || RequiredSections("other") != nil ||
		DocumentType(KindSalesContract) != DocumentTypeOrder || DocumentType(KindProformaInvoice) != DocumentTypeProformaInvoice {
		t.Fatal("section sets")
	}
	if i, ok := CheckSections("other", nil); ok || i != 0 {
		t.Fatalf("unknown kind: %d %v", i, ok)
	}
}

// golden compares got with testdata/name, or writes it when ORDERDOC_WRITE_VECTORS=1.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("ORDERDOC_WRITE_VECTORS") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the golden vector:\n got %s\nwant %s", name, got, want)
	}
	if h, ok := goldenSHA256[name]; !ok || quote.Hash(want) != h {
		t.Fatalf("%s: SHA-256 %s is not the pinned %s", name, quote.Hash(want), h)
	}
}

// goldenSHA256 pins the golden vectors, so a regeneration that changes them fails until reviewed.
var goldenSHA256 = map[string]string{
	"sc-brc001-r1.txt": "b98045cc9123b753e9eb628923366165279cb4688753585f780a13547a0918f6",
	"pi-brc001-r2.txt": "1b32e307df5b323e5a571c95a8d1d003478b81447c7f209731716e7d6010e79c",
	"sc-variant.txt":   "5de2c27300e82994828339cca697434a2ebe181c430765a40d4f5c212462406e",
}
