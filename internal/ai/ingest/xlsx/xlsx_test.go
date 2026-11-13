package xlsx

import (
	"archive/zip"
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/ingest/ingesttest"
)

var q01 = DefaultLimits(3, 500, 40, 50000)

// rfq is a synthetic inquiry workbook (no customer data): a header row, two lines with a formula,
// a delivery date and a hidden helper column, and a hidden second sheet.
func rfq() ingesttest.Workbook {
	return ingesttest.Workbook{Sheets: []ingesttest.Sheet{
		{Name: "询价单", Cells: []ingesttest.Cell{
			{Ref: "A1", Text: "款号"}, {Ref: "B1", Text: "数量"}, {Ref: "C1", Text: "单位"}, {Ref: "D1", Text: "交期"},
			{Ref: "A2", Text: "ST-100"}, {Ref: "B2", Number: "120"}, {Ref: "C2", Text: "PCS"}, {Ref: "D2", Number: "46339", Style: ingesttest.Date},
			{Ref: "A3", Text: "SP-200"}, {Ref: "B3", Number: "24", Formula: "2*12"}, {Ref: "C3", Text: "PCS"}, {Ref: "D3", Number: "46339.3541666667", Style: ingesttest.DateTime},
			{Ref: "E2", Text: "helper"},
		}, HiddenCols: []int{5}},
		{Name: "Notes", State: "hidden", Cells: []ingesttest.Cell{{Ref: "A1", Inline: "line one\nline two"}}},
	}}
}

