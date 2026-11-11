package quote

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The synthetic quote of the S3-01 suite on TRD's references: BRC-001's RFQ line L1 (120 PCS of
// sku.1), 8 USD a piece, the 660.00 USD cost snapshot, a customer with a buyer address, Chinese
// customer notes with a line break (Python examples/quote.json).

var issuedAt = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

const (
	resultHash   = "2a809a7b878bb2bedba3eed7534ecd55d43952b41bad144babe3c2d289cc677f"
	evidenceHash = "3668d7dc21b908c3ef5b25ffab99b94193b51c785d0f66a41950fed5107d221b"
	linesHash    = "3ee92f1118c480ab32896c9bbb35186fa05687b9e44c43f0e1544527267c868f"
	sourceHash   = "0397250103796959c07c27f59f03da066db3e42b27d81da397a18d2f2b936a61"
	profileHash  = "9572be119f04ad3072f7a8f98a14a6946efeec50764cb45d43bce99deafe8090"
)

var (
	company  = Ref{Kind: KindCompanyProfile, ID: "company.live", Revision: 1}
	usd      = Ref{Kind: KindCurrency, ID: "USD", Revision: 1}
	pcs      = Ref{Kind: KindUnit, ID: "PCS", Revision: 1}
	sale     = Ref{Kind: KindTaxApplication, ID: "tax.sale", Revision: 1}
	zero     = Ref{Kind: KindTaxProfile, ID: "tax.zero", Revision: 1}
	policy   = Ref{Kind: KindPricingPolicy, ID: "policy.1", Revision: 1}
	trade    = Ref{Kind: KindTradeTerms, ID: "terms.test", Revision: 1}
	payment  = Ref{Kind: KindPaymentTerms, ID: "payment.test", Revision: 1}
	template = Ref{Kind: KindQuoteDocumentTemplate, ID: "quote.template", Revision: 1}
	sequence = Ref{Kind: KindNumberSequence, ID: "quote.sequence", Revision: 1}
	freight  = Ref{Kind: KindFeeCategory, ID: "freight.sale", Revision: 1}
	chargeTx = Ref{Kind: KindTaxApplication, ID: "tax.charge", Revision: 1}
	proof    = []Evidence{{SourceID: "source.1", Locator: "合成核实依据"}}
	hq       = AddressRef{PartyID: "customer.1", PartyRevision: 2, AddressID: "hq"}
)

func hashOf(s string) string { return Hash([]byte(s)) }

func draft() Data {
	notes := "客户可见说明：合成样例\nKeep this line break."
	return Data{Issuer: &company, Customer: &PartyRef{PartyID: "customer.1", Revision: 2},
		Contact: &ContactRef{ContactID: "contact.1", RowVersion: 2}, Profile: &ProfileRef{PartyID: "customer.1", Revision: 1},
		Addresses: []AddressRef{hq}, Rfq: &RfqRef{RfqID: "rfq.1", RowVersion: 2},
		Snapshot: &SnapshotRef{SnapshotID: "snapshot.1", ResultHash: resultHash},
		Lines: []Line{{LineID: "L1", RfqLineID: "L1", Sku: SkuRef{SkuID: "sku.1", SpecRevision: 1}, Quantity: "120.000000", Unit: pcs,
			UnitPrice: "8.0000", PriceBasisQuantity: "1.0", DiscountRatio: "0.00", TaxApplication: sale, Evidence: proof}},
		Currency: &usd, PricingPolicy: &policy, TradeTerms: &trade, PaymentTerms: &payment, DocumentTemplate: &template, Sequence: &sequence,
		ExpiresAt: "2026-10-28T08:00:00.000000Z", CustomerNotes: &notes, Evidence: proof}
}

func facts() Facts {
	f := Facts{Company: "company.live", Party: &Party{Confirmed: true, Revision: 2, Customer: true},
		Contact: &Contact{PartyID: "customer.1", RowVersion: 2, Confirmed: true},
		Profile: &Profile{ContentHash: profileHash, PartyRevision: 2, PaymentTerms: payment, TradeTerms: trade,
			Contacts: []ContactRef{{ContactID: "contact.1", RowVersion: 2}}},
		Addresses: map[AddressRef]Address{hq: {Role: "buyer", Address: "Synthetic buyer address\n上海"},
			{PartyID: "customer.1", PartyRevision: 2, AddressID: "ap"}: {Role: "billing", Address: "Synthetic billing address"}},
		Rfq: &Rfq{RowVersion: 2, Ready: true, LinesHash: linesHash, Lines: []RfqLine{{LineID: "L1", SkuID: "sku.1", SpecRevision: 1,
			Quantity: "120", UOM: "PCS"}}},
		Specs: map[SkuRef]Verdict{{SkuID: "sku.1", SpecRevision: 1}: nil},
		Configs: map[Ref]Config{
			company: {ContentHash: hashOf("company"), Company: &IssuerSnapshot{LegalName: "SYNTHETIC Exporter — no actual trade",
				RegistrationNo: "SYNTHETIC-NOT-REAL", Country: "CN", Address: "Synthetic Shanghai address"}},
			usd:      {ContentHash: hashOf("usd"), CurrencyCode: "USD"},
			pcs:      {ContentHash: hashOf("pcs"), UnitCode: "PCS"},
			sale:     {ContentHash: hashOf("sale"), Nested: []Ref{zero}, TaxApplication: &TaxApplication{Side: SideSales, SubjectKind: SubjectSKU, SubjectID: "sku.1"}},
			chargeTx: {ContentHash: hashOf("charge"), Nested: []Ref{zero}, TaxApplication: &TaxApplication{Side: SideSales, SubjectKind: SubjectFeeCategory, SubjectID: "freight.sale"}},
			zero:     {ContentHash: hashOf("zero")},
			policy:   {ContentHash: hashOf("policy"), Nested: []Ref{zero}, Policy: &Policy{CalculationCurrency: "USD", QuoteValidDays: 7}},
			trade:    {ContentHash: hashOf("trade")},
			payment:  {ContentHash: hashOf("payment")},
			template: {ContentHash: hashOf("template"), Renderer: QuoteRendererV010},
			sequence: {ContentHash: hashOf("sequence"), SequenceType: DocumentTypeQuote},
			freight:  {ContentHash: hashOf("freight"), FeeSide: SideSalesCharge},
		},
		Snapshot: &Snapshot{RfqID: "rfq.1", RfqVersion: 2, RfqLinesHash: linesHash, Currency: "USD", EvidenceHash: evidenceHash, LineIDs: []string{"L1"}},
		Sources:  map[string]string{"source.1": sourceHash},
	}
	return f
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

// goVectors are TRD's own golden commercial documents: the Go document of the fixture and the
// same with a server calculation (the S3-02 shape), each with the canonical bytes and the hash that
// Python V0.50 protocol.canonical(protocol.normalize(document)) gives for the document's JSON
// (protocol's schema check aside: the document has TRD's references). Regenerate with
// TRD_WRITE_VECTORS=1 and check the result with the Python oracle before committing it.
const goVectors = "testdata/trd-commercial-vectors.json"

type goVector struct {
	Name       string          `json:"name"`
	Document   json.RawMessage `json:"document"`
	Normalized string          `json:"normalized_canonical"`
	SHA256     string          `json:"sha256"`
}

func calculation() *Calculation {
	return &Calculation{Lines: []AmountLine{{ID: "L1", NetMinor: "96000", TaxMinor: "0", PayableMinor: "96000"}}, Charges: []AmountLine{},
		NetMinor: "96000", TaxMinor: "0", PayableMinor: "96000", ComparisonCurrency: usd, ComparisonRevenueMinor: "96000", CostMinor: "66000",
		ProfitMinor: "30000", MarginNumerator: "30000", MarginDenominator: "96000", MarginRule: MarginPass, CostBasis: CostBasisNominalDirect,
		ExcludedCostCategories: []string{"unrealized_fx_hedge", "imputed_financing_cost", "expected_credit_loss"},
		Payments:               []Payment{{StageID: "P1", Percent: "100.00", Trigger: "before_shipment", Days: 0, DayBasis: "calendar", AmountMinor: "96000"}}}
}

// TRD-INV-002 and WBS S3-01 (哈希跨实现互操作向量，含中文与换行): the freeze of the fixture seals a
// normalized canonical document whose bytes and SHA-256 are pinned, with Chinese text and line
// breaks kept, decimals trimmed, and the document JSON reproducing the same bytes through the
// normalization alone.
func TestCommercialDocumentGoldenVectors(t *testing.T) {
	cases := []struct {
		name string
		calc *Calculation
	}{{"fixture without server calculation", nil}, {"fixture with server calculation", calculation()}}
	var got []goVector
	for _, c := range cases {
		s, err := SealDraft("quote.1", 1, draft(), facts(), c.calc, issuedAt)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if s.BusinessHash != Hash(s.Commercial) || !strings.Contains(string(s.Commercial), `\nKeep this line break.`) ||
			!strings.Contains(string(s.Commercial), "合成核实依据") || !strings.Contains(string(s.Commercial), `"quantity":"120"`) {
			t.Fatalf("%s: %s", c.name, s.Commercial)
		}
		doc, err := ParseCommercial(s.Commercial, s.BusinessHash)
		if err != nil || doc.QuoteID != "quote.1" || doc.BusinessRevision != 1 || doc.IssuedAt != "2026-10-22T09:00:00.000000Z" ||
			doc.NormalizationVersion != NormalizationVersion || (doc.Calculation == nil) != (c.calc == nil) {
			t.Fatalf("%s: parsed %+v, %v", c.name, doc, err)
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, goVector{Name: c.name, Document: raw, Normalized: string(s.Commercial), SHA256: s.BusinessHash})
	}
	if os.Getenv("TRD_WRITE_VECTORS") == "1" {
		out, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(goVectors, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(goVectors)
	if err != nil {
		t.Fatal(err)
	}
	var want []goVector
	if err = json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(got) {
		t.Fatalf("%d vectors, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Normalized != want[i].Normalized || got[i].SHA256 != want[i].SHA256 {
			t.Fatalf("%s: commercial document changed:\n%s\nwant\n%s", want[i].Name, got[i].Normalized, want[i].Normalized)
		}
		tree, err := Tree(want[i].Document)
		if err != nil {
			t.Fatal(err)
		}
		canonical, hash, err := Digest(tree)
		if err != nil || string(canonical) != want[i].Normalized || hash != want[i].SHA256 {
			t.Fatalf("%s: the document does not normalize to its bytes: %v", want[i].Name, err)
		}
	}
	if want[0].SHA256 == want[1].SHA256 {
		t.Fatal("the calculation is part of the business hash")
	}
}

// TRD-INV-002: what the customer is offered changes the hash; another spelling of the same decimals
// or another order of the lines' evidence does not.
func TestBusinessHashCoversCommercialContentOnly(t *testing.T) {
	base, err := SealDraft("quote.1", 1, draft(), facts(), nil, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	same := draft()
	same.Lines[0].Quantity, same.Lines[0].UnitPrice, same.Lines[0].DiscountRatio = "120", "8", "0"
	same.Evidence = append([]Evidence{{SourceID: "source.1", Locator: "第二处"}}, proof...)
	twice := draft()
	twice.Evidence = append(slicesClone(proof), Evidence{SourceID: "source.1", Locator: "第二处"})
	a, err := SealDraft("quote.1", 1, same, facts(), nil, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SealDraft("quote.1", 1, twice, facts(), nil, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if a.BusinessHash != b.BusinessHash {
		t.Fatal("evidence order must not change the hash")
	}
	spelled := draft()
	spelled.Lines[0].Quantity, spelled.Lines[0].UnitPrice = "120.0", "8.00000000"
	c, err := SealDraft("quote.1", 1, spelled, facts(), nil, issuedAt)
	if err != nil || c.BusinessHash != base.BusinessHash {
		t.Fatalf("equivalent decimals: %v", err)
	}
	for name, edit := range map[string]func(d *Data){
		"customer notes": func(d *Data) { n := *d.CustomerNotes + "\n"; d.CustomerNotes = &n },
		"unit price":     func(d *Data) { d.Lines[0].UnitPrice = "8.01" },
		"expiry":         func(d *Data) { d.ExpiresAt = "2026-10-28T08:00:00.000001Z" },
	} {
		d := draft()
		edit(&d)
		s, err := SealDraft("quote.1", 1, d, facts(), nil, issuedAt)
		if err != nil || s.BusinessHash == base.BusinessHash {
			t.Errorf("%s must change the hash: %v", name, err)
		}
	}
	other, err := SealDraft("quote.1", 2, draft(), facts(), nil, issuedAt)
	if err != nil || other.BusinessHash == base.BusinessHash {
		t.Fatalf("the business revision is part of the hash: %v", err)
	}
}

func slicesClone(es []Evidence) []Evidence { return append([]Evidence(nil), es...) }

// The freeze's checks in their order, each refused with its catalog code at its path (Python
// t03_storage freeze, _seal_quote_timing, _server_calculation, t03_trade_parties.bind).
func TestSealDraftRefusesWhatCannotBeFrozen(t *testing.T) {
	stale := func(path string) error { return fail(CodeReferenceStale, path, "") }
	cases := []struct {
		name string
		d    func(d *Data)
		f    func(f *Facts)
		want string
	}{
		{"no customer", func(d *Data) { d.Customer = nil }, nil, "CUSTOMER_REFERENCE_MISSING at /args/data/customer_ref"},
		{"no contact", func(d *Data) { d.Contact = nil }, nil, "CONTACT_REFERENCE_MISSING at /args/data/contact_ref"},
		{"no snapshot", func(d *Data) { d.Snapshot = nil }, nil, "COST_SNAPSHOT_MISSING at /args/data/cost_snapshot_ref"},
		{"no expiry", func(d *Data) { d.ExpiresAt = "" }, nil, "DRAFT_NOT_READY at /args/data/expires_at"},
		{"no evidence", func(d *Data) { d.Evidence = nil }, nil, "DRAFT_NOT_READY at /args/data/evidence"},
		{"unreadable evidence", nil, func(f *Facts) { delete(f.Sources, "source.1") }, "REFERENCE_INVALID at /args/data/evidence/0/source_id"},
		{"another company", func(d *Data) { r := Ref{Kind: KindCompanyProfile, ID: "company.other", Revision: 1}; d.Issuer = &r }, nil,
			"REFERENCE_INVALID at /args/data/issuer_ref/id"},
		{"customer revised", nil, func(f *Facts) { f.Party.Revision = 3 }, "REFERENCE_STALE at /args/data/customer_ref"},
		{"not a customer", nil, func(f *Facts) { f.Party.Customer = false }, "REFERENCE_INVALID at /args/data/customer_ref"},
		{"contact of another party", nil, func(f *Facts) { f.Contact.PartyID = "customer.2" }, "REFERENCE_INVALID at /args/data/contact_ref"},
		{"contact changed", nil, func(f *Facts) { f.Contact.RowVersion = 3 }, "REFERENCE_STALE at /args/data/contact_ref"},
		{"profile superseded", nil, func(f *Facts) { f.Profile.Check = stale }, "REFERENCE_STALE at /args/data/trading_profile_ref"},
		{"contact not pinned", nil, func(f *Facts) { f.Profile.Contacts = nil }, "REFERENCE_INVALID at /args/data/contact_ref"},
		{"unknown address", func(d *Data) {
			d.Addresses = []AddressRef{{PartyID: "customer.1", PartyRevision: 2, AddressID: "nowhere"}}
		}, nil,
			"REFERENCE_INVALID at /args/data/customer_address_refs/0"},
		{"billing only is enough, consignee only is not", func(d *Data) {
			d.Addresses = []AddressRef{{PartyID: "customer.1", PartyRevision: 2, AddressID: "dock"}}
		}, func(f *Facts) {
			f.Addresses[AddressRef{PartyID: "customer.1", PartyRevision: 2, AddressID: "dock"}] = Address{Role: "consignee", Address: "dock"}
		}, "REFERENCE_INVALID at /args/data/customer_address_refs"},
		{"rfq cancelled", nil, func(f *Facts) { f.Rfq.Ready = false }, "REFERENCE_STALE at /args/data/rfq_ref"},
		{"spec revised", nil, func(f *Facts) { f.Specs[SkuRef{SkuID: "sku.1", SpecRevision: 1}] = stale },
			"REFERENCE_STALE at /args/data/lines/0/sku_ref"},
		{"config retired", nil, func(f *Facts) {
			c := f.Configs[template]
			c.Check = func(p string) error { return kernel.RejectAs("CONFIG_NOT_ACTIVE", kernel.CategoryInvalid, p, "") }
			f.Configs[template] = c
		}, "CONFIG_NOT_ACTIVE at /args/data/document_template_ref"},
		{"nested tax profile unknown", nil, func(f *Facts) { delete(f.Configs, zero) }, "REFERENCE_INVALID at /args/data/lines/0/tax_application_ref"},
		{"snapshot not adoptable", nil, func(f *Facts) {
			f.Snapshot.Check = func(p string) error {
				return kernel.RejectAs("COST_SNAPSHOT_INELIGIBLE", kernel.CategoryInvalid, p, "")
			}
		}, "COST_SNAPSHOT_INELIGIBLE at /args/data/cost_snapshot_ref"},
		{"partial line set", nil, func(f *Facts) {
			f.Rfq.Lines = append(f.Rfq.Lines, RfqLine{LineID: "L2", SkuID: "sku.2", Quantity: "1", UOM: "PCS"})
		},
			"COMPARISON_BASIS_MISMATCH at /args/data/lines"},
		{"another sku", func(d *Data) { d.Lines[0].Sku.SpecRevision = 2 }, func(f *Facts) { f.Specs[SkuRef{SkuID: "sku.1", SpecRevision: 2}] = nil },
			"COMPARISON_BASIS_MISMATCH at /args/data/lines/0/sku_ref"},
		{"another quantity", func(d *Data) { d.Lines[0].Quantity = "100" }, nil, "BASE_QUANTITY_MISMATCH at /args/data/lines/0/quantity"},
		{"purchase tax application", nil, func(f *Facts) { f.Configs[sale].TaxApplication.Side = "purchase" },
			"REFERENCE_INVALID at /args/data/lines/0/tax_application_ref"},
		{"tax application of another party", nil, func(f *Facts) { f.Configs[sale].TaxApplication.PartyID = "customer.2" },
			"REFERENCE_INVALID at /args/data/lines/0/tax_application_ref"},
		{"redundant fx", func(d *Data) { r := Ref{Kind: KindFXRate, ID: "usd.usd", Revision: 1}; d.SalesFX = &r }, func(f *Facts) {
			f.Configs[Ref{Kind: KindFXRate, ID: "usd.usd", Revision: 1}] = Config{ContentHash: hashOf("fx")}
		}, "REFERENCE_INVALID at /args/data/sales_fx_ref"},
		{"terms not the profile's", nil, func(f *Facts) { f.Profile.TradeTerms = Ref{Kind: KindTradeTerms, ID: "FOB", Revision: 1} },
			"COMPARISON_BASIS_MISMATCH at /args/data/trade_terms_ref"},
		{"renderer", nil, func(f *Facts) { c := f.Configs[template]; c.Renderer = "html/1"; f.Configs[template] = c },
			"DEPENDENCY_NOT_SUPPORTED at /args/data/document_template_ref"},
		{"order sequence", nil, func(f *Facts) { c := f.Configs[sequence]; c.SequenceType = "order"; f.Configs[sequence] = c },
			"REFERENCE_INVALID at /args/data/sequence_ref"},
		{"snapshot of another rfq version", nil, func(f *Facts) { f.Snapshot.RfqVersion = 1 },
			"COMPARISON_BASIS_MISMATCH at /args/data/cost_snapshot_ref"},
		{"snapshot in another currency", nil, func(f *Facts) { f.Snapshot.Currency = "EUR" },
			"COMPARISON_BASIS_MISMATCH at /args/data/cost_snapshot_ref"},
		{"expired at issue", func(d *Data) { d.ExpiresAt = kernel.FormatTimestamp(issuedAt) }, nil, "QUOTE_EXPIRED at /args/data/expires_at"},
		{"beyond the policy's validity", func(d *Data) { d.ExpiresAt = kernel.FormatTimestamp(issuedAt.Add(7*24*time.Hour + time.Microsecond)) }, nil,
			"QUOTE_VALIDITY_INVALID at /args/data/expires_at"},
		{"duplicate obligation", func(d *Data) {
			c := Charge{ChargeID: "C1", FeeCategory: freight, ObligationKey: "freight", Amount: "60", Inclusion: InclusionSeparate,
				TaxApplication: chargeTx, Evidence: proof}
			c2 := c
			c2.ChargeID = "C2"
			d.Charges = []Charge{c, c2}
		}, nil, "CHARGE_SCOPE_UNRESOLVED at /args/data/charges/1/obligation_key"},
	}
	for _, c := range cases {
		d, f := draft(), facts()
		if c.d != nil {
			c.d(&d)
		}
		if c.f != nil {
			c.f(&f)
		}
		_, err := SealDraft("quote.1", 1, d, f, nil, issuedAt)
		if got := code(err); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	// The policy's last valid instant is admitted (issued < expires ≤ issued + 7 days).
	d := draft()
	d.ExpiresAt = kernel.FormatTimestamp(issuedAt.Add(7 * 24 * time.Hour))
	if _, err := SealDraft("quote.1", 1, d, facts(), nil, issuedAt); err != nil {
		t.Fatalf("seven days: %v", err)
	}
}

// A quote in another unit than the RFQ's converts its quantity exactly through the SKU
// specification's conversion (TRD-INV-006, BASE_QUANTITY_MISMATCH otherwise).
func TestLineQuantityConvertsToTheRfqUnit(t *testing.T) {
	dozen := Ref{Kind: KindUnit, ID: "DOZ", Revision: 1}
	conv := Ref{Kind: KindUnitConversion, ID: "sku.1.doz", Revision: 1}
	f := facts()
	f.Configs[dozen] = Config{ContentHash: hashOf("doz"), UnitCode: "DOZ"}
	f.Configs[conv] = Config{ContentHash: hashOf("conv"), Conversion: &Conversion{SkuID: "sku.1", SpecRevision: 1, From: "DOZ", To: "PCS", Factor: "12"}}
	for q, want := range map[string]string{"10": "OK", "10.5": "BASE_QUANTITY_MISMATCH at /args/data/lines/0/quantity"} {
		d := draft()
		d.Lines[0].Quantity, d.Lines[0].Unit, d.Lines[0].Conversion = q, dozen, &conv
		_, err := SealDraft("quote.1", 1, d, f, nil, issuedAt)
		if got := code(err); got != want {
			t.Errorf("%s DOZ: %s, want %s", q, got, want)
		}
	}
	d := draft()
	d.Lines[0].Unit = dozen
	if got := code(func() error { _, err := SealDraft("quote.1", 1, d, f, nil, issuedAt); return err }()); got !=
		"REFERENCE_INVALID at /args/data/lines/0/conversion_ref" {
		t.Errorf("a missing conversion: %s", got)
	}
}

// A server calculation that does not fit the revision is not sealed.
func TestCalculationMustFitTheRevision(t *testing.T) {
	for name, edit := range map[string]func(c *Calculation){
		"another line":       func(c *Calculation) { c.Lines[0].ID = "L2" },
		"tax not added":      func(c *Calculation) { c.TaxMinor = "1" },
		"payments short":     func(c *Calculation) { c.Payments[0].AmountMinor = "95999" },
		"unknown margin":     func(c *Calculation) { c.MarginRule = "maybe" },
		"float-like amount":  func(c *Calculation) { c.NetMinor = "960.00" },
		"no excluded list":   func(c *Calculation) { c.ExcludedCostCategories = nil },
		"another cost basis": func(c *Calculation) { c.CostBasis = "full_cost" },
	} {
		c := calculation()
		edit(c)
		if _, err := SealDraft("quote.1", 1, draft(), facts(), c, issuedAt); code(err) == "OK" || !strings.HasPrefix(code(err), "INVALID_KNOWN_VALUE at /calculation") {
			t.Errorf("%s: %s", name, code(err))
		}
	}
}

func human(id string) kernel.ActorRef { return kernel.ActorRef{Kind: kernel.ActorHuman, ID: id} }

// SM-TRD-REVISION, S3-01 part (Python test_t03_quote_review test_revise_and_discard_preserve_sealed
// _history, test_hash_and_revision_are_immutable_and_cas_is_required): save replaces the working
// revision, freeze seals it, revise copies a frozen revision into a new number, discard keeps the
// revision and never reuses its number; metadata moves the root version only.
func TestRevisionLifecycle(t *testing.T) {
	q := New("quote.1", "author", "trade")
	if err := CheckSave(nil, false, 1); code(err) != "NOT_FOUND at /args/quote_id" {
		t.Fatalf("a missing root at version 1: %s", code(err))
	}
	now := issuedAt
	w := q.Save(draft(), human("author"), 1, now)
	q.Save(draft(), kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent"}, 1, now)
	if q.RootVersion != 2 || w.Number != 1 || len(q.Revisions) != 1 || len(w.Contributors) != 2 {
		t.Fatalf("after two saves %+v", q)
	}
	if err := CheckSave(q, true, 1); code(err) != "VERSION_CONFLICT at /args/expected_version" {
		t.Fatalf("stale version: %s", code(err))
	}
	if _, err := q.Revise(1, human("author"), 1, now); code(err) != "INVALID_STATE at /args/quote_id" {
		t.Fatalf("revise with a working revision: %s", code(err))
	}
	s, err := SealDraft(q.ID, w.Number, w.Data, facts(), nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.CheckFreeze(2); code(err) != "INVALID_STATE at /args/working_revision" {
		t.Fatalf("another working revision: %s", code(err))
	}
	if _, err = q.Freeze(1, s, human("checker"), 1, now); err != nil {
		t.Fatal(err)
	}
	if w.Status != Frozen || q.RootVersion != 3 || w.Seal.RootVersion != 3 || q.FrozenAt(3) != w || q.Working() != nil {
		t.Fatalf("frozen %+v", w)
	}
	if err = CheckSave(q, true, 3); code(err) != "INVALID_STATE at /args/quote_id" {
		t.Fatalf("save after freeze: %s", code(err))
	}
	hash := w.Seal.BusinessHash
	q.UpdateMetadata([]string{"VIP", "急"}, "内部备注\n第二行")
	if q.RootVersion != 4 || w.Seal.BusinessHash != hash || !q.MetadataChanged {
		t.Fatal("metadata moves the root version only")
	}
	if _, err = q.Revise(2, human("author"), 1, now); code(err) != "REFERENCE_INVALID at /args/base_revision" {
		t.Fatalf("revise an unknown revision: %s", code(err))
	}
	r2, err := q.Revise(1, human("reviser"), 1, now)
	if err != nil || r2.Number != 2 || r2.Base != 1 || r2.Hash != w.Data.Hash() || len(r2.Contributors) != 3 {
		t.Fatalf("revise %+v, %v", r2, err)
	}
	if _, err = q.Discard(1, human("author"), 1, now, "reason", proof); code(err) != "INVALID_STATE at /args/working_revision" {
		t.Fatalf("discard a frozen revision: %s", code(err))
	}
	if _, err = q.Discard(2, human("author"), 1, now, "客户改需求", proof); err != nil || r2.Status != Discarded {
		t.Fatalf("discard: %v", err)
	}
	r3, err := q.Revise(1, human("author"), 1, now)
	if err != nil || r3.Number != 3 {
		t.Fatalf("a discarded number is not reused: %+v, %v", r3, err)
	}
}

// TRD-INV-015 (Python test_t03_a24_write_control and t03_storage.guard): who may write while the
// root is unbound, bound or stopping, and with which claim.
func TestControlFence(t *testing.T) {
	q := New("quote.1", "author", "")
	now := issuedAt
	task := &Claim{TaskID: "task.1", Epoch: 1}
	if err := q.Control.Admit(nil, 7); err != nil {
		t.Fatalf("an unbound root takes manual writes: %v", err)
	}
	if got := code(q.Control.Admit(task, 7)); got != "CONTROL_EPOCH_STALE at /" {
		t.Fatalf("a claim on an unbound root: %s", got)
	}
	if got := code(q.Bind("task.1", 1, human("author"), 7, now)); got != "VERSION_CONFLICT at /args/expected_control_epoch" {
		t.Fatalf("bind at another epoch: %s", got)
	}
	if err := q.Bind("task.1", 0, human("author"), 7, now); err != nil || q.Control.Epoch != 1 || q.Control.State != Bound {
		t.Fatalf("bind: %v %+v", err, q.Control)
	}
	if got := code(q.Bind("task.2", 1, human("author"), 7, now)); got != "OBJECT_CONTROL_CONFLICT at /args/quote_id" {
		t.Fatalf("a second binding: %s", got)
	}
	for name, c := range map[string]struct {
		claim     *Claim
		authority int64
		want      string
	}{
		"no claim":          {nil, 7, "OBJECT_CONTROL_CONFLICT at /"},
		"another task":      {&Claim{TaskID: "task.2", Epoch: 1}, 7, "OBJECT_CONTROL_CONFLICT at /"},
		"an earlier epoch":  {&Claim{TaskID: "task.1", Epoch: 0}, 7, "CONTROL_EPOCH_STALE at /"},
		"access changed":    {task, 8, "OBJECT_CONTROL_CONFLICT at /"},
		"the bound task":    {task, 7, "OK"},
		"a future epoch":    {&Claim{TaskID: "task.1", Epoch: 2}, 7, "CONTROL_EPOCH_STALE at /"},
		"guessed and wrong": {&Claim{TaskID: "other", Epoch: 2}, 7, "CONTROL_EPOCH_STALE at /"},
	} {
		if got := code(q.Control.Admit(c.claim, c.authority)); got != c.want {
			t.Errorf("bound, %s: %s, want %s", name, got, c.want)
		}
	}
	if got := code(q.Release(0, human("author"), now)); got != "VERSION_CONFLICT at /args/expected_control_epoch" {
		t.Fatalf("release at an old epoch: %s", got)
	}
	if err := q.Stop(1, human("lead"), now); err != nil || q.Control.State != Stopping || q.Control.Epoch != 2 || q.Control.TaskID != "task.1" {
		t.Fatalf("stop: %v %+v", err, q.Control)
	}
	if got := code(q.Control.Admit(task, 7)); got != "CONTROL_EPOCH_STALE at /" {
		t.Fatalf("the stopped task: %s", got)
	}
	if got := code(q.Control.Admit(nil, 7)); got != "OBJECT_CONTROL_CONFLICT at /" {
		t.Fatalf("a manual write while stopping: %s", got)
	}
	if got := code(q.Control.Admit(&Claim{TaskID: "task.1", Epoch: 2}, 7)); got != "OBJECT_CONTROL_CONFLICT at /" {
		t.Fatalf("a claim of the stop's epoch: %s", got)
	}
	if got := code(q.Stop(2, human("lead"), now)); got != "INVALID_STATE at /args/quote_id" {
		t.Fatalf("stop twice: %s", got)
	}
	if err := q.Release(2, human("lead"), now); err != nil || q.Control.State != Unbound || q.Control.Epoch != 3 || q.Control.TaskID != "" {
		t.Fatalf("release: %v %+v", err, q.Control)
	}
	if err := q.Control.Admit(nil, 9); err != nil {
		t.Fatalf("released: %v", err)
	}
	if got := code(q.Release(3, human("lead"), now)); got != "INVALID_STATE at /args/quote_id" {
		t.Fatalf("release an unbound root: %s", got)
	}
	if err := q.Bind("task.2", 3, kernel.ActorRef{Kind: kernel.ActorSystem, ID: "tsk"}, 9, now); err != nil || q.Control.Epoch != 4 {
		t.Fatalf("rebind: %v", err)
	}
	if got := code(q.Control.Admit(task, 9)); got != "CONTROL_EPOCH_STALE at /" {
		t.Fatalf("the first task after a rebinding: %s", got)
	}
	if err := q.Control.Admit(&Claim{TaskID: "task.2", Epoch: 4}, 9); err != nil {
		t.Fatalf("the new task: %v", err)
	}
	if q.RootVersion != 0 {
		t.Fatal("control never moves the root version")
	}
}

func TestDraftChecks(t *testing.T) {
	readable := map[string]bool{"source.1": true}
	for name, c := range map[string]struct {
		edit func(d *Data)
		want string
	}{
		"duplicate line":     {func(d *Data) { d.Lines = append(d.Lines, d.Lines[0]) }, "DUPLICATE_LINE_ID at /args/data/lines/1/line_id"},
		"duplicate rfq line": {func(d *Data) { l := d.Lines[0]; l.LineID = "L2"; d.Lines = append(d.Lines, l) }, "DUPLICATE_LINE_ID at /args/data/lines/1/rfq_line_id"},
		"wrong kind":         {func(d *Data) { d.Currency = &pcs }, "REFERENCE_INVALID at /args/data/currency_ref/kind"},
		"included in nothing": {func(d *Data) {
			d.Charges = []Charge{{ChargeID: "C1", FeeCategory: freight, ObligationKey: "o", Amount: "1", Inclusion: InclusionIncludedInLine, IncludedLineID: "L9", TaxApplication: chargeTx, Evidence: proof}}
		}, "REFERENCE_INVALID at /args/data/charges/0/included_line_id"},
		"unreadable":        {func(d *Data) { d.Lines[0].Evidence = []Evidence{{SourceID: "secret", Locator: "x"}} }, "REFERENCE_INVALID at /args/data/lines/0/evidence/0/source_id"},
		"duplicate address": {func(d *Data) { d.Addresses = append(d.Addresses, hq) }, "INVALID_KNOWN_VALUE at /args/data/customer_address_refs/1/address_id"},
		"empty draft":       {func(d *Data) { *d = Data{} }, "OK"},
	} {
		d := draft()
		c.edit(&d)
		if got := code(CheckDraft(d, readable)); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	if got := fmt.Sprint(draft().Sources()); got != "[source.1]" {
		t.Fatalf("sources %s", got)
	}
}
