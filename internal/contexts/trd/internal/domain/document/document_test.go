package document

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func formalize(t *testing.T, r Revision, tpl Template, f Facts, number string) *Formal {
	t.Helper()
	p, err := Prepare(r, tpl, f)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	formal, err := p.Formalize(number)
	if err != nil {
		t.Fatalf("formalize: %v", err)
	}
	return formal
}

// TRD-INV-009 and WBS S3-04 (确定性渲染): the frozen fixture, sealed by the freeze with the server
// calculation, renders the bytes Python's document.render prints for the same values (testdata
// vector "go fixture mirror": Python's example normalized, with this fixture's business hash,
// contact, buyer address, trade terms as scope, delivery and microsecond instants), and the same
// bytes on every run.
func TestFrozenRevisionRendersPythonBytes(t *testing.T) {
	v := renderVectors(t)["go fixture mirror"]
	r := seal(t, nil)
	if r.BusinessHash != v.BusinessHash {
		t.Fatalf("the fixture's business hash %s is not the vector's %s", r.BusinessHash, v.BusinessHash)
	}
	f := formalize(t, r, template(), facts(), v.Number)
	if !bytes.Equal(f.File.Content, []byte(v.ContentText)) {
		t.Fatalf("differs at byte %d:\n%s", firstDifference(f.File.Content, []byte(v.ContentText)), f.File.Content)
	}
	if f.File.SHA256 != v.ContentSHA256 || f.File.Size != v.SizeBytes {
		t.Fatalf("file %s %d", f.File.SHA256, f.File.Size)
	}
	again := formalize(t, seal(t, nil), template(), facts(), v.Number)
	if !bytes.Equal(again.File.Content, f.File.Content) || again.Binding != f.Binding {
		t.Fatal("rendering is not deterministic")
	}
}

// Python verify.py "document-reference" (no internal cost fields, hash and content bytes): the
// document shows neither the cost (66000) nor the profit, margin or cost members, and its SHA-256
// and size are those of its bytes.
func TestDocumentShowsNoInternalCostFields(t *testing.T) {
	f := formalize(t, seal(t, nil), template(), facts(), "SYN-Q-2026-00001")
	text := string(f.File.Content)
	for _, internal := range []string{"66000", "30000", "cost_minor", "cost", "profit", "margin"} {
		if strings.Contains(text, internal) {
			t.Errorf("the document shows %q", internal)
		}
	}
	if f.File.SHA256 != quote.Hash(f.File.Content) || f.File.Size != len(f.File.Content) {
		t.Fatal("hash and size are not the content's")
	}
}

// The document is a function of the business-hashed content: drafts that differ only in the
// lexical form of their decimals freeze to one business hash and render one document, decimals as
// the normalized document holds them.
func TestEquivalentDecimalsRenderOneDocument(t *testing.T) {
	short := seal(t, func(d *quote.Data, _ *quote.Facts) {
		d.Lines[0].Quantity, d.Lines[0].UnitPrice, d.Lines[0].PriceBasisQuantity, d.Lines[0].DiscountRatio = "120", "8", "1", "0"
	})
	long := seal(t, nil)
	if short.BusinessHash != long.BusinessHash {
		t.Fatal("equivalent decimals must freeze to one business hash")
	}
	a := formalize(t, short, template(), facts(), "SYN-Q-2026-00001")
	b := formalize(t, long, template(), facts(), "SYN-Q-2026-00001")
	if !bytes.Equal(a.File.Content, b.File.Content) || !strings.Contains(string(a.File.Content), "| 120 PCS | price 8 per 1 | discount 0 |") {
		t.Fatalf("%s", a.File.Content)
	}
}

