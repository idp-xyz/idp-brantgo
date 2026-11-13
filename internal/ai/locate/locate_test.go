package locate

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/codes"
	"go.idp.xyz/idp-brant/internal/ai/ingest"
	"go.idp.xyz/idp-brant/internal/ai/ingest/ingesttest"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// fakeEVD serves sources and the extracts of file_extract sources as EVD would to one caller.
type fakeEVD struct {
	sources  map[string]evdapi.SourceView
	extracts map[string]evdapi.ExtractView
	denied   bool
	reads    []string
}

func (f *fakeEVD) GetSource(_ context.Context, id string) (evdapi.SourceView, error) {
	f.reads = append(f.reads, "source "+id)
	if f.denied {
		return evdapi.SourceView{}, kernel.Reject(kernel.CodePermissionDenied, "/", "")
	}
	v, ok := f.sources[id]
	if !ok {
		return v, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	return v, nil
}

func (f *fakeEVD) GetExtract(_ context.Context, id string) (evdapi.ExtractView, error) {
	f.reads = append(f.reads, "extract "+id)
	v, ok := f.extracts[id]
	if !ok {
		return v, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	return v, nil
}

// register adds a file_extract source read from content by the platform's extractor.
func (f *fakeEVD) register(t *testing.T, id, mime string, content []byte) *ingest.Extract {
	t.Helper()
	x, err := ingest.EVD{Profile: ingest.Q01Upload}.Extract(mime, content)
	if err != nil {
		t.Fatal(err)
	}
	x.FileID = "file." + id
	f.sources[id] = evdapi.SourceView{SourceID: id, SourceKind: evdapi.KindFileExtract, Extract: &x.ExtractRef}
	f.extracts[id] = evdapi.ExtractView{SourceID: id, ExtractRef: x.ExtractRef, LocatedForm: x.LocatedForm}
	e, err := ingest.FromEVD(f.extracts[id])
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func newEVD() *fakeEVD {
	return &fakeEVD{sources: map[string]evdapi.SourceView{"src.mail": {SourceID: "src.mail", SourceKind: evdapi.KindManualText}},
		extracts: map[string]evdapi.ExtractView{}}
}

// A synthetic inquiry workbook (decision E-6): style ST-100 asked twice (rows 2 and 4), SP-200's
// quantity a formula's cached value in a hidden row, no unit for SP-200.
func inquiry() []byte {
	return ingesttest.Workbook{Sheets: []ingesttest.Sheet{{Name: "询价单", Cells: []ingesttest.Cell{
		{Ref: "A1", Text: "款号"}, {Ref: "B1", Text: "数量"}, {Ref: "C1", Text: "单位"},
		{Ref: "A2", Text: "ST-100"}, {Ref: "B2", Number: "120"}, {Ref: "C2", Text: "PCS"},
		{Ref: "A3", Text: "SP-200"}, {Ref: "B3", Number: "24", Formula: "2*12"},
		{Ref: "A4", Text: "ST-100"}, {Ref: "B4", Number: "36"}, {Ref: "C4", Text: "PCS"},
	}, HiddenRows: []int{3}}}}.Bytes()
}

func loc(field, locator, quote string) inqapi.FieldLocator {
	return inqapi.FieldLocator{Field: field, Locator: locator, Quote: quote}
}

// Every field locator of a candidate line is checked against the extract (WBS S4-04b; BC-AT27
// 字段定位; AN01): verified where the quote stands, relocated to the one place it stands, every place
// kept and the field pending ambiguous where it stands twice, dropped and pending unlocated where it
// stands nowhere, pending noted where the reader noted the value (a formula's cached value in a
// hidden row); a value without a locator is pending unlocated; the model's own pending entries keep
// their candidates. Lines of other sources are left as they are.
func TestLinesVerifyAndCompleteTheCandidates(t *testing.T) {
	evd := newEVD()
	evd.register(t, "src.x", evdapi.MimeXLSX, inquiry())
	lines := []inqapi.RfqLine{
		{LineID: "L1", RequestedItem: "ST-100", Quantity: "120", UOM: "PCS", SourceID: "src.x", Locators: []inqapi.FieldLocator{
			loc("requested_item", "询价单!A2", "ST-100"), loc("quantity", "询价单!B9", "120"), loc("uom", "询价单!c2", "PCS")}},
		{LineID: "L2", RequestedItem: "SP-200", Quantity: "24", SourceID: "src.x", Locators: []inqapi.FieldLocator{
			loc("requested_item", "询价单!A3", "SP-200"), loc("quantity", "询价单!B3", "24")},
			Pending: []inqapi.PendingField{{Field: "uom", Reason: "missing"}}},
		{LineID: "L3", RequestedItem: "ST-100", Quantity: "36", SkuID: "sku.1", SourceID: "src.x", Locators: []inqapi.FieldLocator{
			loc("requested_item", "询价单!A9", "ST-100"), loc("quantity", "询价单!B4", "999")},
			Pending: []inqapi.PendingField{{Field: "sku_id", Reason: "ambiguous", Candidates: []string{"sku.1", "sku.2"}}}},
		{LineID: "L4", RequestedItem: "毛衣", Quantity: "10", SourceID: "src.mail", Locator: "第2段"},
	}
	l := Locator{Extracts: evd}
	got, err := l.Lines(context.Background(), lines)
	if err != nil {
		t.Fatal(err)
	}
	want := []inqapi.RfqLine{
		{LineID: "L1", RequestedItem: "ST-100", Quantity: "120", UOM: "PCS", SourceID: "src.x", Locators: []inqapi.FieldLocator{
			loc("requested_item", "询价单!A2", "ST-100"), loc("quantity", "询价单!B2", "120"), loc("uom", "询价单!C2", "PCS")}},
		{LineID: "L2", RequestedItem: "SP-200", Quantity: "24", SourceID: "src.x", Locators: []inqapi.FieldLocator{
			loc("requested_item", "询价单!A3", "SP-200"), loc("quantity", "询价单!B3", "24")},
			Pending: []inqapi.PendingField{{Field: "requested_item", Reason: "noted"}, {Field: "quantity", Reason: "noted"},
				{Field: "uom", Reason: "missing"}}},
		{LineID: "L3", RequestedItem: "ST-100", Quantity: "36", SkuID: "sku.1", SourceID: "src.x", Locators: []inqapi.FieldLocator{
			loc("requested_item", "询价单!A2", "ST-100"), loc("requested_item", "询价单!A4", "ST-100")},
			Pending: []inqapi.PendingField{{Field: "requested_item", Reason: "ambiguous"},
				{Field: "sku_id", Reason: "ambiguous", Candidates: []string{"sku.1", "sku.2"}}, {Field: "quantity", Reason: "unlocated"}}},
		lines[3],
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lines\n got %+v\nwant %+v", got, want)
	}
	// The source of each line is read once, through EVD as the caller (CT16).
	if fmt.Sprint(evd.reads) != "[source src.x extract src.x source src.mail]" {
		t.Fatalf("reads %v", evd.reads)
	}
	// The reviewer's read says the same field by field.
	findings, err := l.Check(context.Background(), lines)
	if err != nil {
		t.Fatal(err)
	}
	var says []string
	for _, f := range findings {
		says = append(says, fmt.Sprintf("%s.%s=%s%v%v", f.LineID, f.Field, f.Status, f.Places, f.Notes))
	}
	if got := strings.Join(says, " "); got != "L1.requested_item=verified[询价单!A2][] L1.quantity=relocated[询价单!B2][] "+
		"L1.uom=verified[询价单!C2][] L2.requested_item=verified[询价单!A3][hidden_row] "+
		"L2.quantity=verified[询价单!B3][hidden_row formula_cached_value] L3.requested_item=ambiguous[询价单!A2 询价单!A4][] "+
		"L3.quantity=unlocated[][] L3.sku_id=unlocated[][]" {
		t.Fatalf("findings %s", got)
	}
}

// Find gives the hint when the quote stands there, the one place it stands otherwise, and
// SOURCE_AMBIGUOUS with the places when it stands at several; text locators are line:col, PDF
// locators page:line, and invisible PDF text is a questioned place (AN10).
func TestFindAndNotes(t *testing.T) {
	text, err := ingest.Q01Upload.ExtractText([]byte("款号 ST-100 数量 120\n款号 ST-100 数量 36\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := Find(text, "ST-100", "2:4"); err != nil || fmt.Sprint(p) != "[2:4]" {
		t.Fatalf("%v %v", p, err)
	}
	if p, err := Find(text, "36", "1:9"); err != nil || fmt.Sprint(p) != "[2:14]" {
		t.Fatalf("%v %v", p, err)
	}
	p, err := Find(text, "ST-100", "")
	if rej, ok := kernel.AsRejection(err); !ok || rej.Code != codes.SourceAmbiguous || fmt.Sprint(p) != "[1:4 2:4]" ||
		rej.Fields[0].Reason != "2 places: 1:4, 2:4" {
		t.Fatalf("%v %v", p, err)
	}
	if p, err := Find(text, "毛衣", "1:1"); err != nil || p != nil {
		t.Fatalf("%v %v", p, err)
	}
	pdf, err := ingest.Q01Upload.ExtractPDF(ingesttest.PDF{Pages: []ingesttest.PDFPage{{Texts: []ingesttest.PDFText{
		{Font: "F1", Size: 12, X: 72, Y: 780, Text: "ST-100 qty 120 PCS"},
		{Font: "F1", Size: 1, X: 72, Y: 760, Text: "qty 999", Invisible: true},
	}}}}.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if p, err := Find(pdf, "999", ""); err != nil || fmt.Sprint(p) != "[1:2]" || fmt.Sprint(Notes(pdf, p[0])) != "[invisible_text]" {
		t.Fatalf("%v %v %v", p, err, Notes(pdf, "1:2"))
	}
	if n := Notes(pdf, "1:1"); n != nil {
		t.Fatalf("notes %v", n)
	}
}

// Locators into a source that is not a readable file extract are REFERENCE_INVALID; a caller
// without evd.read gets PERMISSION_DENIED; more files than a task may hold are
// INPUT_PROFILE_UNSUPPORTED (baseline 14.2), and so are more lines than a quote may hold.
func TestLinesRefusals(t *testing.T) {
	evd := newEVD()
	for _, id := range []string{"src.a", "src.b", "src.c", "src.d"} {
		evd.register(t, id, evdapi.MimeText, []byte("款号 "+id+" 数量 1\n"))
	}
	ctx := context.Background()
	l := Locator{Extracts: evd}
	code := func(err error) string {
		if rej, ok := kernel.AsRejection(err); ok {
			return rej.Code + " " + rej.Fields[0].Path
		}
		return fmt.Sprint(err)
	}
	located := func(id, source string) inqapi.RfqLine {
		return inqapi.RfqLine{LineID: id, SourceID: source, Locators: []inqapi.FieldLocator{loc("requested_item", "1:1", "款号")}}
	}
	for _, c := range []struct {
		lines []inqapi.RfqLine
		want  string
	}{
		{[]inqapi.RfqLine{located("L1", "src.a"), located("L2", "src.mail")}, "REFERENCE_INVALID /args/Lines/1/SourceID"},
		{[]inqapi.RfqLine{located("L1", "src.unknown")}, "REFERENCE_INVALID /args/Lines/0/SourceID"},
		{[]inqapi.RfqLine{located("L1", "src.a"), located("L2", "src.b"), located("L3", "src.c"), located("L4", "src.d")},
			"INPUT_PROFILE_UNSUPPORTED /args/Lines"},
		{make([]inqapi.RfqLine, inqapi.MaxLines+1), "INPUT_PROFILE_UNSUPPORTED /args/Lines"},
	} {
		if _, err := l.Lines(ctx, c.lines); code(err) != c.want {
			t.Errorf("%d lines: %s, want %s", len(c.lines), code(err), c.want)
		}
	}
	evd.denied = true
	if _, err := l.Lines(ctx, []inqapi.RfqLine{located("L1", "src.a")}); code(err) != "PERMISSION_DENIED /" {
		t.Fatalf("%s", code(err))
	}
	// Without a reader nothing can be located.
	if _, err := (Locator{}).Lines(ctx, []inqapi.RfqLine{located("L1", "src.a")}); code(err) != "REFERENCE_INVALID /args/Lines/0/SourceID" {
		t.Fatalf("%s", code(err))
	}
}
