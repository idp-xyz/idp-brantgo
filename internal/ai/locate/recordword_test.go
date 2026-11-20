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

// A place the agent writes as the located form's record word, white space and a place of the
// extract's notation is that place (lead decision 2026-11-19, inside the owner's scope of
// 2026-11-18 13:15). In the live trial bin/ai-eval/live-20261119T082915Z normal-1 the model copied
// the word from the form it reads ("segment\t5:43\tPCS") into every place it wrote, the fields' and
// the line's own, so the gateway took none of them for a place, dropped the hints, and the unit
// "PCS" stayed ambiguous on all four lines.
//
// The reading is one tolerance and nothing else: Canonical drops one leading "segment" and the
// spaces and tabs after it, and what is left has to be a place as it stands. The quote must still
// stand where the gateway says it does (AI-INV-004); every rule that follows the reading is the
// rule of the bare place.
func TestAPlaceWrittenWithTheRecordWordIsThatPlace(t *testing.T) {
	for _, s := range rowShapes() {
		t.Run(s.name, func(t *testing.T) {
			e := newEVD().register(t, s.source, s.mime, s.content)
			p := s.at(3, inqapi.FieldUOM)
			for _, written := range []string{p, "segment " + p, "segment\t" + p, "segment    " + p, "segment \t \t" + p} {
				if got, ok := Canonical(e, written); !ok || got != p {
					t.Errorf("Canonical(%q) = %q, %v; want %q", written, got, ok, p)
				}
			}
			// Everything else that has the word, or something like it, around the place is not a place.
			for _, written := range []string{
				"segment",                   // the word alone
				"segment ",                  // the word and white space, no place
				"segment\t",                 //
				"segment " + p + " PCS",     // words after the place
				"segment " + p + " ",        // white space after it
				"segment " + p + "x",        // not a place of the notation
				" segment " + p,             // white space before the word
				"segment segment " + p,      // the word twice
				"segment\tsegment\t" + p,    //
				"Segment " + p,              // another case
				"SEGMENT " + p,              //
				"segments " + p,             // another word
				"segment: " + p,             // punctuation
				"segment, " + p,             //
				"segment\n" + p,             // a line feed is not the white space tolerated
				"segment\u00a0" + p,         // nor a no-break space
				"note " + p,                 // another record word
				"row " + p,                  //
				"line 3",                    //
				"line 3 " + p,               //
				"segment row " + p,          //
				p + " segment",              // the word behind the place
				"segment segment",           //
				"segment segment " + s.name, //
			} {
				if got, ok := Canonical(e, written); ok {
					t.Errorf("Canonical(%q) = %q, want not a place", written, got)
				}
			}
			// Glued to the place the word is no record word: for a sheet named "segmentInquiry" it
			// would be a place of another sheet, never of this one.
			if got, ok := Canonical(e, "segment"+p); ok && got == p {
				t.Errorf("Canonical(%q) = %q: the word needs white space after it", "segment"+p, got)
			}
		})
	}
}

