package xlsx

import (
	"errors"
	"reflect"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/ingest/ingesttest"
)

// FuzzRead asserts that Read never panics on any bytes, refuses only with *Error, is deterministic,
// and returns only what the limits allow (WBS S4-04: fuzz targets for each reader).
func FuzzRead(f *testing.F) {
	f.Add(rfq().Bytes())
	f.Add(ingesttest.Workbook{Sheets: []ingesttest.Sheet{{Name: "S", Chartsheet: true}}}.Bytes())
	f.Add(ingesttest.Zip(map[string]string{"_rels/.rels": "<Relationships/>"}))
	f.Add([]byte("PK\x03\x04"))
	f.Fuzz(func(t *testing.T, data []byte) {
		check(t, data)
	})
}

// FuzzSheetData puts arbitrary text into a worksheet's <sheetData> of an otherwise valid workbook,
// so that the fuzzer reaches the cell, row and value paths.
func FuzzSheetData(f *testing.F) {
	for _, s := range []string{
		`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1"><f>1+1</f><v>2</v></c></row>`,
		`<row><c><v>1.5E3</v></c><c t="b"><v>1</v></c><c t="e"><v>#N/A</v></c></row><row hidden="1"><c t="inlineStr"><is><r><t>a</t></r><rPh><t>x</t></rPh></is></c></row>`,
		`<row r="2"><c r="D2" s="1"><v>46339.5</v></c><c r="E2" t="str"><f>"x"</f><v>_x000D_x</v></c></row>`,
		`<row r="1048576"><c r="XFD1048576" t="d"><v>2026-11-13</v></c></row>`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sheetData string) {
		wb := ingesttest.Workbook{Sheets: []ingesttest.Sheet{
			{Name: "S", SheetData: sheetData},
			{Name: "T", Cells: []ingesttest.Cell{{Ref: "A1", Text: "shared"}}},
		}}
		check(t, wb.Bytes())
	})
}

func check(t *testing.T, data []byte) {
	t.Helper()
	lim := DefaultLimits(3, 50, 10, 2000)
	lim.MaxPartBytes, lim.MaxTotalBytes = 1<<20, 4<<20
	wb, err := Read(data, lim)
	again, err2 := Read(data, lim)
	if !reflect.DeepEqual(wb, again) || !reflect.DeepEqual(err, err2) {
		t.Fatalf("two reads differ: %v / %v", err, err2)
	}
	if err != nil {
		var e *Error
		if wb != nil || !errors.As(err, &e) {
			t.Fatalf("workbook %v with error %v", wb, err)
		}
		return
	}
	if len(wb.Sheets) == 0 || len(wb.Sheets) > lim.MaxSheets {
		t.Fatalf("%d sheets", len(wb.Sheets))
	}
	chars := 0
	for _, s := range wb.Sheets {
		rows, cols := map[int]bool{}, map[int]bool{}
		for i, c := range s.Cells {
			rows[c.Row], cols[c.Col] = true, true
			if c.Value == "" || c.Row < 1 || c.Row > maxRow || c.Col < 1 || c.Col > maxColumn {
				t.Fatalf("cell %+v", c)
			}
			if i > 0 && (s.Cells[i-1].Row > c.Row || s.Cells[i-1].Row == c.Row && s.Cells[i-1].Col >= c.Col) {
				t.Fatalf("cells out of order: %+v then %+v", s.Cells[i-1], c)
			}
			for _, r := range c.Value {
				if control(r) {
					t.Fatalf("control character in %q", c.Value)
				}
				chars++
			}
			chars++
		}
		if len(rows) > lim.MaxRows || len(cols) > lim.MaxColumns {
			t.Fatalf("sheet %q: %d rows, %d columns", s.Name, len(rows), len(cols))
		}
	}
	if chars-1 > lim.MaxCharacters {
		t.Fatalf("%d characters", chars-1)
	}
}