// Charges print after the lines, with the amounts the server calculation gives them, in the
// document's order (Python document.render); the recipient is addressed at the buyer address, else
// the billing address, and never at another role's (Python test_t03_a31_address_roles.py
// test_buyer_and_billing_are_bound_as_selected_roles, PYT-0379, the printed half; the sealed half is
// quote's TestCustomerAddressesBindTheirRoles).
func TestChargesAndRecipientAddress(t *testing.T) {
	vat := quote.Ref{Kind: quote.KindTaxProfile, ID: "tax.vat13", Revision: 1}
	charged := seal(t, func(d *quote.Data, f *quote.Facts) {
		d.Addresses = []quote.AddressRef{buyer, billing}
		d.Charges = []quote.Charge{{ChargeID: "C1", FeeCategory: freight, ObligationKey: "freight.export", Amount: "50.00",
			Inclusion: quote.InclusionSeparate, TaxApplication: chargeTax, Evidence: proof}}
		f.Configs[chargeTax] = quote.Config{ContentHash: hashOf("charge"), Nested: []quote.Ref{vat}}
		f.Configs[vat] = quote.Config{ContentHash: hashOf("vat13")}
	}, func(q *calc.Quote, _ *calc.Facts, _ map[string]quote.Stage) {
		q.Charges[0].Tax.Profile = calc.TaxProfile{Ref: quote.CalcRef(vat), Rate: kernel.MustDecimal("0.13"), PriceBasis: calc.TaxExclusive,
			Recovery: calc.Nonrecoverable}
	})
	text := string(formalize(t, charged, template(), facts(), "SYN-Q-2026-00001").File.Content)
	for _, want := range []string{
		"| payable 96000\ncharge C1 | freight.export | separate | net 5000 | tax 650 | payable 5650\n\n",
		"Net: 101000 | Tax: 650 | Payable: 101650",
		"Customer: Synthetic Buyer Ltd\nSynthetic buyer address\n上海\nContact: ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	billed := seal(t, func(d *quote.Data, _ *quote.Facts) { d.Addresses = []quote.AddressRef{billing} })
	if text := string(formalize(t, billed, template(), facts(), "Q-1").File.Content); !strings.Contains(text,
		"Customer: Synthetic Buyer Ltd\nSynthetic billing address\nContact: ") {
		t.Errorf("billing address:\n%s", text)
	}
	if _, ok := recipientAddress([]quote.AddressSnapshot{{AddressID: "x", Role: "consignee", Address: "a"}}); ok {
		t.Error("a consignee address does not address a quote")
	}
}

// Forms beyond Python's (package doc): a stage of payment terms without a day basis counts
// calendar days and one without a calendar prints no calendar; a lead-time delivery prints its
// days, day basis, anchor and calendar in place of the date.
func TestFormsBeyondPython(t *testing.T) {
	anchor := quote.BusinessRef{Kind: "delivery_anchor", ID: "order_confirmed", Revision: 1}
	cn := quote.Ref{Kind: quote.KindCalendar, ID: "cal.cn", Revision: 2}
	r := seal(t, func(d *quote.Data, f *quote.Facts) {
		d.Delivery = &quote.Delivery{Kind: "lead_time", Anchor: &anchor, Days: 30, DayBasis: "working", Calendar: &cn,
			Milestone: "goods_ready", Place: deham, TimeZone: "Asia/Shanghai"}
		f.Configs[cn] = quote.Config{ContentHash: hashOf("cal.cn")}
	}, func(q *calc.Quote, f *calc.Facts, stages map[string]quote.Stage) {
		f.Cost.Delivery = q.Delivery // the costing compared its candidates on the same lead time
		stages["P1"] = quote.Stage{Trigger: "before_shipment"}
	})
	text := string(formalize(t, r, template(), facts(), "Q-1").File.Content)
	for _, want := range []string{
		"\nP1 | 100% | 96000 | before_shipment | 0 calendar days\n\n",
		"\nDelivery: 30 working days after order_confirmed r1 | calendar cal.cn r2 | goods_ready | Asia/Shanghai\n\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	working := seal(t, nil, func(_ *calc.Quote, _ *calc.Facts, stages map[string]quote.Stage) {
		stages["P1"] = quote.Stage{Trigger: "before_shipment", DayBasis: "working", Calendar: &calendar}
	})
	text = string(formalize(t, working, template(), facts(), "Q-1").File.Content)
	if !strings.Contains(text, "\nDelivery: 2027-01-31 | handover | Asia/Shanghai\n\n") ||
		!strings.Contains(text, "| 0 working days | calendar payment.calendar r1\n") {
		t.Errorf("%s", text)
	}
}