// A sheet may be called "segment": its places are places with or without the word before them, and
// the word is dropped once.
func TestASheetNamedLikeTheRecordWord(t *testing.T) {
	evd := newEVD()
	e := evd.register(t, "src.x", evdapi.MimeXLSX, ingesttest.Workbook{Sheets: []ingesttest.Sheet{
		{Name: "segment", Cells: []ingesttest.Cell{{Ref: "A1", Text: "ST-1"}, {Ref: "C1", Text: "PCS"}}},
		{Name: "Q1 2026", Cells: []ingesttest.Cell{{Ref: "A1", Text: "ST-2"}, {Ref: "C1", Text: "CTN"}}},
	}}.Bytes())
	for _, c := range []struct{ written, want string }{
		{"segment!C1", "segment!C1"},
		{"segment segment!C1", "segment!C1"},
		{"segment\tsegment!c1", "segment!C1"},
		{"segment 'segment'!C1", "segment!C1"},
		{"segment 'Q1 2026'!C1", "'Q1 2026'!C1"},
		{"segment\t'Q1 2026'!c1", "'Q1 2026'!C1"},
		{"'Q1 2026'!C1", "'Q1 2026'!C1"},
		{"segment segment segment!C1", ""},
		{"segment segment", ""},
	} {
		got, ok := Canonical(e, c.written)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("Canonical(%q) = %q, %v; want %q", c.written, got, ok, c.want)
		}
	}
	for _, c := range []struct{ quote, written, want string }{
		{"PCS", "segment segment!C1", "[segment!C1]"},
		{"CTN", "segment 'Q1 2026'!A1", "['Q1 2026'!C1]"}, // the words stand at one other place: moved there
		{"CTN", "segment segment!A1", "['Q1 2026'!C1]"},
	} {
		if places, err := Find(e, c.quote, c.written); err != nil || fmt.Sprint(places) != c.want {
			t.Errorf("Find(%q, %q) = %v, %v; want %s", c.quote, c.written, places, err, c.want)
		}
	}
}

// wrongColumn is a place in row r that names the row and holds no word of the unit, as the live
// trial's line 3 did (7:43 where the unit stands at 7:45): for text two columns before the unit, for
// XLSX the item's column, for PDF the row's own line (a PDF line is a place of its own).
func wrongColumn(s rowShape, r int) string {
	switch s.name {
	case "text":
		line, col, _ := ingest.ParseTextLocator(s.at(r, inqapi.FieldUOM))
		return ingest.TextLocator(line, col-2)
	case "xlsx":
		return fmt.Sprintf("Inquiry!B%d", r)
	}
	return s.start(r)
}

