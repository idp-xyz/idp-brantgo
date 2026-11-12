package document

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
)

// Golden files. The python-v050-* example files are Python V0.50 t03_candidate/examples copied byte
// for byte (commercial.json, registry.json, document.json, candidate-document.txt). The two vector
// files were written by Python V0.50's own code: t03_candidate/document.render for the quote
// documents (render-vectors) and the loop of brant_core/foundation.py document_render over
// string.Formatter().parse for plain-text templates (plaintext-vectors), driven by a scratch oracle
// run with python3 -I (not part of the repository).
var golden = map[string]string{
	"python-v050-candidate-document.txt": "f25b3b89a9c897d051cc50eecf6240eb7e89b9207a16ec4e3baf371b3fd902ef",
	"python-v050-commercial.json":        "1f252abdbbd7a245319f52e4cc1438b0d07de1943e485548c86db57241c2b5ac",
	"python-v050-document.json":          "ad6e0a49dd67b3b5087bb5ade89faed28549769ba2ffe48358f09781bd688603",
	"python-v050-registry.json":          "83de806a87854bf1fe90028134ea886aa181490ef0b6a5076f307d06443d72d5",
	"python-v050-render-vectors.json":    "74082c28c223c47377221f444dcfd507a4e4110a6d28b4b095083e891c13a0c0",
	"python-v050-plaintext-vectors.json": "100ffd7d879d4bb35a1aef17d84d8ecdd7f19ec9a00e7f47a81bb62a93e6bdb3",
}

func load(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if got := quote.Hash(raw); got != golden[name] {
		t.Fatalf("testdata/%s is not the pinned file: SHA-256 %s", name, got)
	}
	return raw
}