// WBS S3-04 (文件或编号失败整体回滚): the plan names the number before anything is rendered, and
// Formalize binds number, file and template to the revision; the binding's evidence entries come
// in a fixed order and the stored document verifies.
func TestPlanAndFormalization(t *testing.T) {
	r := seal(t, nil)
	p, err := Prepare(r, template(), facts())
	if err != nil {
		t.Fatal(err)
	}
	if p.QuoteID != "quote.1" || p.BusinessRevision != 1 || p.BusinessHash != r.BusinessHash || p.Template != templateRef ||
		p.TemplateHash != hashOf("template") {
		t.Fatalf("plan %+v", p)
	}
	if p.Number != (NumberRequest{Sequence: sequence, DocumentType: "quote", BusinessRef: "quote.1:1"}) {
		t.Fatalf("number request %+v", p.Number)
	}
	f, err := p.Formalize("SYN-Q-2026-00001")
	if err != nil {
		t.Fatal(err)
	}
	if f.File.Name != "SYN-Q-2026-00001.txt" || f.File.MimeType != "text/plain" || f.File.SHA256 != quote.Hash(f.File.Content) ||
		f.File.Size != len(f.File.Content) {
		t.Fatalf("file %+v", f.File)
	}
	want := Binding{QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: r.BusinessHash, Number: "SYN-Q-2026-00001",
		Sequence: sequence, BusinessRef: "quote.1:1", Template: templateRef, TemplateHash: hashOf("template"),
		RendererVersion: "quote-text/0.1.0", FileName: "SYN-Q-2026-00001.txt", MimeType: "text/plain", ContentSHA256: f.File.SHA256,
		SizeBytes: f.File.Size}
	if f.Binding != want {
		t.Fatalf("binding %+v", f.Binding)
	}
	var names []string
	for _, e := range f.Binding.Entries() {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"quote_id", "business_revision", "business_hash", "number", "sequence_ref", "business_ref",
		"template_ref", "template_content_hash", "renderer_version", "file_name", "mime_type", "content_sha256", "size_bytes"}) {
		t.Fatalf("entries %v", names)
	}
	if e := f.Binding.Entries(); e[4].Value != "number_sequence:quote.sequence@1" || e[12].Value != "1070" {
		t.Fatalf("entries %v", e)
	}
	if doc, err := json.Marshal(f.Binding); err != nil || !strings.Contains(string(doc), `"template_ref":{"kind":"quote_document_template"`) {
		t.Fatalf("%s %v", doc, err)
	}
	if err = Verify(&f.Binding, f.File.Content, "quote.1", 1, r.BusinessHash); err != nil {
		t.Fatal(err)
	}
	// The plan stays as it was: another number renders another document from the same plan.
	other, err := p.Formalize("SYN-Q-2026-00002")
	if err != nil || !strings.Contains(string(other.File.Content), "Number: SYN-Q-2026-00002\n") ||
		strings.Contains(string(other.File.Content), "00001") {
		t.Fatalf("%v", err)
	}
}

// A number POL could not have issued is refused before anything is bound.
func TestFormalizeRefusesMalformedNumbers(t *testing.T) {
	p, err := Prepare(seal(t, nil), template(), facts())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"", "QT:2026-1", "QT/2026-1", "QT 2026", "-QT-1", "报价-1", strings.Repeat("9", 65), "QT\n1"} {
		if _, err := p.Formalize(n); code(err) != "REFERENCE_INVALID at /number" {
			t.Errorf("%q: %s", n, code(err))
		}
	}
	if _, err := p.Formalize(strings.Repeat("9", 64)); err != nil {
		t.Error(err)
	}
}

// The business reference of a revision's number: Python's "<quote_id>:<revision>", or a hashed
// form without ':' when that is longer than an identifier.
func TestBusinessRef(t *testing.T) {
	if got := BusinessRef("quote.1", 12); got != "quote.1:12" {
		t.Fatal(got)
	}
	long := strings.Repeat("q", 62)
	got := BusinessRef(long, 123)
	if len(got) != 64 || !strings.HasPrefix(got, "quote-") || strings.Contains(got, ":") || !kernel.ValidIdentifier(got) ||
		got != BusinessRef(long, 123) || got == BusinessRef(long, 124) {
		t.Fatal(got)
	}
	if got := BusinessRef(strings.Repeat("q", 61), 99); got != strings.Repeat("q", 61)+":99" {
		t.Fatal(got)
	}
}

