package shipmentdocumentset

import (
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The shipment document set's rules (WBS S5-08; FUL-INV-004, SM-FUL-DOCUMENT, BT12, CN-AT17,
// INT-05). The fixture is a synthetic shipment of two lines of two SKUs (S1: 50 PCS of sku.1 at
// 8.0000 USD from an order line of 120 PCS worth 960.00; S2: 24 SET of sku.2 at 12.5 USD from 24
// SET worth 300.00 with 39.00 tax), its actual packing of 5 + 2 cartons, a booking, the order's
// FCA terms with a 30/70 payment plan and a consignee address, and CI and PL templates in en and
// zh-CN. Python V0.50 has no fulfilment context: the golden files come from the renderer itself
// and are pinned by their SHA-256.

var at = time.Date(2026, 11, 12, 8, 30, 0, 123456000, time.UTC)

func dec(s string) kernel.Decimal { return kernel.MustDecimal(s) }

func ptr(s string) *kernel.Decimal {
	d := dec(s)
	return &d
}

func fixtureShipment() *shipment.Shipment {
	return &shipment.Shipment{ID: "shipment.1", Status: shipment.StatusPrepared, RowVersion: 2, Route: shipment.RouteSupplierDirect,
		Order: shipment.OrderBinding{OrderID: "order.1", OrderRevision: 1, OrderHash: strings.Repeat("a", 64)}, CustomerID: "customer.1",
		Lines: []shipment.Line{
			{ID: "S1", OrderLineID: "L1", SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS", UnitRevision: 1, OrderQuantity: dec("120"), Quantity: dec("50")},
			{ID: "S2", OrderLineID: "L2", SkuID: "sku.2", SpecRevision: 3, UnitID: "SET", UnitRevision: 1, OrderQuantity: dec("24"), Quantity: dec("24")}},
		Parties: []shipment.Party{{Role: shipment.RoleCustomer, PartyID: "customer.1", PartyRevision: 2}},
		Actual: &shipment.Packing{Kind: shipment.KindActual, Seq: 1, Packages: []shipment.Package{
			{Cartons: 5, CartonFrom: 1, Contents: []shipment.Content{{LineID: "S1", PerCarton: dec("10")}}, Gross: ptr("10.5"), Net: ptr("9.5"),
				Volume: ptr("0.06")},
			{Cartons: 2, CartonFrom: 6, Contents: []shipment.Content{{LineID: "S2", PerCarton: dec("12")}}, Gross: ptr("8"), Net: ptr("7.25"),
				Volume: ptr("0.05")}}}}
}

func fixtureFacts() Facts {
	return Facts{Shipment: fixtureShipment(), Prices: map[string]shipment.Price{
		"S1": {CurrencyID: "USD", CurrencyRevision: 1, UnitPrice: dec("8.0000"), Net: big.NewInt(96000), Tax: big.NewInt(0), Payable: big.NewInt(96000)},
		"S2": {CurrencyID: "USD", CurrencyRevision: 1, UnitPrice: dec("12.5"), Net: big.NewInt(30000), Tax: big.NewInt(3900), Payable: big.NewInt(33900)}},
		Booking: &shipment.Booking{Version: 1, Number: "SHA2611001", Vessel: "SYNTHETIC STAR", Voyage: "026E", ETD: "2026-12-20",
			PortOfLoading: "CNSHA", PortOfDischarge: "DEHAM"}}
}

func fixtureUpstream(number string) Upstream {
	return Upstream{Order: &OrderFacts{Binding: OrderBinding{OrderID: "order.1", OrderRevision: 1, OrderHash: strings.Repeat("a", 64)}, Incoterm: "FCA",
		Edition: "Incoterms 2020", NamedPlace: "DEHAM", Stages: []Stage{{StageID: "P1", Percent: "30", Trigger: "order_confirmed", Days: 0},
			{StageID: "P2", Percent: "70", Trigger: "after_shipment", Days: 30, DayBasis: "calendar"}},
		Addresses: []Address{{Role: "buyer", PartyID: "customer.1", PartyRevision: 2, Text: "Synthetic buyer address\nHamburg"},
			{Role: "consignee", PartyID: "consignee.1", PartyRevision: 1, Text: "Synthetic warehouse\nHamburg port"}}},
		Names:    map[PartyKey]string{{PartyID: "customer.1", Revision: 2}: "Synthetic Buyer GmbH", {PartyID: "consignee.1", Revision: 1}: "Synthetic Logistics KG"},
		Currency: &Currency{Code: "USD", Revision: 1, MinorUnits: 2}, Found: map[string]bool{"evidence": true}, Number: number}
}

var (
	ciSections = []string{"seller", "buyer", "consignee", "notify", "identity", "shipment", "lines", "totals", "terms", "marks", "clauses",
		"signatures", "notes"}
	plSections = []string{"seller", "buyer", "consignee", "identity", "shipment", "packages", "totals", "marks", "signatures", "notes"}
)

func template(kind, language, title string, sections, clauses []string) Template {
	return Template{Ref: Ref{Kind: KindShipmentDocumentTemplate, ID: kind + "." + language, Revision: 1}, Hash: strings.Repeat("b", 64),
		DocumentKind: kind, Language: language, RendererVersion: RendererVersion, Title: title, Sections: sections, Clauses: clauses,
		FooterNote: "Synthetic data. Not an actual trade. 合成数据。"}
}

func request(kind string) Request {
	ts := []Template{template(KindCommercialInvoice, LanguageEn, "COMMERCIAL INVOICE", ciSections, []string{"We certify the goods are of Chinese origin."}),
		template(KindCommercialInvoice, LanguageZhCN, "商业发票", ciSections, []string{"兹证明货物原产于中国。"})}
	if kind == KindPackingList {
		ts = []Template{template(KindPackingList, LanguageEn, "PACKING LIST", plSections, nil), template(KindPackingList, LanguageZhCN, "装箱单",
			plSections, nil)}
	}
	return Request{Kind: kind, Templates: ts,
		Sequence: &Sequence{Ref: Ref{Kind: KindNumberSequence, ID: "seq", Revision: 1}, Hash: strings.Repeat("c", 64), DocumentType: DocumentType(kind)},
		Seller: &Seller{Ref: Ref{Kind: KindCompanyProfile, ID: "company.A", Revision: 1}, Hash: strings.Repeat("d", 64),
			LegalName: "SYNTHETIC Exporter Co., Ltd.", RegistrationNo: "SYNTHETIC-NOT-REAL", Country: "CN", Address: "Synthetic Road 1\nShanghai"},
		Particulars: Particulars{Notify: &Party{Name: "Synthetic Buyer GmbH", Address: "Synthetic buyer address\nHamburg"},
			Marks: "SYNTHETIC BUYER\nHAMBURG\nC/NO. 1-7", Names: map[string]map[string]string{
				"sku.1": {LanguageEn: "Cotton shirt, red", LanguageZhCN: "红色棉质衬衫"}, "sku.2": {LanguageEn: "Gift set", LanguageZhCN: "礼品套装"}},
			NameOrder: []NameRef{{"sku.1", LanguageEn}, {"sku.1", LanguageZhCN}, {"sku.2", LanguageEn}, {"sku.2", LanguageZhCN}}},
		Evidence: []string{"evidence"}, IdempotencyKey: "k1"}
}

func act() Act {
	return Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "docs.1"}, Epoch: 3, At: at}
}