// Every rule after the reading is the rule of the bare place (row_test.go): a field locator with the
// word is verified where the words stand, moved to the one place they stand when it names another,
// takes the one place in the row it names where the words stand in several rows, and is ambiguous
// where the bare place would be. The cases are the live trial's, in text, PDF and XLSX: the unit
// "PCS" stands in rows 2 to 4. Each case is also run without the word and must come out the same.
func TestAFieldLocatorWithTheRecordWordIsCheckedLikeTheBarePlace(t *testing.T) {
	ambiguousIn := func(err error) bool {
		rej, ok := kernel.AsRejection(err)
		return ok && rej.Code == codes.SourceAmbiguous
	}
	for _, s := range rowShapes() {
		t.Run(s.name, func(t *testing.T) {
			evd := newEVD()
			e := evd.register(t, s.source, s.mime, s.content)
			at := func(r int, field string) string { return "[" + s.at(r, field) + "]" }
			every := fmt.Sprintf("[%s %s %s]", s.at(2, inqapi.FieldUOM), s.at(3, inqapi.FieldUOM), s.at(4, inqapi.FieldUOM))
			for _, c := range []struct {
				name      string
				field     string
				row       int    // the row the words stand in
				hint      string // where the agent says they stand, without the word; "" writes the word alone
				want      string
				status    string
				ambiguous bool
			}{
				{"the sku at its place", inqapi.FieldSkuID, 3, s.at(3, inqapi.FieldSkuID), at(3, inqapi.FieldSkuID), Verified, false},
				{"the item at its place", inqapi.FieldRequestedItem, 3, s.at(3, inqapi.FieldRequestedItem), at(3, inqapi.FieldRequestedItem), Verified, false},
				{"the quantity at its place", inqapi.FieldQuantity, 3, s.at(3, inqapi.FieldQuantity), at(3, inqapi.FieldQuantity), Verified, false},
				{"the unit at its place", inqapi.FieldUOM, 3, s.at(3, inqapi.FieldUOM), at(3, inqapi.FieldUOM), Verified, false},
				// The words stand at one place and the agent names another: moved to where they stand.
				{"a sku named in another row", inqapi.FieldSkuID, 3, s.at(2, inqapi.FieldSkuID), at(3, inqapi.FieldSkuID), Relocated, false},
				{"a quantity named in the header row", inqapi.FieldQuantity, 4, s.start(1), at(4, inqapi.FieldQuantity), Relocated, false},
				// The words stand in every row: the row the agent names holds them once.
				{"the unit at the start of row 2", inqapi.FieldUOM, 2, s.start(2), at(2, inqapi.FieldUOM), s.uom, false},
				{"the unit at the start of row 3", inqapi.FieldUOM, 3, s.start(3), at(3, inqapi.FieldUOM), s.uom, false},
				{"the unit at the start of row 4", inqapi.FieldUOM, 4, s.start(4), at(4, inqapi.FieldUOM), s.uom, false},
				{"the unit in the wrong column of its row", inqapi.FieldUOM, 4, wrongColumn(s, 4), at(4, inqapi.FieldUOM), s.uom, false},
				// Held ambiguous as the bare place is: a row without the words, a row beyond the
				// extract, no place.
				{"the unit named in the header row", inqapi.FieldUOM, 3, s.start(1), every, Ambiguous, true},
				{"the unit named in a row beyond the extract", inqapi.FieldUOM, 3, s.start(9), every, Ambiguous, true},
				{"the unit named by the word alone", inqapi.FieldUOM, 3, "", every, Ambiguous, true},
			} {
				quote, written := quoteOf(c.row, c.field), "segment "+c.hint
				if c.hint == "" {
					written = "segment"
				}
				places, err := Find(e, quote, written)
				bare, bareErr := Find(e, quote, c.hint)
				if fmt.Sprint(places) != c.want || fmt.Sprint(bare) != c.want {
					t.Errorf("%s: places %v (without the word %v), want %s", c.name, places, bare, c.want)
				}
				if ambiguousIn(err) != c.ambiguous || ambiguousIn(bareErr) != c.ambiguous || (err != nil) != c.ambiguous || (bareErr != nil) != c.ambiguous {
					t.Errorf("%s: %v (without the word %v), want ambiguous %v", c.name, err, bareErr, c.ambiguous)
				}
				findings, err := Locator{Extracts: evd}.Check(context.Background(), []inqapi.RfqLine{{LineID: "L", SourceID: s.source,
					Locators: []inqapi.FieldLocator{loc(c.field, written, quote)}}})
				if err != nil || len(findings) != 1 || findings[0].Status != c.status || findings[0].Locator != written ||
					fmt.Sprint(findings[0].Places) != c.want {
					t.Errorf("%s: findings %+v (%v), want status %s at %s", c.name, findings, err, c.status, c.want)
				}
			}
		})
	}
}

// The unit stands in every row, and a place written as "segment <row>:<any column>" names that row
// whatever the column (text), or "segment <sheet>!<any cell of the row>" (XLSX): the unit takes the
// place in that row.
func TestTheUnitInEveryRowTakesThePlaceInTheRowWhateverTheColumn(t *testing.T) {
	for _, s := range rowShapes() {
		if s.name == "pdf" {
			continue // a line is a place of its own: there is no column
		}
		t.Run(s.name, func(t *testing.T) {
			e := newEVD().register(t, s.source, s.mime, s.content)
			for r := 2; r <= 4; r++ {
				var hints []string
				if s.name == "text" {
					for col := 1; col <= 60; col++ {
						hints = append(hints, fmt.Sprintf("%d:%d", r, col))
					}
				} else {
					for col := 'A'; col <= 'H'; col++ {
						hints = append(hints, fmt.Sprintf("Inquiry!%c%d", col, r))
					}
				}
				for _, hint := range hints {
					for _, written := range []string{"segment " + hint, "segment\t" + hint} {
						if places, err := Find(e, "PCS", written); err != nil || fmt.Sprint(places) != "["+s.at(r, inqapi.FieldUOM)+"]" {
							t.Errorf("Find(PCS, %q) = %v, %v; want %s", written, places, err, s.at(r, inqapi.FieldUOM))
						}
					}
				}
			}
		})
	}
}