func decode(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

// Python's shapes, as t03_candidate/document.py reads them.

type pyRef struct {
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Revision   *int64 `json:"revision"`
	RowVersion *int64 `json:"row_version"`
}

func (r pyRef) version() int64 {
	if r.Revision != nil {
		return *r.Revision
	}
	return *r.RowVersion
}

func (r pyRef) key() string { return fmt.Sprintf("%s|%s|%d", r.Kind, r.ID, r.version()) }

type pyAmount struct {
	ID      string `json:"id"`
	Net     string `json:"net_minor"`
	Tax     string `json:"tax_minor"`
	Payable string `json:"payable_minor"`
}

type pyCommercial struct {
	QuoteID          string `json:"quote_id"`
	BusinessRevision int64  `json:"business_revision"`
	IssuedAt         string `json:"issued_at"`
	Data             struct {
		Lines []struct {
			LineID    string `json:"line_id"`
			Sku       pyRef  `json:"sku_profile_ref"`
			Quantity  string `json:"quantity"`
			Unit      pyRef  `json:"unit_ref"`
			UnitPrice string `json:"unit_price"`
			Basis     string `json:"price_basis_quantity"`
			Discount  string `json:"discount_ratio"`
		} `json:"lines"`
		Charges []struct {
			ChargeID      string `json:"charge_id"`
			ObligationKey string `json:"obligation_key"`
			Inclusion     string `json:"inclusion"`
		} `json:"charges"`
		Currency   pyRef `json:"currency_ref"`
		TradeTerms pyRef `json:"trade_terms_ref"`
		NamedPlace pyRef `json:"named_place_ref"`
		TradeScope pyRef `json:"trade_scope_ref"`
		Delivery   struct {
			Date      string `json:"date"`
			Milestone string `json:"milestone"`
			TimeZone  string `json:"time_zone"`
		} `json:"delivery"`
		ExpiresAt     string `json:"expires_at"`
		CustomerNotes string `json:"customer_notes"`
		Template      pyRef  `json:"document_template_ref"`
	} `json:"data"`
	Calculation struct {
		Lines    []pyAmount `json:"lines"`
		Charges  []pyAmount `json:"charges"`
		Net      string     `json:"net_minor"`
		Tax      string     `json:"tax_minor"`
		Payable  string     `json:"payable_minor"`
		Payments []struct {
			StageID  string `json:"stage_id"`
			Percent  string `json:"percent"`
			Amount   string `json:"amount_minor"`
			Trigger  string `json:"trigger"`
			Days     int    `json:"days"`
			DayBasis string `json:"day_basis"`
			Calendar pyRef  `json:"calendar"`
		} `json:"payments"`
	} `json:"calculation"`
	PartySnapshots struct {
		Issuer      struct{ Name, Address string } `json:"issuer"`
		Customer    struct{ Name, Address string } `json:"customer"`
		ContactName string                         `json:"contact_name"`
	} `json:"party_snapshots"`
}

type pyRecord struct {
	Ref  pyRef           `json:"ref"`
	Data json.RawMessage `json:"data"`
}

// pySheet reads a Python commercial document and the registry records it names exactly as
// document.render does, into the template and sheet this package prints.
func pySheet(t *testing.T, c pyCommercial, records []pyRecord, number, businessHash string) (Template, sheet) {
	t.Helper()
	data := map[string]json.RawMessage{}
	for _, r := range records {
		data[r.Ref.key()] = r.Data
	}
	get := func(r pyRef, v any) {
		t.Helper()
		raw, ok := data[r.key()]
		if !ok {
			t.Fatalf("no record %s", r.key())
		}
		decode(t, raw, v)
	}
	var tpl struct {
		Language        string   `json:"language"`
		RendererVersion string   `json:"renderer_version"`
		Title           string   `json:"title"`
		Sections        []string `json:"sections"`
		FooterNote      string   `json:"footer_note"`
	}
	get(c.Data.Template, &tpl)
	template := Template{Language: tpl.Language, RendererVersion: tpl.RendererVersion, Title: tpl.Title, Sections: tpl.Sections,
		FooterNote: tpl.FooterNote}
	if err := template.Supported(); err != nil {
		t.Fatal(err)
	}
	calc := c.Calculation
	s := sheet{issuerName: c.PartySnapshots.Issuer.Name, issuerAddress: c.PartySnapshots.Issuer.Address,
		customerName: c.PartySnapshots.Customer.Name, customerAddress: c.PartySnapshots.Customer.Address,
		contact: c.PartySnapshots.ContactName, number: number, quoteID: c.QuoteID, revision: c.BusinessRevision,
		businessHash: businessHash, net: calc.Net, tax: calc.Tax, payable: calc.Payable, issuedAt: c.IssuedAt,
		expiresAt: c.Data.ExpiresAt, customerNotes: c.Data.CustomerNotes}
	money := map[string]pyAmount{}
	for _, a := range calc.Lines {
		money[a.ID] = a
	}
	for _, l := range c.Data.Lines {
		var sku struct {
			ItemName string `json:"item_name"`
			Spec     string `json:"specification_version"`
		}
		var unit struct {
			Code string `json:"code"`
		}
		get(l.Sku, &sku)
		get(l.Unit, &unit)
		a := money[l.LineID]
		s.lines = append(s.lines, lineRow{id: l.LineID, item: sku.ItemName, specification: sku.Spec, quantity: l.Quantity, unit: unit.Code,
			unitPrice: l.UnitPrice, basis: l.Basis, discount: l.Discount, net: a.Net, tax: a.Tax, payable: a.Payable})
	}
	for _, ch := range c.Data.Charges {
		for _, a := range calc.Charges {
			if a.ID == ch.ChargeID {
				s.charges = append(s.charges, chargeRow{id: ch.ChargeID, obligation: ch.ObligationKey, inclusion: ch.Inclusion,
					net: a.Net, tax: a.Tax, payable: a.Payable})
				break
			}
		}
	}
	var currency struct {
		Code       string `json:"code"`
		MinorUnits int    `json:"minor_units"`
	}
	get(c.Data.Currency, &currency)
	s.currency, s.minorUnits = currency.Code, currency.MinorUnits
	var terms struct {
		Incoterm string `json:"incoterm"`
		Edition  string `json:"edition"`
		Note     string `json:"responsibility_note"`
	}
	var place struct {
		Name string `json:"name"`
	}
	get(c.Data.TradeTerms, &terms)
	get(c.Data.NamedPlace, &place)
	s.incoterm, s.edition, s.responsibility, s.namedPlace = terms.Incoterm, terms.Edition, terms.Note, place.Name
	s.scope = c.Data.TradeScope.ID + " r" + fmt.Sprint(c.Data.TradeScope.version())
	s.delivery = []string{c.Data.Delivery.Date, c.Data.Delivery.Milestone, c.Data.Delivery.TimeZone}
	for _, p := range calc.Payments {
		cal := quote.Ref{Kind: p.Calendar.Kind, ID: p.Calendar.ID, Revision: p.Calendar.version()}
		s.payments = append(s.payments, paymentRow{stage: p.StageID, percent: p.Percent, amount: p.Amount, trigger: p.Trigger,
			dayBasis: p.DayBasis, days: p.Days, calendar: &cal})
	}
	return template, s
}

// Python t03_candidate/verify.py "document-reference": render(commercial.json, registry.json,
// "SYN-Q-2026-00001") is document.json, the exact immutable sample bytes of
// candidate-document.txt, and content_sha256 is the SHA-256 of those bytes. Read through
// document.py's lookups, the same values print the same bytes here.
func TestCandidateDocumentIsReproducedByteForByte(t *testing.T) {
	want := load(t, "python-v050-candidate-document.txt")
	var c pyCommercial
	decode(t, load(t, "python-v050-commercial.json"), &c)
	var registry struct {
		Records []pyRecord `json:"records"`
	}
	decode(t, load(t, "python-v050-registry.json"), &registry)
	var doc struct {
		BusinessHash    string `json:"business_hash"`
		DocumentNumber  string `json:"document_number"`
		ContentSHA256   string `json:"content_sha256"`
		SizeBytes       int    `json:"size_bytes"`
		MimeType        string `json:"mime_type"`
		RendererVersion string `json:"renderer_version"`
		ContentText     string `json:"content_text"`
	}
	decode(t, load(t, "python-v050-document.json"), &doc)

	template, s := pySheet(t, c, registry.Records, doc.DocumentNumber, doc.BusinessHash)
	got := text(template, s)
	if !bytes.Equal(got, want) {
		t.Fatalf("differs from candidate-document.txt at byte %d:\n%s", firstDifference(got, want), got)
	}
	if doc.ContentText != string(want) || doc.ContentSHA256 != quote.Hash(got) || doc.SizeBytes != len(got) ||
		doc.MimeType != MimeText || doc.RendererVersion != RendererVersion {
		t.Fatalf("document.json disagrees: %+v", doc)
	}
}

type renderVector struct {
	Name          string          `json:"name"`
	Number        string          `json:"number"`
	BusinessHash  string          `json:"business_hash"`
	Commercial    json.RawMessage `json:"commercial"`
	Records       []pyRecord      `json:"records"`
	ContentText   string          `json:"content_text"`
	ContentSHA256 string          `json:"content_sha256"`
	SizeBytes     int             `json:"size_bytes"`
}

func renderVectors(t *testing.T) map[string]renderVector {
	t.Helper()
	var vs []renderVector
	decode(t, load(t, "python-v050-render-vectors.json"), &vs)
	out := map[string]renderVector{}
	for _, v := range vs {
		if quote.Hash([]byte(v.ContentText)) != v.ContentSHA256 || len(v.ContentText) != v.SizeBytes {
			t.Fatalf("%s: the vector's hash or size is not its text's", v.Name)
		}
		out[v.Name] = v
	}
	if len(out) != 3 {
		t.Fatalf("%d vectors", len(out))
	}
	return out
}

// Python document.render on derived inputs: the example after Python's own normalization
// (decimals trimmed, as a business-hashed document holds them), and a variant with three lines
// out of line_id order (L1, L10, L2 by code point), an included and a separate charge in document
// order, two payment stages in calculation order with business days and two calendars, JPY with 0
// minor digits, the sections reversed, empty customer notes, CR LF and supplementary-plane
// characters in names, notes and the title.
func TestPythonRenderVectors(t *testing.T) {
	vectors := renderVectors(t)
	for _, name := range []string{"candidate normalized", "variant"} {
		v := vectors[name]
		t.Run(name, func(t *testing.T) {
			var c pyCommercial
			decode(t, v.Commercial, &c)
			template, s := pySheet(t, c, v.Records, v.Number, v.BusinessHash)
			if got := text(template, s); !bytes.Equal(got, []byte(v.ContentText)) {
				t.Fatalf("differs at byte %d:\n%s", firstDifference(got, []byte(v.ContentText)), got)
			}
		})
	}
}

type plainVector struct {
	Name          string            `json:"name"`
	TemplateText  string            `json:"template_text"`
	FieldNames    []string          `json:"field_names"`
	Number        string            `json:"number"`
	Values        map[string]string `json:"values"`
	ContentText   string            `json:"content_text"`
	ContentSHA256 string            `json:"content_sha256"`
}

// Python brant_core/foundation.py document_render: the draft banner, then the template text through
// string.Formatter().parse with each field's value, doubled braces made single, CR LF and line
// breaks inside values kept, a field used twice filled twice and unused values ignored.
func TestPlainTextVectors(t *testing.T) {
	var vs []plainVector
	decode(t, load(t, "python-v050-plaintext-vectors.json"), &vs)
	if len(vs) != 3 {
		t.Fatalf("%d vectors", len(vs))
	}
	for _, v := range vs {
		t.Run(v.Name, func(t *testing.T) {
			f, err := RenderPlain(PlainTemplate{DocumentType: "quote", Text: v.TemplateText, FieldNames: v.FieldNames},
				Number{Value: v.Number, DocumentType: "quote"}, v.Values)
			if err != nil {
				t.Fatal(err)
			}
			if string(f.Content) != v.ContentText || f.SHA256 != v.ContentSHA256 || f.Size != len(v.ContentText) {
				t.Fatalf("got %q", f.Content)
			}
		})
	}
}

func firstDifference(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}