func prepared(t *testing.T, s *Set, kind, number string, f Facts) *Document {
	t.Helper()
	d, err := Prepare(s, request(kind), f, fixtureUpstream(number), act())
	if err != nil {
		t.Fatal(err)
	}
	s.Add(d)
	return d
}

func outcome(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code + " " + rej.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

// CN-AT17 and the layout of shipment-text/0.1.0: one content, four renditions pinned as golden
// files; the figures, currency, SKUs, terms' codes and payment percentages of en and zh-CN are the
// same characters; amounts are the shares of the order lines' amounts (50 of 120 PCS of 960.00 is
// 400.00; all 24 SET are 300.00 + 39.00).
func TestRenditionsAreGoldenAndCorrespond(t *testing.T) {
	s := NewSet("docs.1", fixtureShipment(), "", act())
	f := fixtureFacts()
	ci := prepared(t, s, KindCommercialInvoice, "CI-2026-00001", f)
	pl := prepared(t, s, KindPackingList, "PL-2026-00001", f)
	if ci.Version != 1 || ci.Status != Draft || ci.SetVersion != 1 || pl.SetVersion != 2 || s.RowVersion != 2 || s.DocumentCount != 2 {
		t.Fatalf("versions %+v %+v", ci, pl)
	}
	files := map[string]*Rendition{"ci-en.txt": ci.Rendition(LanguageEn), "ci-zh-CN.txt": ci.Rendition(LanguageZhCN),
		"pl-en.txt": pl.Rendition(LanguageEn), "pl-zh-CN.txt": pl.Rendition(LanguageZhCN)}
	for name, r := range files {
		if r == nil || r.Size != len(r.Content) || Hash(r.Content) != r.SHA256 || r.MimeType != MimeText {
			t.Fatalf("%s: %+v", name, r)
		}
		golden(t, name, r.Content)
	}
	if ci.Rendition(LanguageEn).FileName != "CI-2026-00001-v1-en.txt" || pl.Rendition(LanguageZhCN).FileName != "PL-2026-00001-v1-zh-CN.txt" {
		t.Fatal("file names")
	}
	en, zh := string(ci.Rendition(LanguageEn).Content), string(ci.Rendition(LanguageZhCN).Content)
	figures := regexp.MustCompile(`[0-9][0-9.]*%?|USD|PCS|SET|FCA|DEHAM|CNSHA|sku\.[0-9]|CI-2026-00001`)
	if a, b := figures.FindAllString(en, -1), figures.FindAllString(zh, -1); !slices.Equal(a, b) {
		t.Fatalf("the figures differ:\n%v\n%v", a, b)
	}
	for _, want := range []string{"| net 400.00 | tax 0.00 | total 400.00", "| net 300.00 | tax 39.00 | total 339.00",
		"Net: 700.00 | Tax: 39.00 | Total: 739.00", "Quantity: 50 PCS; 24 SET", "Consignee: Synthetic Logistics KG\nSynthetic warehouse",
		"Payment: P2 | 70% | after_shipment | 30 calendar days", "Booking: SHA2611001 | Vessel: SYNTHETIC STAR | Voyage: 026E | ETD: 2026-12-20"} {
		if !strings.Contains(en, want) {
			t.Fatalf("%q not in\n%s", want, en)
		}
	}
	for _, want := range []string{"| 净额 400.00 | 税额 0.00 | 合计 400.00", "付款：P2 | 70% | 出运后 | 30 个日历日", "红色棉质衬衫", "收货人：Synthetic Logistics KG"} {
		if !strings.Contains(zh, want) {
			t.Fatalf("%q not in\n%s", want, zh)
		}
	}
	// The content is language-neutral, hashed and kept with the version.
	if json, sum := Canonical(ci.Content); json != ci.ContentJSON || sum != ci.ContentSHA256 || ci.Content.ConsigneeSource != "order" ||
		ci.Content.PortsSource != "booking" || pl.Content.PackingSource != "actual:1" || pl.Content.Totals.Cartons != 7 ||
		pl.Content.Totals.GrossWeightKg != "68.5" || pl.Content.Totals.NetWeightKg != "62" || pl.Content.Totals.VolumeCbm != "0.4" {
		t.Fatalf("contents %+v / %+v", ci.Content, pl.Content)
	}
}

// golden compares got with testdata/name, or writes it when FUL_DOCS_WRITE_VECTORS=1.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("FUL_DOCS_WRITE_VECTORS") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("%q: %q,", name, Hash(got))
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s differs from the golden file:\n got %s\nwant %s", name, got, want)
	}
	if h, ok := goldenSHA256[name]; !ok || Hash(want) != h {
		t.Fatalf("%s: SHA-256 %s is not the pinned one", name, Hash(want))
	}
}