// The live trial's draft, as the model sent it, in each kind of extract: every place, each field's
// and the line's own, written with the record word, the third line's unit in the wrong column. It
// is located as the same draft without the word is: every field at its own place, the unit taken
// from the row named, nothing pending, and no place with the word kept. The line's own place is
// stored as the place it reads as. Locating what comes out changes nothing.
func TestTheLiveDraftWithTheRecordWordIsLocatedAsWithout(t *testing.T) {
	ctx := context.Background()
	ids := []string{"L1", "L2", "L3"}
	for _, s := range rowShapes() {
		t.Run(s.name, func(t *testing.T) {
			evd := newEVD()
			evd.register(t, s.source, s.mime, s.content)
			draft := func(word string) []inqapi.RfqLine {
				var lines []inqapi.RfqLine
				for i, id := range ids {
					r, d := i+2, rowData[i]
					start, unitAt := s.start(r), s.start(r)
					if id == "L3" {
						unitAt = wrongColumn(s, r)
					}
					lines = append(lines, inqapi.RfqLine{LineID: id, SkuID: d.sku, RequestedItem: d.item, Quantity: d.qty, UOM: "PCS", SourceID: s.source,
						Locator: word + start, Locators: []inqapi.FieldLocator{loc(inqapi.FieldSkuID, word+start, d.sku),
							loc(inqapi.FieldRequestedItem, word+start, d.item), loc(inqapi.FieldQuantity, word+start, d.qty),
							loc(inqapi.FieldUOM, word+unitAt, "PCS")}})
				}
				return lines
			}
			got, err := Locator{Extracts: evd}.Lines(ctx, draft("segment "))
			if err != nil {
				t.Fatal(err)
			}
			bare, err := Locator{Extracts: evd}.Lines(ctx, draft(""))
			if err != nil || !reflect.DeepEqual(got, bare) {
				t.Fatalf("with the word\n got %+v\nwithout it (%v)\nwant %+v", got, err, bare)
			}
			for i, id := range ids {
				r, d := i+2, rowData[i]
				var places []inqapi.FieldLocator
				for _, f := range []string{inqapi.FieldSkuID, inqapi.FieldRequestedItem, inqapi.FieldQuantity, inqapi.FieldUOM} {
					places = append(places, loc(f, s.at(r, f), quoteOf(r, f)))
				}
				want := inqapi.RfqLine{LineID: id, SkuID: d.sku, RequestedItem: d.item, Quantity: d.qty, UOM: "PCS", SourceID: s.source,
					Locator: s.start(r), Locators: places}
				if !reflect.DeepEqual(got[i], want) {
					t.Errorf("%s\n got %+v\nwant %+v", id, got[i], want)
				}
			}
			if rendered := fmt.Sprintf("%+v", got); strings.Contains(rendered, "segment") {
				t.Errorf("a place with the record word is kept: %s", rendered)
			}
			if again, err := (Locator{Extracts: evd}).Lines(ctx, got); err != nil || !reflect.DeepEqual(again, got) {
				t.Fatalf("locating the located draft again\n got %+v (%v)\nwant %+v", again, err, got)
			}
		})
	}
}

