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

// A quote that stands at several places of the extract takes the one place in the row the agent
// named, when it stands exactly once in that row (owner decision 2026-11-18 20:15, after the live
// trials bin/ai-eval/live-20261118T115840Z normal-1 and normal-4: the model quoted each field's own
// words and placed all of them where its row begins, and the unit "PCS" stands in every row, so a
// person named the place four times a trial). In every other case nothing changes: no place named,
// a place of another notation, a row the quote does not stand in, a row it stands in twice. The
// choice is among the places the extract proves (AI-INV-004).
//
// A row is what Row says: a text line, a PDF line (which is its own place, so a PDF quote that
// stands in the named line is verified before the rule is reached), a sheet's row.
func TestAQuoteAtSeveralPlacesTakesTheOneInTheRowTheAgentNamed(t *testing.T) {
	text, err := ingest.Q01Upload.ExtractText([]byte(strings.Join([]string{
		"Code | Item | Qty | Unit",
		"sku.1 | shirt red | 120 | PCS",
		"sku.2 | shirt blue | 60 | PCS",
		"sku.3 | jacket, 12 PCS pack | 48 | PCS",
		"sku.4 | bag | 24 |",
	}, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := ingest.Q01Upload.ExtractPDF(ingesttest.PDF{Pages: []ingesttest.PDFPage{
		{Texts: []ingesttest.PDFText{
			{Font: "F1", Size: 12, X: 72, Y: 780, Text: "Code Item Qty Unit"},
			{Font: "F1", Size: 12, X: 72, Y: 760, Text: "sku.1 shirt 120 PCS"},
			{Font: "F1", Size: 12, X: 72, Y: 740, Text: "sku.2 jacket 12 PCS 48 PCS"},
			{Font: "F1", Size: 12, X: 72, Y: 720, Text: "sku.3 bag 24"},
		}},
		{Texts: []ingesttest.PDFText{{Font: "F1", Size: 12, X: 72, Y: 780, Text: "sku.4 scarf 6 PCS"}}},
	}}.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	xlsx, err := ingest.Q01Upload.ExtractXLSX(ingesttest.Workbook{Sheets: []ingesttest.Sheet{
		{Name: "询价单", Cells: []ingesttest.Cell{
			{Ref: "A1", Text: "款号"}, {Ref: "B1", Text: "数量"}, {Ref: "C1", Text: "单位"}, {Ref: "D1", Text: "备注"},
			{Ref: "A2", Text: "ST-100"}, {Ref: "B2", Number: "120"}, {Ref: "C2", Text: "PCS"},
			{Ref: "A3", Text: "ST-200"}, {Ref: "B3", Number: "60"}, {Ref: "C3", Text: "PCS"}, {Ref: "D3", Text: "PCS pack"},
			{Ref: "A4", Text: "ST-300"}, {Ref: "B4", Number: "24"},
		}},
		{Name: "Q1 2026", Cells: []ingesttest.Cell{{Ref: "A1", Text: "ST-400"}, {Ref: "C1", Text: "PCS"}}},
	}}.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	const (
		textPCS = "[2:27 3:27 4:20 4:36]"
		pdfPCS  = "[1:2 1:3 2:1]"
		xlsxPCS = "[询价单!C2 询价单!C3 询价单!D3 'Q1 2026'!C1]"
	)
	for _, c := range []struct {
		name        string
		e           *ingest.Extract
		quote, hint string
		want        string
		ambiguous   bool
	}{
		// Text: line:col; the row is the line.
		{"text, once in the named row", text, "PCS", "2:1", "[2:27]", false},
		{"text, any column of the named row", text, "PCS", "3:5", "[3:27]", false},
		{"text, a column the row does not have", text, "PCS", "2:999", "[2:27]", false},
		{"text, twice in the named row", text, "PCS", "4:1", textPCS, true},
		{"text, not in the named row (the header)", text, "PCS", "1:1", textPCS, true},
		{"text, a row without the quote", text, "PCS", "5:1", textPCS, true},
		{"text, a row beyond the extract", text, "PCS", "9:1", textPCS, true},
		{"text, no place named", text, "PCS", "", textPCS, true},
		{"text, a place of another notation", text, "PCS", "Sheet1!C2", textPCS, true},
		{"text, not a place at all", text, "PCS", "row 2", textPCS, true},
		{"text, the precise place wins over the row", text, "PCS", "4:36", "[4:36]", false},
		{"text, the quote at one place and another row named", text, "120", "3:1", "[2:21]", false},
		// PDF: page:line; a line is its own row, so a quote in the named line is verified there.
		{"pdf, the named line holds it", pdf, "PCS", "1:2", "[1:2]", false},
		{"pdf, a line holding it twice is one place", pdf, "PCS", "1:3", "[1:3]", false},
		{"pdf, the same line number of the next page", pdf, "PCS", "2:1", "[2:1]", false},
		{"pdf, a line without the quote", pdf, "PCS", "1:4", pdfPCS, true},
		{"pdf, the header line", pdf, "PCS", "1:1", pdfPCS, true},
		{"pdf, a line beyond the page", pdf, "PCS", "2:2", pdfPCS, true},
		{"pdf, no place named", pdf, "PCS", "", pdfPCS, true},
		{"pdf, the quote at one place and another line named", pdf, "scarf", "1:2", "[2:1]", false},
		// XLSX: Sheet!B3; the row is a sheet's row, and the rows of two sheets are two rows.
		{"xlsx, once in the named row", xlsx, "PCS", "询价单!A2", "[询价单!C2]", false},
		{"xlsx, the named cell in another case", xlsx, "PCS", "询价单!b2", "[询价单!C2]", false},
		{"xlsx, a cell the row has no value in", xlsx, "PCS", "询价单!Z2", "[询价单!C2]", false},
		{"xlsx, twice in the named row", xlsx, "PCS", "询价单!A3", xlsxPCS, true},
		{"xlsx, not in the named row (the header)", xlsx, "PCS", "询价单!A1", xlsxPCS, true},
		{"xlsx, a row without the quote", xlsx, "PCS", "询价单!A4", xlsxPCS, true},
		{"xlsx, the same row of another sheet", xlsx, "PCS", "'Q1 2026'!A1", "['Q1 2026'!C1]", false},
		{"xlsx, no place named", xlsx, "PCS", "", xlsxPCS, true},
		{"xlsx, a row notation is no place", xlsx, "PCS", "询价单!2:2", xlsxPCS, true},
		{"xlsx, a place of another notation", xlsx, "PCS", "2:1", xlsxPCS, true},
		{"xlsx, the precise place wins over the row", xlsx, "PCS", "询价单!D3", "[询价单!D3]", false},
		{"xlsx, the quote at one place and another row named", xlsx, "ST-300", "询价单!A2", "[询价单!A4]", false},
	} {
		places, err := Find(c.e, c.quote, c.hint)
		if got := fmt.Sprint(places); got != c.want {
			t.Errorf("%s: places %s, want %s", c.name, got, c.want)
		}
		rej, ok := kernel.AsRejection(err)
		switch {
		case c.ambiguous && (!ok || rej.Code != codes.SourceAmbiguous):
			t.Errorf("%s: %v, want SOURCE_AMBIGUOUS", c.name, err)
		case !c.ambiguous && err != nil:
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// rowShape is one kind of extract holding the same three data rows under a header row, rows 2 to 4,
// with the unit "PCS" in every one — the live trials' inquiry — and the notation of its places.
type rowShape struct {
	name, source string
	mime         string
	content      []byte
	// start is where a model that places every field at the start of its row puts a field of row r
	// (a PDF line has no start but itself); at is where a field's words stand; rowOf is the row in the
	// extract's notation, which is what a line without a place of its own takes.
	start func(r int) string
	at    func(r int, field string) string
	rowOf func(r int) string
	// uom is the status of the unit placed at its row's start.
	uom string
}

var rowData = []struct{ sku, item, qty string }{{"ST01.red.S", "tee red S", "24"}, {"ST01.red.M", "tee red M", "36"}, {"ST01.blue.S", "tee blue S", "12"}}

func quoteOf(r int, field string) string {
	d := rowData[r-2]
	switch field {
	case inqapi.FieldSkuID:
		return d.sku
	case inqapi.FieldRequestedItem:
		return d.item
	case inqapi.FieldQuantity:
		return d.qty
	}
	return "PCS"
}

func rowShapes() []rowShape {
	var textLines []string
	textLines = append(textLines, "Code | Item | Qty | Unit")
	var pdfTexts []ingesttest.PDFText
	pdfTexts = append(pdfTexts, ingesttest.PDFText{Font: "F1", Size: 12, X: 72, Y: 780, Text: "Code Item Qty Unit"})
	cells := []ingesttest.Cell{{Ref: "A1", Text: "Code"}, {Ref: "B1", Text: "Item"}, {Ref: "C1", Text: "Qty"}, {Ref: "D1", Text: "Unit"}}
	for i, d := range rowData {
		r := i + 2
		textLines = append(textLines, fmt.Sprintf("%s | %s | %s | PCS", d.sku, d.item, d.qty))
		pdfTexts = append(pdfTexts, ingesttest.PDFText{Font: "F1", Size: 12, X: 72, Y: float64(780 - 20*i - 20), Text: fmt.Sprintf("%s %s %s PCS", d.sku, d.item, d.qty)})
		cells = append(cells, ingesttest.Cell{Ref: fmt.Sprintf("A%d", r), Text: d.sku}, ingesttest.Cell{Ref: fmt.Sprintf("B%d", r), Text: d.item},
			ingesttest.Cell{Ref: fmt.Sprintf("C%d", r), Number: d.qty}, ingesttest.Cell{Ref: fmt.Sprintf("D%d", r), Text: "PCS"})
	}
	column := map[string]string{inqapi.FieldSkuID: "A", inqapi.FieldRequestedItem: "B", inqapi.FieldQuantity: "C", inqapi.FieldUOM: "D"}
	return []rowShape{
		{name: "text", source: "src.text", mime: evdapi.MimeText, content: []byte(strings.Join(textLines, "\n") + "\n"),
			start: func(r int) string { return fmt.Sprintf("%d:1", r) },
			at: func(r int, field string) string {
				return fmt.Sprintf("%d:%d", r, strings.Index(textLines[r-1], quoteOf(r, field))+1)
			},
			rowOf: func(r int) string { return fmt.Sprintf("%d:1", r) }, uom: Relocated},
		{name: "pdf", source: "src.pdf", mime: evdapi.MimePDF, content: ingesttest.PDF{Pages: []ingesttest.PDFPage{{Texts: pdfTexts}}}.Bytes(),
			start: func(r int) string { return fmt.Sprintf("1:%d", r) },
			at:    func(r int, _ string) string { return fmt.Sprintf("1:%d", r) },
			rowOf: func(r int) string { return fmt.Sprintf("1:%d", r) }, uom: Verified},
		{name: "xlsx", source: "src.xlsx", mime: evdapi.MimeXLSX, content: ingesttest.Workbook{Sheets: []ingesttest.Sheet{{Name: "Inquiry", Cells: cells}}}.Bytes(),
			start: func(r int) string { return fmt.Sprintf("Inquiry!A%d", r) },
			at:    func(r int, field string) string { return fmt.Sprintf("Inquiry!%s%d", column[field], r) },
			rowOf: func(r int) string { return fmt.Sprintf("Inquiry!%d:%d", r, r) }, uom: Relocated},
	}
}

// The live trials' draft, in each kind of extract: every field of a line quoted with its own words
// and placed where the row begins, the unit's words in every row. The unit takes the place in the
// row the agent named and the line takes that row, so a person confirms the line as it stands. A
// unit named in a row it does not stand in stays at every place, pending ambiguous. A line whose
// fields resolve into two rows — the unit named in the next row, where it stands once — has every
// field placed and no place of its own: the row rule is not the line's, which d8ed11a leaves empty
// when its fields stand in several rows, for a person to name (INQ: DRAFT_NOT_READY at the line's
// locator).
func TestALiveShapedDraftTakesItsUnitsFromTheRowsTheAgentNamed(t *testing.T) {
	ctx := context.Background()
	for _, s := range rowShapes() {
		t.Run(s.name, func(t *testing.T) {
			evd := newEVD()
			evd.register(t, s.source, s.mime, s.content)
			line := func(id string, r int, unitAt string) inqapi.RfqLine {
				d, start := rowData[r-2], s.start(r)
				return inqapi.RfqLine{LineID: id, SkuID: d.sku, RequestedItem: d.item, Quantity: d.qty, UOM: "PCS", SourceID: s.source,
					Locators: []inqapi.FieldLocator{loc(inqapi.FieldSkuID, start, d.sku), loc(inqapi.FieldRequestedItem, start, d.item),
						loc(inqapi.FieldQuantity, start, d.qty), loc(inqapi.FieldUOM, unitAt, "PCS")}}
			}
			// placed is where the words of a line of row r stand, the unit's in unitRow, in the order the
			// model listed them.
			placed := func(r, unitRow int) []inqapi.FieldLocator {
				var out []inqapi.FieldLocator
				for _, f := range []string{inqapi.FieldSkuID, inqapi.FieldRequestedItem, inqapi.FieldQuantity, inqapi.FieldUOM} {
					row := r
					if f == inqapi.FieldUOM {
						row = unitRow
					}
					out = append(out, loc(f, s.at(row, f), quoteOf(row, f)))
				}
				return out
			}
			header := s.start(1)
			lines := []inqapi.RfqLine{
				line("L1", 2, s.start(2)), // every field at its row's start
				line("L2", 3, s.start(4)), // the unit named in the next row
				line("L3", 4, header),     // the unit named in a row without it
			}
			got, err := Locator{Extracts: evd}.Lines(ctx, lines)
			if err != nil {
				t.Fatal(err)
			}
			want := []inqapi.RfqLine{
				{LineID: "L1", SkuID: "ST01.red.S", RequestedItem: "tee red S", Quantity: "24", UOM: "PCS", SourceID: s.source,
					Locator: s.rowOf(2), Locators: placed(2, 2)},
				{LineID: "L2", SkuID: "ST01.red.M", RequestedItem: "tee red M", Quantity: "36", UOM: "PCS", SourceID: s.source,
					Locator: "", Locators: placed(3, 4)},
				{LineID: "L3", SkuID: "ST01.blue.S", RequestedItem: "tee blue S", Quantity: "12", UOM: "PCS", SourceID: s.source,
					Locator: s.rowOf(4), Locators: append(placed(4, 4)[:3:3], loc(inqapi.FieldUOM, s.at(2, inqapi.FieldUOM), "PCS"),
						loc(inqapi.FieldUOM, s.at(3, inqapi.FieldUOM), "PCS"), loc(inqapi.FieldUOM, s.at(4, inqapi.FieldUOM), "PCS")),
					Pending: []inqapi.PendingField{{Field: inqapi.FieldUOM, Reason: inqapi.PendingAmbiguous}}},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("lines\n got %+v\nwant %+v", got, want)
			}
			// The located draft goes through the locating again unchanged: the places it keeps are the
			// ones the extract proves, wherever the model named them.
			if again, err := (Locator{Extracts: evd}).Lines(ctx, got); err != nil || !reflect.DeepEqual(again, got) {
				t.Fatalf("locating the located draft again\n got %+v (%v)\nwant %+v", again, err, got)
			}
			// The reviewer's read says how each unit was placed.
			findings, err := Locator{Extracts: evd}.Check(ctx, lines)
			if err != nil {
				t.Fatal(err)
			}
			status := map[string]string{}
			for _, f := range findings {
				if f.Field == inqapi.FieldUOM {
					status[f.LineID] = f.Status
				}
			}
			if status["L1"] != s.uom || status["L2"] != s.uom || status["L3"] != Ambiguous {
				t.Fatalf("units: %v, want %s %s %s", status, s.uom, s.uom, Ambiguous)
			}
		})
	}
}

// The place the row rule takes is a place like any other. A reader's note at it questions the
// field (pending noted) and the reviewer's read shows the note; a text extract's columns count
// characters, so a Chinese inquiry's rows are chosen the same way.
func TestThePlaceTakenFromTheRowKeepsItsNotes(t *testing.T) {
	ctx := context.Background()
	evd := newEVD()
	evd.register(t, "src.x", evdapi.MimeXLSX, ingesttest.Workbook{Sheets: []ingesttest.Sheet{
		{Name: "Inquiry", Cells: []ingesttest.Cell{
			{Ref: "A1", Text: "Code"}, {Ref: "B1", Text: "Qty"}, {Ref: "C1", Text: "Unit"},
			{Ref: "A2", Text: "ST-1"}, {Ref: "B2", Number: "24"}, {Ref: "C2", Text: "PCS"},
			{Ref: "A3", Text: "ST-2"}, {Ref: "B3", Number: "36"}, {Ref: "C3", Text: "PCS"},
		}, HiddenRows: []int{3}}}}.Bytes())
	evd.register(t, "src.cn", evdapi.MimeText, []byte("款号 | 品名 | 数量 | 单位\nST-1 | 红色衬衫 | 24 | 件\nST-2 | 蓝色衬衫 | 36 | 件\n"))
	lines := []inqapi.RfqLine{
		{LineID: "L1", SourceID: "src.x", Locators: []inqapi.FieldLocator{loc(inqapi.FieldSkuID, "Inquiry!A3", "ST-2"),
			loc(inqapi.FieldUOM, "Inquiry!A3", "PCS")}},
		{LineID: "L2", SourceID: "src.cn", Locators: []inqapi.FieldLocator{loc(inqapi.FieldSkuID, "3:1", "ST-2"),
			loc(inqapi.FieldUOM, "3:1", "件")}},
	}
	got, err := Locator{Extracts: evd}.Lines(ctx, lines)
	if err != nil {
		t.Fatal(err)
	}
	want := []inqapi.RfqLine{
		{LineID: "L1", SourceID: "src.x", Locator: "Inquiry!3:3", Locators: []inqapi.FieldLocator{loc(inqapi.FieldSkuID, "Inquiry!A3", "ST-2"),
			loc(inqapi.FieldUOM, "Inquiry!C3", "PCS")},
			Pending: []inqapi.PendingField{{Field: inqapi.FieldSkuID, Reason: inqapi.PendingNoted}, {Field: inqapi.FieldUOM, Reason: inqapi.PendingNoted}}},
		{LineID: "L2", SourceID: "src.cn", Locator: "3:1", Locators: []inqapi.FieldLocator{loc(inqapi.FieldSkuID, "3:1", "ST-2"),
			loc(inqapi.FieldUOM, "3:20", "件")}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lines\n got %+v\nwant %+v", got, want)
	}
	findings, err := Locator{Extracts: evd}.Check(ctx, lines)
	if err != nil {
		t.Fatal(err)
	}
	var says []string
	for _, f := range findings {
		says = append(says, fmt.Sprintf("%s.%s=%s%v%v", f.LineID, f.Field, f.Status, f.Places, f.Notes))
	}
	if s := strings.Join(says, " "); s != "L1.sku_id=verified[Inquiry!A3][hidden_row] L1.uom=relocated[Inquiry!C3][hidden_row] "+
		"L2.sku_id=verified[3:1][] L2.uom=relocated[3:20][]" {
		t.Fatalf("findings %s", s)
	}
}