// goldenSHA256 pins the golden files, so a regeneration that changes them fails until reviewed.
var goldenSHA256 = map[string]string{
	"ci-en.txt":    "92d4caa58644fa92b0ef80fef7e78500e2aa9d6d110b1733989549c6550af5b7",
	"ci-zh-CN.txt": "eac09177f34480d8276b547ff9d3b08761440068bb5fb5002c640953ad1beee9",
	"pl-en.txt":    "bb38c558f3bbbf24e0777048fefce56b9fb4f72c973441bd4e098243f880f6f0",
	"pl-zh-CN.txt": "a3ca15359bb020eb80bd269169601f2d0f10e2f8a052fb83c8621f49b79234db",
}

// Prepare's refusals in their order, and what no number is spent on (CheckLayout).
func TestPrepareRefusals(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Request, *Facts, *Upstream)
		want string
	}{
		{"cancelled", func(_ *Request, f *Facts, _ *Upstream) { f.Shipment.Status = shipment.StatusCancelled }, "INVALID_STATE /args/shipment_id"},
		{"evidence", func(r *Request, _ *Facts, _ *Upstream) { r.Evidence = []string{"hidden"} }, "REFERENCE_INVALID /args/evidence_ids/0"},
		{"template kind", func(r *Request, _ *Facts, _ *Upstream) { r.Templates[1].DocumentKind = KindPackingList }, "REFERENCE_INVALID /args/templates/1"},
		{"renderer", func(r *Request, _ *Facts, _ *Upstream) { r.Templates[0].RendererVersion = "shipment-text/9" },
			"DEPENDENCY_NOT_SUPPORTED /args/templates/0"},
		{"language", func(r *Request, _ *Facts, _ *Upstream) { r.Templates[1].Language = "de" }, "DEPENDENCY_NOT_SUPPORTED /args/templates/1"},
		{"two of a language", func(r *Request, _ *Facts, _ *Upstream) { r.Templates[1].Language = LanguageEn }, "REFERENCE_INVALID /args/templates/1"},
		{"sections", func(r *Request, _ *Facts, _ *Upstream) { r.Templates[0].Sections = r.Templates[0].Sections[1:] },
			"REFERENCE_INVALID /args/templates/0"},
		{"sequence", func(r *Request, _ *Facts, _ *Upstream) { r.Sequence.DocumentType = DocumentTypePackingList }, "REFERENCE_INVALID /args/sequence_ref"},
		{"seller", func(r *Request, _ *Facts, _ *Upstream) { r.Seller.Address = " " }, "REFERENCE_INVALID /args/seller_ref"},
		{"no order", func(_ *Request, _ *Facts, u *Upstream) { u.Order = nil }, "REFERENCE_INVALID /args/shipment_id"},
		{"number", func(_ *Request, _ *Facts, u *Upstream) { u.Number = "" }, "REFERENCE_INVALID /number"},
		{"buyer name", func(_ *Request, _ *Facts, u *Upstream) { delete(u.Names, PartyKey{PartyID: "customer.1", Revision: 2}) },
			"REFERENCE_INVALID /args/shipment_id"},
		{"name of another SKU", func(r *Request, _ *Facts, _ *Upstream) { r.Particulars.NameOrder[2].SkuID = "sku.9" },
			"REFERENCE_INVALID /args/particulars/descriptions/2/sku_id"},
		{"missing name", func(r *Request, _ *Facts, _ *Upstream) { delete(r.Particulars.Names["sku.2"], LanguageZhCN) },
			"REFERENCE_INVALID /args/particulars/descriptions"},
		{"no price", func(_ *Request, f *Facts, _ *Upstream) { delete(f.Prices, "S2") }, "REFERENCE_INVALID /args/shipment_id"},
		{"currency", func(_ *Request, _ *Facts, u *Upstream) { u.Currency.Revision = 2 }, "REFERENCE_INVALID /args/shipment_id"},
	}
	for _, c := range cases {
		r, f, u := request(KindCommercialInvoice), fixtureFacts(), fixtureUpstream("CI-1")
		c.edit(&r, &f, &u)
		_, err := Prepare(NewSet("docs.1", f.Shipment, "", act()), r, f, u, act())
		if got := outcome(err); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	r := request(KindPackingList)
	r.Packages = []shipment.Package{{Cartons: 1, CartonFrom: 1, Contents: []shipment.Content{{LineID: "S9", PerCarton: dec("1")}}, Gross: ptr("1"),
		Net: ptr("1"), Volume: ptr("1")}}
	if _, err := Prepare(NewSet("docs.1", fixtureShipment(), "", act()), r, fixtureFacts(), fixtureUpstream("PL-1"), act()); outcome(err) !=
		"REFERENCE_INVALID /args/packages/0/contents/0/line_id" {
		t.Fatal(outcome(err))
	}
	f := fixtureFacts()
	f.Shipment.Actual = nil
	if _, err := Prepare(NewSet("docs.1", f.Shipment, "", act()), request(KindPackingList), f, fixtureUpstream("PL-1"), act()); outcome(err) !=
		"REFERENCE_INVALID /args/packages" {
		t.Fatal(outcome(err))
	}
	bl := Request{Kind: KindBillOfLading, BillOfLading: &BillOfLading{Number: "B1"}, FileID: "file.bl"}
	if _, err := Prepare(NewSet("docs.1", fixtureShipment(), "", act()), bl, fixtureFacts(), Upstream{}, act()); outcome(err) !=
		"REFERENCE_INVALID /args/bill_of_lading/file_id" {
		t.Fatal(outcome(err))
	}
	d, err := Prepare(NewSet("docs.1", fixtureShipment(), "", act()), bl, fixtureFacts(), Upstream{File: &File{ID: "file.bl", SHA256: strings.Repeat("e", 64),
		MimeType: "application/pdf"}}, act())
	if err != nil || d.Number != "B1" || d.File.ID != "file.bl" || d.Record.BillOfLading == nil || len(d.Renditions) != 0 {
		t.Fatalf("%+v %v", d, err)
	}
}

