// Package ingesttest builds synthetic files for the tests of internal/ai/ingest and of the units
// that use it (B4 locating and candidates, D1 BC-AT27): XLSX workbooks and PDF documents made in the
// repository, byte for byte deterministic, with no customer data (decision E-6). It is meant for
// test files, and imports no Brant package, so that the readers' own tests can use it.
package ingesttest

import (
	"archive/zip"
	"bytes"
	"fmt"
	"html"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Workbook is a synthetic XLSX workbook.
type Workbook struct {
	Sheets      []Sheet
	Date1904    bool
	ContentType string            // the workbook part's content type; empty is the XLSX one
	Parts       map[string]string // more parts by name, e.g. xl/vbaProject.bin or xl/drawings/drawing1.xml
}

// Sheet is a worksheet (or a chart sheet).
type Sheet struct {
	Name        string
	State       string // "", "hidden" or "veryHidden"
	Chartsheet  bool
	Cells       []Cell
	HiddenRows  []int
	HiddenCols  []int
	SheetData   string // raw <sheetData> content, used instead of Cells when set
	ExtraBefore string // raw XML before <sheetData>, e.g. <cols>
}

// Cell styles of the workbook's style sheet (cellXfs index).
const (
	General  = 0
	Date     = 1 // built-in 14, m/d/yyyy
	DateTime = 2 // built-in 22, m/d/yyyy h:mm
	Time     = 3 // built-in 21, h:mm:ss
	DateZH   = 4 // custom yyyy"年"m"月"d"日"
	Fixed2   = 5 // built-in 2, 0.00
)

// Cell is one cell. Exactly one of Text (shared string), Inline, Number, Bool and Error is the
// value; Formula adds a formula whose cached value is that value (none when the value is empty).
type Cell struct {
	Ref     string
	Text    string
	Inline  string
	Number  string
	Bool    string
	Error   string
	Formula string
	Style   int
}

// Bytes returns the workbook as an XLSX package.
func (w Workbook) Bytes() []byte {
	parts := map[string]string{}
	var shared []string
	index := map[string]int{}
	ct := w.ContentType
	if ct == "" {
		ct = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"
	}
	var types, sheets, rels strings.Builder
	types.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/xl/workbook.xml" ContentType="` + ct + `"/>` +
		`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>` +
		`<Override PartName="/xl/sharedStrings.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sharedStrings+xml"/>`)
	const relNS = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	for i, s := range w.Sheets {
		n := i + 1
		kind, dir := "worksheet", "worksheets"
		if s.Chartsheet {
			kind, dir = "chartsheet", "chartsheets"
		}
		part := fmt.Sprintf("xl/%s/sheet%d.xml", dir, n)
		state := ""
		if s.State != "" {
			state = ` state="` + s.State + `"`
		}
		fmt.Fprintf(&sheets, `<sheet name="%s" sheetId="%d"%s r:id="rId%d"/>`, html.EscapeString(s.Name), n, state, n)
		fmt.Fprintf(&rels, `<Relationship Id="rId%d" Type="%s/%s" Target="%s/sheet%d.xml"/>`, n, relNS, kind, dir, n)
		fmt.Fprintf(&types, `<Override PartName="/%s" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.%s+xml"/>`, part, kind)
		if s.Chartsheet {
			parts[part] = `<?xml version="1.0" encoding="UTF-8"?><chartsheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`
			continue
		}
		parts[part] = s.xml(func(text string) int {
			i, ok := index[text]
			if !ok {
				i = len(shared)
				index[text] = i
				shared = append(shared, text)
			}
			return i
		})
	}
	types.WriteString(`</Types>`)
	n := len(w.Sheets)
	fmt.Fprintf(&rels, `<Relationship Id="rId%d" Type="%s/styles" Target="styles.xml"/>`, n+1, relNS)
	fmt.Fprintf(&rels, `<Relationship Id="rId%d" Type="%s/sharedStrings" Target="sharedStrings.xml"/>`, n+2, relNS)
	pr := ""
	if w.Date1904 {
		pr = `<workbookPr date1904="1"/>`
	}
	parts["[Content_Types].xml"] = types.String()
	parts["_rels/.rels"] = `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="` + relNS + `/officeDocument" Target="xl/workbook.xml"/></Relationships>`
	parts["xl/workbook.xml"] = `<?xml version="1.0" encoding="UTF-8"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="` + relNS + `">` +
		pr + `<sheets>` + sheets.String() + `</sheets></workbook>`
	parts["xl/_rels/workbook.xml.rels"] = `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		rels.String() + `</Relationships>`
	parts["xl/styles.xml"] = `<?xml version="1.0" encoding="UTF-8"?><styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<numFmts count="1"><numFmt numFmtId="164" formatCode="yyyy&quot;年&quot;m&quot;月&quot;d&quot;日&quot;"/></numFmts>` +
		`<cellStyleXfs count="1"><xf numFmtId="0"/></cellStyleXfs>` +
		`<cellXfs count="6"><xf numFmtId="0"/><xf numFmtId="14"/><xf numFmtId="22"/><xf numFmtId="21"/><xf numFmtId="164"/><xf numFmtId="2"/></cellXfs></styleSheet>`
	var sst strings.Builder
	fmt.Fprintf(&sst, `<?xml version="1.0" encoding="UTF-8"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="%d" uniqueCount="%d">`, len(shared), len(shared))
	for _, s := range shared {
		sst.WriteString(`<si><t xml:space="preserve">` + html.EscapeString(s) + `</t></si>`)
	}
	sst.WriteString(`</sst>`)
	parts["xl/sharedStrings.xml"] = sst.String()
	for name, content := range w.Parts {
		parts[name] = content
	}
	return Zip(parts)
}

func (s Sheet) xml(sharedIndex func(string) int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	b.WriteString(s.ExtraBefore)
	if len(s.HiddenCols) > 0 {
		b.WriteString(`<cols>`)
		for _, c := range s.HiddenCols {
			fmt.Fprintf(&b, `<col min="%d" max="%d" width="9" hidden="1"/>`, c, c)
		}
		b.WriteString(`</cols>`)
	}
	b.WriteString(`<sheetData>`)
	if s.SheetData != "" {
		b.WriteString(s.SheetData)
	} else {
		rows := map[int][]Cell{}
		for _, c := range s.Cells {
			r, err := strconv.Atoi(strings.TrimLeft(c.Ref, "ABCDEFGHIJKLMNOPQRSTUVWXYZ"))
			if err != nil {
				panic("ingesttest: bad cell reference " + c.Ref)
			}
			rows[r] = append(rows[r], c)
		}
		hidden := map[int]bool{}
		for _, r := range s.HiddenRows {
			hidden[r] = true
			if _, ok := rows[r]; !ok {
				rows[r] = nil
			}
		}
		numbers := make([]int, 0, len(rows))
		for r := range rows {
			numbers = append(numbers, r)
		}
		sort.Ints(numbers)
		for _, r := range numbers {
			attr := ""
			if hidden[r] {
				attr = ` hidden="1"`
			}
			fmt.Fprintf(&b, `<row r="%d"%s>`, r, attr)
			for _, c := range rows[r] {
				b.WriteString(c.xml(sharedIndex))
			}
			b.WriteString(`</row>`)
		}
	}
	b.WriteString(`</sheetData></worksheet>`)
	return b.String()
}

func (c Cell) xml(sharedIndex func(string) int) string {
	style := ""
	if c.Style != 0 {
		style = fmt.Sprintf(` s="%d"`, c.Style)
	}
	f := ""
	if c.Formula != "" {
		f = `<f>` + html.EscapeString(c.Formula) + `</f>`
	}
	v := func(t, value string) string {
		if value == "" {
			return fmt.Sprintf(`<c r="%s"%s%s>%s</c>`, c.Ref, t, style, f)
		}
		return fmt.Sprintf(`<c r="%s"%s%s>%s<v>%s</v></c>`, c.Ref, t, style, f, html.EscapeString(value))
	}
	switch {
	case c.Text != "" && c.Formula != "":
		return v(` t="str"`, c.Text)
	case c.Text != "":
		return v(` t="s"`, fmt.Sprint(sharedIndex(c.Text)))
	case c.Inline != "":
		return fmt.Sprintf(`<c r="%s" t="inlineStr"%s><is><t>%s</t></is></c>`, c.Ref, style, html.EscapeString(c.Inline))
	case c.Bool != "":
		return v(` t="b"`, c.Bool)
	case c.Error != "":
		return v(` t="e"`, c.Error)
	}
	return v("", c.Number)
}

// Zip packs parts (name -> content) into a zip archive with Deflate, in name order and with a fixed
// modification time, so that the same parts give the same bytes.
func Zip(parts map[string]string) []byte {
	names := make([]string, 0, len(parts))
	for name := range parts {
		names = append(names, name)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			panic(err)
		}
		if _, err = w.Write([]byte(parts[name])); err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