// edited returns r's commercial document changed by edit, normalized and hashed again: a stored
// document no freeze would have sealed.
func edited(t *testing.T, r Revision, edit func(doc map[string]any)) Revision {
	t.Helper()
	tree, err := quote.Tree(r.Commercial)
	if err != nil {
		t.Fatal(err)
	}
	edit(tree.(map[string]any))
	canonical, hash, err := quote.Digest(tree)
	if err != nil {
		t.Fatal(err)
	}
	return Revision{Commercial: canonical, BusinessHash: hash}
}

func member(doc map[string]any, path ...string) map[string]any {
	for _, p := range path {
		doc = doc[p].(map[string]any)
	}
	return doc
}

// Prepare's refusals, each before a number is asked for (WBS S3-04: a formalization that cannot
// complete takes no number): the stored document, its calculation, the template's binding and
// support, every fact, and the size.
func TestPrepareRefusals(t *testing.T) {
	good := seal(t, nil)
	indented := new(bytes.Buffer)
	if err := json.Indent(indented, good.Commercial, "", " "); err != nil {
		t.Fatal(err)
	}
	consignee := edited(t, good, func(doc map[string]any) {
		member(doc, "party_snapshots", "customer")["addresses"].([]any)[0].(map[string]any)["role"] = "consignee"
	})
	unpinned := edited(t, good, func(doc map[string]any) {
		var kept []any
		for _, d := range doc["dependency_manifest"].([]any) {
			if d.(map[string]any)["kind"] != quote.KindQuoteDocumentTemplate {
				kept = append(kept, d)
			}
		}
		doc["dependency_manifest"] = kept
	})
	unfit := edited(t, good, func(doc map[string]any) {
		member(doc, "calculation")["payments"].([]any)[0].(map[string]any)["amount_minor"] = "1"
	})
	uncalculated := edited(t, good, func(doc map[string]any) { doc["calculation"] = nil })
	delivery := func(edit func(d map[string]any)) Revision {
		return edited(t, good, func(doc map[string]any) { edit(member(doc, "data", "delivery")) })
	}
	leadTime := func(days int64) func(d map[string]any) {
		return func(d map[string]any) {
			delete(d, "date")
			d["kind"], d["days"], d["day_basis"] = "lead_time", days, "calendar"
			d["calendar_ref"] = map[string]any{"kind": "calendar", "id": "payment.calendar", "revision": int64(1)}
		}
	}
	cases := []struct {
		name string
		rev  Revision
		tpl  func(*Template)
		fact func(*Facts)
		want string
	}{
		{name: "another business hash", rev: Revision{Commercial: good.Commercial, BusinessHash: hashOf("other")},
			want: "BUSINESS_HASH_MISMATCH at /business_hash"},
		{name: "not the normalized canonical form", rev: Revision{Commercial: indented.Bytes(), BusinessHash: quote.Hash(indented.Bytes())},
			want: "BUSINESS_HASH_MISMATCH at /business_hash"},
		{name: "no server calculation", rev: uncalculated, want: "REFERENCE_INVALID at /calculation"},
		{name: "a calculation that does not fit", rev: unfit, want: "INVALID_KNOWN_VALUE at /calculation/payments"},
		{name: "a template the manifest does not pin", rev: unpinned, want: "REFERENCE_INVALID at /data/document_template_ref"},
		{name: "no buyer or billing address", rev: consignee, want: "REFERENCE_INVALID at /party_snapshots/customer/addresses"},
		{name: "another template revision", tpl: func(t *Template) { t.Ref.Revision = 2 }, want: "REFERENCE_INVALID at /data/document_template_ref"},
		{name: "a plain-text template", tpl: func(t *Template) { t.Ref.Kind = "document_template" }, want: "REFERENCE_INVALID at /data/document_template_ref"},
		{name: "template content not pinned", tpl: func(t *Template) { t.ContentHash = hashOf("changed") }, want: "REFERENCE_STALE at /data/document_template_ref"},
		{name: "another renderer", tpl: func(t *Template) { t.RendererVersion = "quote-text/0.3.0" }, want: "DEPENDENCY_NOT_SUPPORTED at /template/renderer_version"},
		{name: "nine sections", tpl: func(t *Template) { t.Sections = t.Sections[1:] }, want: "REFERENCE_INVALID at /template/sections"},
		{name: "a repeated section", tpl: func(t *Template) { t.Sections[9] = "issuer" }, want: "REFERENCE_INVALID at /template/sections/9"},
		{name: "an unknown section", tpl: func(t *Template) { t.Sections[3] = "cost" }, want: "REFERENCE_INVALID at /template/sections/3"},
		{name: "zh-CN", tpl: func(t *Template) { t.Language = "zh-CN" }, want: "DEPENDENCY_NOT_SUPPORTED at /template/language"},
		{name: "another customer revision", fact: func(f *Facts) { f.Customer.Ref.Revision = 1 }, want: "REFERENCE_INVALID at /data/customer_ref"},
		{name: "no customer name", fact: func(f *Facts) { f.Customer.Name = " " }, want: "REFERENCE_INVALID at /data/customer_ref"},
		{name: "another contact row version", fact: func(f *Facts) { f.Contact.Ref.RowVersion = 3 }, want: "REFERENCE_INVALID at /data/contact_ref"},
		{name: "no contact name", fact: func(f *Facts) { f.Contact.Name = "" }, want: "REFERENCE_INVALID at /data/contact_ref"},
		{name: "no item", fact: func(f *Facts) { f.Items = nil }, want: "REFERENCE_INVALID at /data/lines/0/sku_ref"},
		{name: "no specification label", fact: func(f *Facts) { f.Items[sku1] = Item{Name: "合成商品"} }, want: "REFERENCE_INVALID at /data/lines/0/sku_ref"},
		{name: "another currency", fact: func(f *Facts) { f.Currency.Ref.ID = "EUR" }, want: "REFERENCE_INVALID at /data/currency_ref"},
		{name: "currency content not pinned", fact: func(f *Facts) { f.Currency.ContentHash = hashOf("eur") }, want: "REFERENCE_STALE at /data/currency_ref"},
		{name: "five minor units", fact: func(f *Facts) { f.Currency.MinorUnits = 5 }, want: "REFERENCE_INVALID at /data/currency_ref"},
		{name: "no currency fact", fact: func(f *Facts) { f.Currency = Currency{} }, want: "REFERENCE_INVALID at /data/currency_ref"},
		{name: "another trade terms revision", fact: func(f *Facts) { f.TradeTerms.Ref.Revision = 2 }, want: "REFERENCE_INVALID at /data/trade_terms_ref"},
		{name: "trade terms content not pinned", fact: func(f *Facts) { f.TradeTerms.ContentHash = "" }, want: "REFERENCE_STALE at /data/trade_terms_ref"},
		{name: "no Incoterm", fact: func(f *Facts) { f.TradeTerms.Incoterm = "" }, want: "REFERENCE_INVALID at /data/trade_terms_ref"},
		{name: "a delivery of no known kind", rev: delivery(func(d map[string]any) { d["kind"] = "fixed_date" }),
			want: "REFERENCE_INVALID at /data/delivery"},
		{name: "a date that does not exist", rev: delivery(func(d map[string]any) { d["date"] = "2026-02-30" }),
			want: "REFERENCE_INVALID at /data/delivery"},
		{name: "no milestone", rev: delivery(func(d map[string]any) { d["milestone"] = "" }), want: "REFERENCE_INVALID at /data/delivery"},
		{name: "no time zone", rev: delivery(func(d map[string]any) { delete(d, "time_zone") }), want: "REFERENCE_INVALID at /data/delivery"},
		{name: "a lead time without anchor", rev: delivery(leadTime(30)), want: "REFERENCE_INVALID at /data/delivery"},
		{name: "a lead time beyond ten years", rev: delivery(func(d map[string]any) {
			leadTime(3651)(d)
			d["anchor_ref"] = map[string]any{"kind": "delivery_anchor", "id": "order_confirmed", "revision": int64(1)}
		}), want: "REFERENCE_INVALID at /data/delivery"},
		{name: "a name that is not UTF-8", fact: func(f *Facts) { f.Contact.Name = "Wang \xff" }, want: "REFERENCE_INVALID at /"},
		{name: "over 1 MiB", tpl: func(t *Template) { t.FooterNote = strings.Repeat("页", MaxDocumentBytes/3) }, want: "CALCULATION_LIMIT_EXCEEDED at /"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rev, tpl, f := good, template(), facts()
			if c.rev.Commercial != nil {
				rev = c.rev
			}
			if c.tpl != nil {
				c.tpl(&tpl)
			}
			if c.fact != nil {
				c.fact(&f)
			}
			p, err := Prepare(rev, tpl, f)
			if got := code(err); got != c.want || p != nil {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}

// Prepare budgets the longest number: a document that fits with a 64-character number is prepared
// and formalizes at exactly MaxDocumentBytes; one byte more is refused before any number.
func TestPrepareBudgetsTheLongestNumber(t *testing.T) {
	r, tpl := seal(t, nil), template()
	base := formalize(t, r, tpl, facts(), strings.Repeat("9", 64)).File.Size
	tpl.FooterNote += strings.Repeat("x", MaxDocumentBytes-base)
	f := formalize(t, r, tpl, facts(), strings.Repeat("9", 64))
	if f.File.Size != MaxDocumentBytes {
		t.Fatalf("%d bytes", f.File.Size)
	}
	tpl.FooterNote += "x"
	if _, err := Prepare(r, tpl, facts()); code(err) != "CALCULATION_LIMIT_EXCEEDED at /" {
		t.Fatal(code(err))
	}
}

// Python document.py and t03_config.py: renderer quote-text/0.1.0, the ten sections once each in
// any order, language en.
func TestTemplateSupported(t *testing.T) {
	reversed := template()
	slices.Reverse(reversed.Sections)
	if err := reversed.Supported(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(Sections(), []string{"issuer", "recipient", "identity", "lines", "totals", "terms", "delivery", "payments", "validity", "notes"}) {
		t.Fatal(Sections())
	}
	both := template()
	both.RendererVersion, both.Language = "quote-pdf/1.0.0", "zh-CN"
	if code(both.Supported()) != "DEPENDENCY_NOT_SUPPORTED at /template/renderer_version" {
		t.Fatal("the renderer version is judged first")
	}
}

// TRD-INV-009 (读取时校验实际字节; BC-AT22): a stored document verifies only against the binding of
// its own revision and only with its exact bytes.
func TestVerify(t *testing.T) {
	r := seal(t, nil)
	f := formalize(t, r, template(), facts(), "SYN-Q-2026-00001")
	changed := bytes.Clone(f.File.Content)
	changed[len(changed)-2] ^= 1
	other := f.Binding
	other.BusinessHash = hashOf("other content")
	cases := []struct {
		name     string
		binding  *Binding
		content  []byte
		quote    string
		revision int64
		want     string
	}{
		{"the revision's own document", &f.Binding, f.File.Content, "quote.1", 1, "OK"},
		{"no binding", nil, f.File.Content, "quote.1", 1, "DOCUMENT_BINDING_MISSING at /document_binding"},
		{"r2 reading r1's document (BC-AT22)", &f.Binding, f.File.Content, "quote.1", 2, "DOCUMENT_BINDING_MISSING at /document_binding"},
		{"another quote's document", &f.Binding, f.File.Content, "quote.2", 1, "DOCUMENT_BINDING_MISSING at /document_binding"},
		{"a binding of other business content", &other, f.File.Content, "quote.1", 1, "BUSINESS_HASH_MISMATCH at /document_binding/business_hash"},
		{"no bytes", &f.Binding, nil, "quote.1", 1, "FORMAL_DOCUMENT_MISSING at /document"},
		{"a changed byte", &f.Binding, changed, "quote.1", 1, "FORMAL_DOCUMENT_INTEGRITY_FAILURE at /document"},
		{"truncated", &f.Binding, f.File.Content[:f.File.Size-1], "quote.1", 1, "FORMAL_DOCUMENT_INTEGRITY_FAILURE at /document"},
	}
	for _, c := range cases {
		if got := code(Verify(c.binding, c.content, c.quote, c.revision, r.BusinessHash)); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// PYT-0135 test_number_and_plaintext_template_bind_snapshot (rendering half; the number and the
// template's fixed fields are POL's, TestSameBusinessReferenceKeepsItsNumber and
// TestPlainTextTemplateNamesFixedFieldsOnly): the template renders with the issued number into
// "<number>.txt", text/plain, behind the draft banner; the unsafe template of the same test is
// never rendered.
func TestPlainTextTemplateRendersFixedFields(t *testing.T) {
	tpl := PlainTemplate{DocumentType: "quote", Text: "报价 {document.number}：{customer.name}，{quote.total} {quote.currency}",
		FieldNames: []string{"document.number", "customer.name", "quote.total", "quote.currency"}}
	f, err := RenderPlain(tpl, Number{Value: "QT-2026-00001", DocumentType: "quote"},
		map[string]string{"customer.name": "合成客户", "quote.total": "960.00", "quote.currency": "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if string(f.Content) != "DRAFT / 业务草稿（未绑定已生效报价）\n报价 QT-2026-00001：合成客户，960.00 USD" || f.Name != "QT-2026-00001.txt" ||
		f.MimeType != "text/plain" || f.SHA256 != quote.Hash(f.Content) || f.Size != len(f.Content) {
		t.Fatalf("%+v", f)
	}
	unsafe := PlainTemplate{DocumentType: "quote", Text: "{customer.name.__class__}", FieldNames: []string{"customer.name"}}
	if _, err := RenderPlain(unsafe, Number{Value: "QT-2026-00001", DocumentType: "quote"}, map[string]string{"customer.name": "x"}); code(err) !=
		"REFERENCE_INVALID at /template/template_text" {
		t.Fatal(code(err))
	}
	if !slices.Equal(TemplateFields(), []string{"company.name", "customer.name", "document.number", "document.date", "lines.summary",
		"quote.total", "quote.currency"}) {
		t.Fatal(TemplateFields())
	}
}

// Python document_render's refusals (REFERENCE_INVALID for another document type,
// INVALID_KNOWN_VALUE for another document.number, DRAFT_NOT_READY for a missing value) and the
// template forms POL refuses as TEMPLATE_INVALID when they are saved.
func TestPlainTextRefusals(t *testing.T) {
	tpl := PlainTemplate{DocumentType: "quote", Text: "{document.number} {customer.name} {quote.total}",
		FieldNames: []string{"document.number", "customer.name", "quote.total"}}
	values := map[string]string{"customer.name": "c", "quote.total": "1.00"}
	number := Number{Value: "QT-2026-00001", DocumentType: "quote"}
	cases := []struct {
		name   string
		edit   func(t *PlainTemplate, n *Number, v map[string]string)
		wanted string
	}{
		{"a format specification", func(t *PlainTemplate, _ *Number, _ map[string]string) { t.Text = "{quote.total:>10}" }, "REFERENCE_INVALID at /template/template_text"},
		{"a conversion", func(t *PlainTemplate, _ *Number, _ map[string]string) { t.Text = "{customer.name!r}" }, "REFERENCE_INVALID at /template/template_text"},
		{"an index", func(t *PlainTemplate, _ *Number, _ map[string]string) { t.Text = "{0}" }, "REFERENCE_INVALID at /template/template_text"},
		{"an unknown field", func(t *PlainTemplate, _ *Number, _ map[string]string) { t.Text = "{customer.secret}" }, "REFERENCE_INVALID at /template/template_text"},
		{"an open brace", func(t *PlainTemplate, _ *Number, _ map[string]string) { t.Text = "a { b" }, "REFERENCE_INVALID at /template/template_text"},
		{"a lone closing brace", func(t *PlainTemplate, _ *Number, _ map[string]string) { t.Text = "a } b" }, "REFERENCE_INVALID at /template/template_text"},
		{"text that is not UTF-8", func(t *PlainTemplate, _ *Number, _ map[string]string) { t.Text = "\xff{document.number}" }, "REFERENCE_INVALID at /template/template_text"},
		{"an undeclared field", func(t *PlainTemplate, _ *Number, _ map[string]string) { t.FieldNames = t.FieldNames[:2] }, "REFERENCE_INVALID at /template/field_names"},
		{"an unused declared field", func(t *PlainTemplate, _ *Number, _ map[string]string) {
			t.FieldNames = append(t.FieldNames, "document.date")
		},
			"REFERENCE_INVALID at /template/field_names"},
		{"a number of another type", func(_ *PlainTemplate, n *Number, _ map[string]string) { n.DocumentType = "order" }, "REFERENCE_INVALID at /template/document_type"},
		{"a malformed number", func(_ *PlainTemplate, n *Number, _ map[string]string) { n.Value = "../QT" }, "REFERENCE_INVALID at /number"},
		{"another document.number", func(_ *PlainTemplate, _ *Number, v map[string]string) { v["document.number"] = "QT-2026-00002" },
			"INVALID_KNOWN_VALUE at /values/document.number"},
		{"a missing value", func(_ *PlainTemplate, _ *Number, v map[string]string) { delete(v, "quote.total") }, "DRAFT_NOT_READY at /values/quote.total"},
		{"a value that is not UTF-8", func(_ *PlainTemplate, _ *Number, v map[string]string) { v["customer.name"] = "\xc3" },
			"INVALID_KNOWN_VALUE at /values/customer.name"},
		{"over the file limit", func(_ *PlainTemplate, _ *Number, v map[string]string) {
			v["customer.name"] = strings.Repeat("x", MaxPlainBytes)
		},
			"CALCULATION_LIMIT_EXCEEDED at /values"},
		{"the same document.number", func(_ *PlainTemplate, _ *Number, v map[string]string) { v["document.number"] = "QT-2026-00001" }, "OK"},
	}
	for _, c := range cases {
		tp, n, v := tpl, number, map[string]string{}
		tp.FieldNames = slices.Clone(tpl.FieldNames)
		for k, x := range values {
			v[k] = x
		}
		c.edit(&tp, &n, v)
		if _, err := RenderPlain(tp, n, v); code(err) != c.wanted {
			t.Errorf("%s: %s, want %s", c.name, code(err), c.wanted)
		}
	}
	// The limit counts what follows the banner: exactly MaxPlainBytes renders.
	exact := PlainTemplate{DocumentType: "quote", Text: "{customer.name}", FieldNames: []string{"customer.name"}}
	f, err := RenderPlain(exact, number, map[string]string{"customer.name": strings.Repeat("x", MaxPlainBytes)})
	if err != nil || f.Size != MaxPlainBytes+len(DraftBanner) {
		t.Fatalf("%v", err)
	}
}

// Error catalog V0.1: the rows this package decides, with their categories; the codes it shares
// with packages quote and calc have the same category there.
func TestCodesFollowTheErrorCatalog(t *testing.T) {
	want := map[string]kernel.Category{
		"DEPENDENCY_NOT_SUPPORTED": kernel.CategoryInvalid, "CALCULATION_LIMIT_EXCEEDED": kernel.CategoryInvalid,
		"BUSINESS_HASH_MISMATCH": kernel.CategoryInvalid, "REFERENCE_STALE": kernel.CategoryConflict,
		"FORMAL_DOCUMENT_MISSING": kernel.CategoryInvalid, "FORMAL_DOCUMENT_INTEGRITY_FAILURE": kernel.CategoryInvalid,
		"DOCUMENT_BINDING_MISSING": kernel.CategoryInvalid,
	}
	if len(Categories) != len(want) {
		t.Fatalf("%d codes", len(Categories))
	}
	for c, category := range want {
		if Categories[c] != category {
			t.Errorf("%s is %q", c, Categories[c])
		}
		if rej, ok := kernel.AsRejection(fail(c, "/x", "")); !ok || rej.Code != c || rej.Category != category {
			t.Errorf("fail(%s) = %v", c, rej)
		}
		if q, ok := quote.Categories[c]; ok && q != category {
			t.Errorf("%s: quote says %q", c, q)
		}
		if k, ok := calc.Categories[c]; ok && k != category {
			t.Errorf("%s: calc says %q", c, k)
		}
	}
	if rej, _ := kernel.AsRejection(fail(kernel.CodeDraftNotReady, "/values/x", "")); rej.Category != kernel.CategoryInvalid {
		t.Error("shared codes keep the kernel's category")
	}
}