// BT12 and FUL-INV-004 in the domain: a PL whose cartons hold 40 where the CI and the shipment ship
// 50 shows every difference with its path, which an issue neither resolved nor acknowledged
// refuses; a missing PL is never acknowledged; a resolved PL issues; an acknowledged difference is
// carried by the next issue while its values stay.
func TestReconcileAndIssueBT12(t *testing.T) {
	f := fixtureFacts()
	s := NewSet("docs.1", f.Shipment, "", act())
	prepared(t, s, KindCommercialInvoice, "CI-1", f)
	if diffs := Reconcile(s, f); len(diffs) != 1 || diffs[0].Path != "/packing_list" || !diffs[0].Required {
		t.Fatalf("no PL %+v", diffs)
	}
	if _, _, err := s.Issue(1, shipment.StatusPrepared, Reconcile(s, f), []Ack{{Path: "/packing_list", Reason: "later"}}, act()); outcome(err) !=
		"INVALID_KNOWN_VALUE /args/acknowledgements/0/path" {
		t.Fatal(outcome(err))
	}
	r := request(KindPackingList)
	r.Packages = []shipment.Package{{Cartons: 4, CartonFrom: 1, Contents: []shipment.Content{{LineID: "S1", PerCarton: dec("10")}}, Gross: ptr("10.5"),
		Net: ptr("9.5"), Volume: ptr("0.06")}, f.Shipment.Actual.Packages[1]}
	pl, err := Prepare(s, r, f, fixtureUpstream("PL-1"), act())
	if err != nil {
		t.Fatal(err)
	}
	s.Add(pl)
	diffs := Reconcile(s, f)
	var got []string
	for _, d := range diffs {
		got = append(got, d.Path+" "+d.Found+"/"+d.Expected+" "+d.Against)
	}
	want := []string{"/packing_list/lines/S1/quantity 40/50 shipment_line", "/packing_list/totals/cartons 6/7 actual_packing",
		"/packing_list/totals/gross_weight_kg 58/68.5 actual_packing", "/packing_list/totals/net_weight_kg 52.5/62 actual_packing",
		"/packing_list/totals/quantity/PCS 40/50 commercial_invoice", "/packing_list/totals/volume_cbm 0.34/0.4 actual_packing"}
	if !slices.Equal(got, want) {
		t.Fatalf("differences\n%v\nwant\n%v", got, want)
	}
	if _, _, err := s.Issue(1, shipment.StatusPrepared, diffs, nil, act()); outcome(err) != "VERSION_CONFLICT /args/expected_version" {
		t.Fatal(outcome(err))
	}
	if _, _, err := s.Issue(2, shipment.StatusCancelled, diffs, nil, act()); outcome(err) != "INVALID_STATE /args/document_set_id" {
		t.Fatal(outcome(err))
	}
	if _, _, err := s.Issue(2, shipment.StatusPrepared, diffs, nil, act()); outcome(err) !=
		"SHIPMENT_DOCUMENTS_UNRECONCILED /packing_list/lines/S1/quantity" {
		t.Fatal(outcome(err))
	}
	pl2 := prepared(t, s, KindPackingList, "PL-1", f)
	if pl.Status != Superseded || pl.SupersededAt.IsZero() || pl2.Version != 2 || len(Reconcile(s, f)) != 0 {
		t.Fatalf("PL v1 %s, v2 %d, %+v", pl.Status, pl2.Version, Reconcile(s, f))
	}
	iss, superseded, err := s.Issue(3, shipment.StatusPrepared, nil, nil, act())
	if err != nil || iss.Seq != 1 || iss.SetVersion != 4 || len(iss.Documents) != 2 || len(superseded) != 0 || s.Current(KindPackingList) != pl2 ||
		pl2.Status != Issued || pl2.IssueSeq != 1 {
		t.Fatalf("issue %+v %v", iss, err)
	}
	if _, _, err := s.Issue(4, shipment.StatusPrepared, nil, nil, act()); outcome(err) != "INVALID_STATE /args/document_set_id" {
		t.Fatal(outcome(err))
	}
	// A B/L record with another port: acknowledged once, carried while its values stay.
	bl := &Document{Kind: KindBillOfLading, Version: 1, Number: "B1", Prepared: act(), Record: &Record{BillOfLading: &BillOfLading{Number: "B1",
		Vessel: "SYNTHETIC STAR", Voyage: "026E", PortOfLoading: "CNSHA", PortOfDischarge: "DEBRV",
		Consignee: PartyContent{Name: "SYNTHETIC LOGISTICS KG", Address: "Synthetic warehouse Hamburg port"},
		Notify:    &PartyContent{Name: "Synthetic Buyer GmbH", Address: "Synthetic buyer address\nHamburg"}, Marks: "SYNTHETIC BUYER HAMBURG C/NO. 1-7",
		Packages: 7, GrossWeightKg: "68.5"}}}
	s.Add(bl)
	diffs = Reconcile(s, f)
	if len(diffs) != 1 || diffs[0].Path != "/bill_of_lading/port_of_discharge" || diffs[0].Against != AgainstBooking {
		t.Fatalf("B/L %+v", diffs)
	}
	iss2, _, err := s.Issue(5, shipment.StatusPrepared, diffs, []Ack{{Path: diffs[0].Path, Reason: "carrier calls Bremerhaven"}}, act())
	if err != nil || len(iss2.Acknowledged) != 1 || iss2.Acknowledged[0].FromIssue != 0 {
		t.Fatalf("%+v %v", iss2, err)
	}
	ci2 := prepared(t, s, KindCommercialInvoice, "CI-1", f)
	iss3, superseded, err := s.Issue(7, shipment.StatusPrepared, Reconcile(s, f), nil, act())
	if err != nil || len(iss3.Acknowledged) != 1 || iss3.Acknowledged[0].FromIssue != 2 || len(superseded) != 1 || superseded[0].Version != 1 ||
		ci2.Status != Issued {
		t.Fatalf("carried %+v %v", iss3, err)
	}
}