// The word Canonical drops is the word the located form (brant-extract/1, ingest.Extract.Canonical)
// writes in front of every place, and what a model copies from a record ("segment", a tab, the
// place) is read as that place. The form is a contract; if its word ever changes this test stops
// the reading from silently missing it.
func TestTheRecordWordIsTheWordTheLocatedFormWrites(t *testing.T) {
	for _, s := range rowShapes() {
		t.Run(s.name, func(t *testing.T) {
			e := newEVD().register(t, s.source, s.mime, s.content)
			records := 0
			for _, line := range strings.Split(string(e.Canonical()), "\n") {
				fields := strings.Split(line, "\t")
				if len(fields) != 3 || fields[0] != recordWord {
					continue
				}
				records++
				if got, ok := Canonical(e, fields[0]+"\t"+fields[1]); !ok || got != fields[1] {
					t.Errorf("the form's record %q: Canonical(%q) = %q, %v; want %q", line, fields[0]+"\t"+fields[1], got, ok, fields[1])
				}
			}
			if records == 0 || records != len(e.Segments) {
				t.Errorf("the located form has %d records starting with %q, the extract has %d segments", records, recordWord, len(e.Segments))
			}
		})
	}
}

// A line's own place is read once, like a field's. A place of the notation is stored as that place,
// with the word or without it and whether or not the line has fields. What is not a place of the
// notation, with the word read off, is stored as the model wrote it, as before this unit: INQ takes
// any text of up to 200 characters there and the gateway never checks it (the model's "row of the
// red shirt" is kept in TestALineWithoutItsPlaceTakesTheRowOfItsFields, L5). A line of another
// source is not touched.
func TestALinesOwnPlaceIsStoredAsThePlaceItReadsAs(t *testing.T) {
	evd := newEVD()
	evd.register(t, "src.text", evdapi.MimeText, []byte("Code | Item | Qty | Unit\nsku.1 | shirt red | 120 | PCS\n"))
	evd.register(t, "src.x", evdapi.MimeXLSX, inquiry())
	evd.register(t, "src.pdf", evdapi.MimePDF, ingesttest.PDF{Pages: []ingesttest.PDFPage{{Texts: []ingesttest.PDFText{
		{Font: "F1", Size: 12, X: 72, Y: 780, Text: "Code Item Qty"}, {Font: "F1", Size: 12, X: 72, Y: 760, Text: "sku.9 jacket 48"}}}}}.Bytes())
	for _, c := range []struct{ source, written, want string }{
		{"src.text", "segment 2:1", "2:1"},
		{"src.text", "segment\t2:14", "2:14"},
		{"src.text", "2:1", "2:1"},
		{"src.text", "row 2", "row 2"},
		{"src.text", "segment row 2", "segment row 2"},
		{"src.text", "segment", "segment"},
		{"src.text", "segment 2:1 PCS", "segment 2:1 PCS"},
		{"src.text", "segment segment 2:1", "segment segment 2:1"},
		{"src.text", "note 2:1", "note 2:1"},
		{"src.text", "line 2", "line 2"},
		{"src.pdf", "segment 1:2", "1:2"},
		{"src.pdf", "segment 1:2 jacket", "segment 1:2 jacket"},
		{"src.x", "segment 询价单!C2", "询价单!C2"},
		{"src.x", "segment 询价单!c2", "询价单!C2"},
		{"src.x", "询价单!C2", "询价单!C2"},
		{"src.x", "segment 询价单!2:2", "segment 询价单!2:2"}, // a row, not a place: as before
		{"src.x", "row 2", "row 2"},
		{"src.mail", "segment 2:1", "segment 2:1"}, // not a file extract: left as it is
	} {
		lines := map[string]inqapi.RfqLine{"no fields": {LineID: "L", SourceID: c.source, Locator: c.written}}
		if c.source == "src.text" {
			lines["a field"] = inqapi.RfqLine{LineID: "L", SourceID: c.source, Locator: c.written, RequestedItem: "shirt",
				Locators: []inqapi.FieldLocator{loc(inqapi.FieldRequestedItem, "", "shirt")}}
		}
		for name, line := range lines {
			got, err := Locator{Extracts: evd}.Lines(context.Background(), []inqapi.RfqLine{line})
			if err != nil || got[0].Locator != c.want {
				t.Errorf("%s %q (%s): stored %q (%v), want %q", c.source, c.written, name, got[0].Locator, err, c.want)
			}
		}
	}
}
