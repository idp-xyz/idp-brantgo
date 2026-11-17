package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The inquiry files of the fixed set: synthetic text, XLSX and PDF made here (decision E-6: synthetic
// data only; root tests/ cannot use internal/ai/ingest/ingesttest, AC6). Each is generated
// deterministically from rows, so the harness knows where every field stands in the file's extract
// (baseline 14.2 locators: text line:col, PDF page:line, XLSX Sheet!A1) — the truth the scripted
// model quotes and the scripted reviewer checks the agent's lines against.

// Input kinds of the Q01.upload.v0.1 profile.
const (
	kindText = "text"
	kindXLSX = "xlsx"
	kindPDF  = "pdf"

	mimeText = "text/plain"
	mimePDF  = "application/pdf"
	mimeXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

	sheetName = "Inquiry"
)

// inquiryRow is one requested item as the customer wrote it. An empty field is not in the file.
type inquiryRow struct {
	line                 string // the RFQ line it becomes
	code, item, qty, uom string // the customer's words: style or SKU code, description, quantity, unit
}

// fieldAt is where a field's words stand in the extract and the words themselves.
type fieldAt struct {
	Locator, Quote string
}

// inquiry is a generated inquiry file.
type inquiry struct {
	kind, name, mime string
	data             []byte
	at               map[string]map[string]fieldAt // RFQ line → field (sku_id, requested_item, quantity, uom) → place
}

// The RFQ line fields a locator can name (INQ FieldLocator.Field).
const (
	fieldSKU  = "sku_id"
	fieldItem = "requested_item"
	fieldQty  = "quantity"
	fieldUOM  = "uom"
)

var inquiryHead = []string{
	"SYNTHETIC INQUIRY - AI evaluation data, not a real customer",
	"From: customer.1 (contact.1), Hamburg",
	"Please quote FCA, delivery 2027-01-31, payment 30/70.",
	"Code | Item | Qty | Unit",
}

// makeInquiry builds the inquiry of rows (and the extra lines after them, such as an injected
// instruction, AN10) as a file of kind.
func makeInquiry(kind, name string, rows []inquiryRow, extra []string) (inquiry, error) {
	in := inquiry{kind: kind, name: name, at: map[string]map[string]fieldAt{}}
	switch kind {
	case kindText:
		in.mime, in.name = mimeText, name+".txt"
		in.data = in.text(rows, extra)
	case kindPDF:
		in.mime, in.name = mimePDF, name+".pdf"
		in.data = in.pdf(rows, extra)
	case kindXLSX:
		in.mime, in.name = mimeXLSX, name+".xlsx"
		var err error
		if in.data, err = in.xlsx(rows, extra); err != nil {
			return in, err
		}
	default:
		return in, fmt.Errorf("input kind %q", kind)
	}
	return in, nil
}

func (r inquiryRow) cells() []string { return []string{r.code, r.item, r.qty, r.uom} }

var rowFields = []string{fieldSKU, fieldItem, fieldQty, fieldUOM}

func (in *inquiry) place(line, field, locator, quote string) {
	if quote == "" {
		return
	}
	if in.at[line] == nil {
		in.at[line] = map[string]fieldAt{}
	}
	in.at[line][field] = fieldAt{Locator: locator, Quote: quote}
}

// rowText is a row as one line of text: the cells joined by " | " (empty cells stay empty), and the
// 1-based code-point column of each cell.
func rowText(r inquiryRow) (string, []int) {
	var b strings.Builder
	cols := make([]int, 4)
	for i, c := range r.cells() {
		if i > 0 {
			b.WriteString(" | ")
		}
		cols[i] = utf8.RuneCountInString(b.String()) + 1
		b.WriteString(c)
	}
	return strings.TrimRight(b.String(), " |"), cols
}

func (in *inquiry) text(rows []inquiryRow, extra []string) []byte {
	lines := append([]string(nil), inquiryHead...)
	for _, r := range rows {
		s, cols := rowText(r)
		n := len(lines) + 1
		for i, c := range r.cells() {
			in.place(r.line, rowFields[i], fmt.Sprintf("%d:%d", n, cols[i]), c)
		}
		lines = append(lines, s)
	}
	lines = append(lines, extra...)
	return []byte(strings.Join(lines, "\n") + "\n")
}

// pdf writes one A4 page with a text layer in Helvetica (WinAnsi): one text line per head line, row
// and extra line, top-down; the PDF locator of a row's field is page 1, the row's visual line.
func (in *inquiry) pdf(rows []inquiryRow, extra []string) []byte {
	lines := append([]string(nil), inquiryHead...)
	for _, r := range rows {
		s, _ := rowText(r)
		n := len(lines) + 1
		for i, c := range r.cells() {
			in.place(r.line, rowFields[i], fmt.Sprintf("1:%d", n), c)
		}
		lines = append(lines, s)
	}
	lines = append(lines, extra...)
	var content strings.Builder
	content.WriteString("BT\n/F1 10 Tf\n")
	for i, l := range lines {
		fmt.Fprintf(&content, "1 0 0 1 40 %d Tm (%s) Tj\n", 790-i*18, pdfEscape(l))
	}
	content.WriteString("ET\n")
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", content.Len(), content.String()),
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	offsets := make([]int, len(objects))
	for i, o := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return b.Bytes()
}

func pdfEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`)
	return r.Replace(s)
}

// xlsx writes a workbook of one sheet: the head lines in column A, then the header row and one row
// per item (code, item, quantity as a number, unit), then the extra lines in column A. Cell values
// only (inline strings and numbers): no formula, macro, style or link.
func (in *inquiry) xlsx(rows []inquiryRow, extra []string) ([]byte, error) {
	type cell struct {
		ref, value string
		number     bool
	}
	var sheet [][]cell
	r := 0
	addRow := func(cells ...cell) { sheet = append(sheet, cells) }
	for _, h := range inquiryHead[:3] {
		r++
		addRow(cell{ref: "A" + strconv.Itoa(r), value: h})
	}
	r++
	var header []cell
	for i, h := range []string{"Code", "Item", "Qty", "Unit"} {
		header = append(header, cell{ref: string(rune('A'+i)) + strconv.Itoa(r), value: h})
	}
	addRow(header...)
	for _, row := range rows {
		r++
		var cells []cell
		for i, c := range row.cells() {
			if c == "" {
				continue
			}
			ref := string(rune('A'+i)) + strconv.Itoa(r)
			_, err := strconv.Atoi(c)
			cells = append(cells, cell{ref: ref, value: c, number: i == 2 && err == nil})
			in.place(row.line, rowFields[i], sheetName+"!"+ref, c)
		}
		addRow(cells...)
	}
	for _, e := range extra {
		r++
		addRow(cell{ref: "A" + strconv.Itoa(r), value: e})
	}
	var ws strings.Builder
	ws.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	ws.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for i, cells := range sheet {
		fmt.Fprintf(&ws, `<row r="%d">`, i+1)
		for _, c := range cells {
			if c.number {
				fmt.Fprintf(&ws, `<c r="%s"><v>%s</v></c>`, c.ref, c.value)
			} else {
				fmt.Fprintf(&ws, `<c r="%s" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, c.ref, xmlEscape(c.value))
			}
		}
		ws.WriteString(`</row>`)
	}
	ws.WriteString(`</sheetData></worksheet>`)
	parts := []struct{ name, body string }{
		{"[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/></Types>`},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		{"xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="` + sheetName + `" sheetId="1" r:id="rId1"/></sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`},
		{"xl/worksheets/sheet1.xml", ws.String()},
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range parts {
		f, err := zw.Create(p.name)
		if err != nil {
			return nil, err
		}
		if _, err = f.Write([]byte(p.body)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// brcRows are BRC-001's four requested items as the customer writes them.
func brcRows() []inquiryRow {
	var out []inquiryRow
	for _, l := range brcLines {
		out = append(out, inquiryRow{line: l.id, code: l.sku, item: l.item, qty: l.qty, uom: "PCS"})
	}
	return out
}