// Values only: shared and inline strings, numbers, a formula's cached value (not evaluated), dates
// from serials, hidden sheets and columns disclosed (decision E-8; baseline 14.2 仅单元格值).
func TestReadValuesOnly(t *testing.T) {
	wb, err := Read(rfq().Bytes(), q01)
	if err != nil {
		t.Fatal(err)
	}
	if len(wb.Sheets) != 2 || wb.Sheets[0].Name != "询价单" || wb.Sheets[1].Name != "Notes" || !wb.Sheets[1].Hidden || wb.Sheets[0].Hidden {
		t.Fatalf("sheets %+v", wb.Sheets)
	}
	want := []Cell{
		{Row: 1, Col: 1, Value: "款号"}, {Row: 1, Col: 2, Value: "数量"}, {Row: 1, Col: 3, Value: "单位"}, {Row: 1, Col: 4, Value: "交期"},
		{Row: 2, Col: 1, Value: "ST-100"}, {Row: 2, Col: 2, Value: "120"}, {Row: 2, Col: 3, Value: "PCS"},
		{Row: 2, Col: 4, Value: "2026-11-13", Date: true}, {Row: 2, Col: 5, Value: "helper"},
		{Row: 3, Col: 1, Value: "SP-200"}, {Row: 3, Col: 2, Value: "24", Formula: true}, {Row: 3, Col: 3, Value: "PCS"},
		{Row: 3, Col: 4, Value: "2026-11-13T08:30:00", Date: true},
	}
	got := wb.Sheets[0].Cells
	if len(got) != len(want) {
		t.Fatalf("cells %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cell %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if h := wb.Sheets[0].HiddenColumns; len(h) != 1 || h[0] != 5 {
		t.Fatalf("hidden columns %v", h)
	}
	if c := wb.Sheets[1].Cells; len(c) != 1 || c[0].Value != "line one\nline two" {
		t.Fatalf("inline %+v", c)
	}
	if wb.Drawings || wb.Comments || wb.Embedded || wb.ExternalLinks {
		t.Fatalf("flags %+v", wb)
	}
}

func TestNumbersKeepFifteenDigitsWithoutFloats(t *testing.T) {
	for in, want := range map[string]string{
		"120": "120", "0.10000000000000001": "0.1", "4.4999999999999998E-2": "0.045", "1.5E-3": "0.0015",
		"-0": "0", "+7": "7", ".5": "0.5", "5.": "5", "1e5": "100000", "12345678901234567": "12345678901234600",
		"99999999999999999": "100000000000000000", "123456789012345678901234": "1.23456789012346E23",
		"1E-20": "1E-20", "-2.50": "-2.5", "0.000001": "0.000001", "960.00": "960",
	} {
		if got, ok := number(in); !ok || got != want {
			t.Errorf("number(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "-", "abc", "1.2.3", "INF", "NaN", "1e", "e5", "1e999", "0x10", "1,5"} {
		if got, ok := number(bad); ok {
			t.Errorf("number(%q) = %q", bad, got)
		}
	}
}

func TestDatesFromSerials(t *testing.T) {
	cases := []struct {
		serial string
		kind   dateKind
		d1904  bool
		want   string
	}{
		{"46339", dateOnly, false, "2026-11-13"},
		{"46339.3541666667", dateTime, false, "2026-11-13T08:30:00"},
		{"46339.9999999999", dateTime, false, "2026-11-14T00:00:00"},
		{"0.5", timeOnly, false, "12:00:00"},
		{"1", dateOnly, false, "1900-01-01"},
		{"59", dateOnly, false, "1900-02-28"},
		{"61", dateOnly, false, "1900-03-01"},
		{"45000", dateOnly, true, "2027-03-16"},
		{"0", dateOnly, true, "1904-01-01"},
		{"2958465", dateOnly, false, "9999-12-31"},
	}
	for _, c := range cases {
		if got, ok := date(c.serial, c.kind, c.d1904); !ok || got != c.want {
			t.Errorf("date(%s) = %q, %v; want %q", c.serial, got, ok, c.want)
		}
	}
	for _, serial := range []string{"60", "0", "-1", "2958466", "1E21"} {
		if got, ok := date(serial, dateOnly, false); ok {
			t.Errorf("date(%s) = %q, want the number kept", serial, got)
		}
	}
	for code, want := range map[string]dateKind{
		`yyyy"年"m"月"d"日"`: dateOnly, "m/d/yyyy h:mm": dateTime, "h:mm AM/PM": timeOnly, "mm:ss": timeOnly,
		"[h]:mm:ss": notDate, "0.00": notDate, "0.00E+00": notDate, "General": notDate, `#,##0 "pcs"`: notDate,
		"[Red]0.00": notDate, "[$-404]e/m/d": dateOnly, "mmm-yy": dateOnly, `0.00_);(0.00)`: notDate, "@": notDate,
	} {
		if got := classify(code); got != want {
			t.Errorf("classify(%q) = %d, want %d", code, got, want)
		}
	}
}

func TestOtherValueTypes(t *testing.T) {
	wb := ingesttest.Workbook{Sheets: []ingesttest.Sheet{{Name: "S", Cells: []ingesttest.Cell{
		{Ref: "A1", Bool: "1"}, {Ref: "B1", Bool: "0"}, {Ref: "C1", Error: "#DIV/0!", Formula: "1/0"},
		{Ref: "D1", Text: "carriage_x000D_\r\nreturn and _x005F_x0041_"}, {Ref: "E1", Number: "0.5", Style: ingesttest.Fixed2},
		{Ref: "F1", Number: "46339", Style: ingesttest.DateZH}, {Ref: "G1", Text: "x", Formula: `"x"`},
	}}}}
	got, err := Read(wb.Bytes(), q01)
	if err != nil {
		t.Fatal(err)
	}
	values := []string{}
	for _, c := range got.Sheets[0].Cells {
		values = append(values, c.Value)
	}
	// XML reads CR LF in character data as LF, and _x000D_ before it makes CR LF again: one line end.
	want := "TRUE|FALSE|#DIV/0!|carriage\nreturn and _x0041_|0.5|2026-11-13|x"
	if strings.Join(values, "|") != want {
		t.Fatalf("values %q", values)
	}
	if c := got.Sheets[0].Cells[2]; !c.Error || !c.Formula {
		t.Fatalf("error cell %+v", c)
	}
}

// A workbook beyond Q01.upload.v0.1 or not of plain cell values is refused, never cut (baseline
// 14.2): sheets, rows, columns, characters, macros, formulas without values, control characters.
func TestReadRefuses(t *testing.T) {
	cells := func(n, cols int) []ingesttest.Cell {
		var out []ingesttest.Cell
		for r := 1; r <= n; r++ {
			for c := 1; c <= cols; c++ {
				out = append(out, ingesttest.Cell{Ref: column(c) + strconv.Itoa(r), Number: "1"})
			}
		}
		return out
	}
	wide := []ingesttest.Cell{}
	for c := 1; c <= 41; c++ {
		wide = append(wide, ingesttest.Cell{Ref: column(c) + "1", Number: "1"})
	}
	one := func(s ...ingesttest.Sheet) []byte { return ingesttest.Workbook{Sheets: s}.Bytes() }
	sheet := func(name string, c ...ingesttest.Cell) ingesttest.Sheet {
		return ingesttest.Sheet{Name: name, Cells: c}
	}
	a1 := ingesttest.Cell{Ref: "A1", Number: "1"}
	cases := []struct {
		name string
		data []byte
		lim  Limits
		want Error
	}{
		{"four sheets", one(sheet("a", a1), sheet("b", a1), sheet("c", a1), sheet("d", a1)), q01, Error{Reason: ReasonSheets, Actual: 4, Max: 3}},
		{"501 rows", one(ingesttest.Sheet{Name: "S", Cells: cells(501, 1)}), q01, Error{Reason: ReasonRows, Sheet: "S", Actual: 501, Max: 500, AtLeast: true}},
		{"41 columns", one(ingesttest.Sheet{Name: "S", Cells: wide}), q01, Error{Reason: ReasonColumns, Sheet: "S", Actual: 41, Max: 40, AtLeast: true}},
		{"characters", one(sheet("S", ingesttest.Cell{Ref: "A1", Text: strings.Repeat("询", 30)}, ingesttest.Cell{Ref: "A2", Text: strings.Repeat("价", 30)})),
			DefaultLimits(3, 500, 40, 60), Error{Reason: ReasonCharacters, Actual: 61, Max: 60, AtLeast: true}},
		{"formula without value", one(sheet("S", ingesttest.Cell{Ref: "B2", Formula: "SUM(A1:A9)"})), q01, Error{Reason: ReasonFormula, Sheet: "S", Row: 2, Col: 2}},
		{"control character", one(sheet("S", ingesttest.Cell{Ref: "C3", Text: "a_x0007_b"})), q01, Error{Reason: ReasonEncoding, Sheet: "S", Row: 3, Col: 3}},
		{"VBA project", ingesttest.Workbook{Sheets: []ingesttest.Sheet{sheet("S", a1)}, Parts: map[string]string{"xl/vbaProject.bin": "x"}}.Bytes(), q01, Error{Reason: ReasonMacros, Part: "xl/vbaProject.bin"}},
		{"macro-enabled", ingesttest.Workbook{Sheets: []ingesttest.Sheet{sheet("S", a1)}, ContentType: "application/vnd.ms-excel.sheet.macroEnabled.main+xml"}.Bytes(), q01, Error{Reason: ReasonMacros, Part: "xl/workbook.xml"}},
		{"Excel 4 macro sheet", ingesttest.Workbook{Sheets: []ingesttest.Sheet{sheet("S", a1)}, Parts: map[string]string{"xl/macrosheets/sheet1.xml": "<x/>"}}.Bytes(), q01, Error{Reason: ReasonMacros, Part: "xl/macrosheets/sheet1.xml"}},
		{"binary workbook", ingesttest.Workbook{Sheets: []ingesttest.Sheet{sheet("S", a1)}, ContentType: "application/vnd.ms-excel.sheet.binary.macroEnabled.main"}.Bytes(), q01, Error{Reason: ReasonType, Part: "xl/workbook.xml"}},
		{"not a zip", []byte("PK\x03\x04 not really"), q01, Error{Reason: ReasonStructure}},
		{"zip without workbook", ingesttest.Zip(map[string]string{"a.txt": "x"}), q01, Error{Reason: ReasonType}},
		{"duplicate cell", one(ingesttest.Sheet{Name: "S", SheetData: `<row r="1"><c r="A1"><v>1</v></c><c r="A1"><v>2</v></c></row>`}), q01, Error{Reason: ReasonStructure, Sheet: "S", Row: 1, Col: 1}},
		{"cell outside its row", one(ingesttest.Sheet{Name: "S", SheetData: `<row r="1"><c r="A2"><v>1</v></c></row>`}), q01, Error{Reason: ReasonStructure, Part: "xl/worksheets/sheet1.xml", Sheet: "S"}},
		{"shared string out of range", one(ingesttest.Sheet{Name: "S", SheetData: `<row r="1"><c r="A1" t="s"><v>7</v></c></row>`}), q01, Error{Reason: ReasonStructure, Sheet: "S", Row: 1, Col: 1}},
		{"entity", one(ingesttest.Sheet{Name: "S", SheetData: `<row r="1"><c r="A1" t="inlineStr"><is><t>&xxe;</t></is></c></row>`}), q01, Error{Reason: ReasonStructure, Part: "xl/worksheets/sheet1.xml"}},
		{"part beyond the bound", one(ingesttest.Sheet{Name: "S", SheetData: `<row r="1"><c r="A1"><v>1</v></c></row>` + strings.Repeat(" ", 4<<20)}),
			Limits{MaxSheets: 3, MaxRows: 500, MaxColumns: 40, MaxCharacters: 50000, MaxEntries: 1000, MaxPartBytes: 1 << 20, MaxTotalBytes: 96 << 20}, Error{Reason: ReasonStructure, Part: "xl/worksheets/sheet1.xml"}},
		{"total beyond the bound", one(sheet("a", a1), sheet("b", a1), sheet("c", a1)),
			Limits{MaxSheets: 3, MaxRows: 500, MaxColumns: 40, MaxCharacters: 50000, MaxEntries: 1000, MaxPartBytes: 1 << 20, MaxTotalBytes: 2000}, Error{Reason: ReasonStructure}},
		{"too many entries", ingesttest.Workbook{Sheets: []ingesttest.Sheet{sheet("S", a1)}, Parts: manyParts(1000)}.Bytes(), q01, Error{Reason: ReasonStructure, Actual: 1007, Max: 1000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wb, err := Read(tc.data, tc.lim)
			var e *Error
			if wb != nil || !errors.As(err, &e) {
				t.Fatalf("workbook %v, error %v", wb, err)
			}
			if tc.want.Reason == ReasonStructure && tc.want.Part == "" && tc.want.Sheet == "" && tc.want.Max == 0 {
				if e.Reason != ReasonStructure {
					t.Fatalf("error %+v", e)
				}
				return
			}
			if *e != tc.want {
				t.Fatalf("error %+v, want %+v", *e, tc.want)
			}
		})
	}
}

// A workbook declared by the Default content type of its extension, with byte order marks, as some
// libraries write it, is read like one declared by an Override.
func TestWorkbookDeclaredByDefault(t *testing.T) {
	wb := rfq()
	wb.ContentType = "application/xml" // the Override no longer says what the workbook is
	if _, err := Read(wb.Bytes(), q01); err == nil {
		t.Fatal("a workbook declared as plain XML was read")
	}
	parts := map[string]string{}
	z, _ := zip.NewReader(bytes.NewReader(rfq().Bytes()), int64(len(rfq().Bytes())))
	for _, f := range z.File {
		rc, _ := f.Open()
		var b bytes.Buffer
		_, _ = b.ReadFrom(rc)
		_ = rc.Close()
		parts[f.Name] = "\ufeff" + b.String()
	}
	parts["[Content_Types].xml"] = strings.Replace(strings.Replace(parts["[Content_Types].xml"],
		`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`, "", 1),
		`<Default Extension="xml" ContentType="application/xml"/>`,
		`<Default Extension="XML" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`, 1)
	got, err := Read(ingesttest.Zip(parts), q01)
	if err != nil || len(got.Sheets) != 2 || len(got.Sheets[0].Cells) != 13 {
		t.Fatalf("workbook %+v, %v", got, err)
	}
}

// A zip with two entries of the same name (in any case) is ambiguous: readers could pick either.
func TestDuplicateEntriesAreRefused(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"xl/workbook.xml", "XL/Workbook.xml"} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte("<workbook/>"))
	}
	_ = zw.Close()
	_, err := Read(buf.Bytes(), q01)
	var e *Error
	if !errors.As(err, &e) || e.Reason != ReasonStructure || e.Part != "XL/Workbook.xml" {
		t.Fatalf("error %v", err)
	}
}