// The reconciliation's other items: CI lines against the shipment lines and the prices kept, the
// order revision the terms came from, the order's consignee, notify and marks between documents,
// the B/L's packages, weights, vessel and voyage, and a PL printed from an actual packing that was
// replaced since.
func TestReconcileItems(t *testing.T) {
	f := fixtureFacts()
	s := NewSet("docs.1", f.Shipment, "", act())
	ci := prepared(t, s, KindCommercialInvoice, "CI-1", f)
	pl := prepared(t, s, KindPackingList, "PL-1", f)
	ci.Content.Lines[0].Quantity, ci.Content.Lines[0].UnitPrice, ci.Content.Lines[0].NetMinor = "49", "8.1", "39690"
	ci.Content.Lines[1].SkuID = "sku.7"
	ci.Content.Lines = append(ci.Content.Lines, LineContent{LineID: "S9", SkuID: "sku.9", Quantity: "1", UnitID: "PCS"})
	ci.Content.Terms.OrderRevision = 2
	ci.Content.Consignee = PartyContent{Name: "TO ORDER", Address: "-"}
	pl.Content.Marks, pl.Content.Notify = "N/M", nil
	pl.Content.PackingSource = "actual:0"
	f.Booking.PortOfLoading = "CNNGB"
	var got []string
	for _, d := range Reconcile(s, f) {
		got = append(got, d.Path+" "+d.Against)
	}
	want := []string{"/commercial_invoice/consignee order", "/commercial_invoice/lines/S1/amount order_line",
		"/commercial_invoice/lines/S1/quantity shipment_line", "/commercial_invoice/lines/S1/unit_price order_line",
		"/commercial_invoice/lines/S2/sku shipment_line", "/commercial_invoice/lines/S9 shipment_line", "/commercial_invoice/port_of_loading booking",
		"/commercial_invoice/sources/order shipment", "/packing_list/consignee commercial_invoice", "/packing_list/marks commercial_invoice",
		"/packing_list/notify commercial_invoice", "/packing_list/port_of_loading booking", "/packing_list/sources/actual_packing actual_packing"}
	if !slices.Equal(got, want) {
		t.Fatalf("differences\n%v\nwant\n%v", got, want)
	}
	if fold("  Synthetic\nBuyer  GmbH ") != fold("synthetic buyer gmbh") || marks(" ") != "N/M" {
		t.Fatal("text comparison")
	}
	_, sum := Seal(nil)
	if _, again := Seal([]Difference{}); sum != again || sum != Hash([]byte("[]")) {
		t.Fatal("an empty reconciliation hashes as []")
	}
}

// CN-AT17 "保存签署版本，草稿不能冒充已签署" in the domain (as TRD's order.document.record_signed): only an
// issued CI or PL version takes a signed copy per language, never its generated bytes; once.
func TestSignedCopies(t *testing.T) {
	f := fixtureFacts()
	s := NewSet("docs.1", f.Shipment, "", act())
	ci := prepared(t, s, KindCommercialInvoice, "CI-1", f)
	prepared(t, s, KindPackingList, "PL-1", f)
	file := File{ID: "file.stamped", SHA256: strings.Repeat("f", 64), MimeType: "application/pdf"}
	found := map[string]bool{"evidence": true}
	sign := func(kind string, version int64, language string, f File, evidence ...string) (bool, error) {
		_, replayed, err := s.Sign(SignRequest{Kind: kind, Version: version, Language: language, File: f, Evidence: evidence}, found, act())
		return replayed, err
	}
	if _, err := sign(KindCommercialInvoice, 1, LanguageEn, file); outcome(err) != "INVALID_STATE /args/version" {
		t.Fatal(outcome(err))
	}
	if _, _, err := s.Issue(2, shipment.StatusPrepared, Reconcile(s, f), nil, act()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		kind     string
		version  int64
		language string
		file     File
		evidence []string
		want     string
	}{
		{KindBillOfLading, 1, LanguageEn, file, nil, "INVALID_KNOWN_VALUE /args/document_kind"},
		{KindCommercialInvoice, 9, LanguageEn, file, nil, "REFERENCE_INVALID /args/version"},
		{KindCommercialInvoice, 1, "de", file, nil, "REFERENCE_INVALID /args/language"},
		{KindCommercialInvoice, 1, LanguageEn, File{ID: "file.generated", SHA256: ci.Rendition(LanguageZhCN).SHA256}, nil,
			"INVALID_KNOWN_VALUE /args/file_id"},
		{KindCommercialInvoice, 1, LanguageEn, file, []string{"hidden"}, "REFERENCE_INVALID /args/evidence_ids/0"},
		{KindCommercialInvoice, 1, LanguageEn, file, []string{"evidence"}, "OK"},
	} {
		if _, err := sign(c.kind, c.version, c.language, c.file, c.evidence...); outcome(err) != c.want {
			t.Fatalf("%+v: %s", c, outcome(err))
		}
	}
	if replayed, err := sign(KindCommercialInvoice, 1, LanguageEn, file); err != nil || !replayed {
		t.Fatalf("the same file again %v %v", replayed, err)
	}
	if _, err := sign(KindCommercialInvoice, 1, LanguageEn, File{ID: "file.other", SHA256: strings.Repeat("0", 64)}); outcome(err) !=
		"INVALID_STATE /args/language" {
		t.Fatal(outcome(err))
	}
	if replayed, err := sign(KindCommercialInvoice, 1, LanguageZhCN, File{ID: "file.zh", SHA256: strings.Repeat("1", 64)}); err != nil || replayed {
		t.Fatal(err)
	}
	if len(ci.Signatures) != 2 || ci.Signatures[0].DocumentSHA256 != ci.Rendition(LanguageEn).SHA256 {
		t.Fatalf("%+v", ci.Signatures)
	}
}

// A prepare against the set: its row version and its shipment.
func TestOpen(t *testing.T) {
	s := NewSet("docs.1", fixtureShipment(), "team.1", act())
	if s.OwnerActor != "docs.1" || s.OwnerTeam != "team.1" || s.Order.OrderID != "order.1" {
		t.Fatalf("%+v", s)
	}
	if outcome(s.Open(1, "shipment.1")) != "VERSION_CONFLICT /args/expected_version" || outcome(s.Open(0, "shipment.2")) !=
		"INVALID_KNOWN_VALUE /args/shipment_id" || s.Open(0, "shipment.1") != nil {
		t.Fatal("open")
	}
}