// Parts the reader does not read are disclosed; a relationship that leaves the package is refused.
func TestUnreadPartsAndEscapingTargets(t *testing.T) {
	wb := rfq()
	wb.Parts = map[string]string{"xl/drawings/drawing1.xml": "<x/>", "xl/comments1.xml": "<x/>",
		"xl/embeddings/oleObject1.bin": "x", "xl/externalLinks/externalLink1.xml": "<x/>"}
	got, err := Read(wb.Bytes(), q01)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Drawings || !got.Comments || !got.Embedded || !got.ExternalLinks {
		t.Fatalf("flags %+v", got)
	}
	evil := rfq()
	evil.Parts = map[string]string{"xl/_rels/workbook.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="../../../etc/passwd"/></Relationships>`}
	if _, err = Read(evil.Bytes(), q01); err == nil {
		t.Fatal("a target outside the package was followed")
	}
}

func manyParts(n int) map[string]string {
	out := map[string]string{}
	for i := 0; i < n; i++ {
		out["docProps/x"+strconv.Itoa(i)+".xml"] = "<x/>"
	}
	return out
}

func column(c int) string {
	var b []byte
	for ; c > 0; c = (c - 1) / 26 {
		b = append([]byte{byte('A' + (c-1)%26)}, b...)
	}
	return string(b)
}
