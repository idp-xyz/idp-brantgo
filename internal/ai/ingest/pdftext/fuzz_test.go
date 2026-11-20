package pdftext

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/ingest/ingesttest"
)

// FuzzRead asserts that Read never panics (strictPanics re-panics what Read would turn into a
// refusal, in this package or in the PDF library), refuses only with *Error, is deterministic, and
// returns only what the limits allow (WBS S4-04: fuzz targets for each reader).
func FuzzRead(f *testing.F) {
	p := inquiry()
	f.Add(p.Bytes())
	p.Compress, p.ObjectStreams = true, true
	f.Add(p.Bytes())
	f.Add(ingesttest.PDF{Pages: []ingesttest.PDFPage{{Image: true}}, Encrypt: true}.Bytes())
	f.Add([]byte("%PDF-1.4\n1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		check(t, data)
	})
}

// FuzzContent puts arbitrary text into a page's content stream, so that the fuzzer reaches the
// lexer, the operators, the fonts and the line grouping.
func FuzzContent(f *testing.F) {
	for _, s := range []string{
		"BT /F1 12 Tf 72 700 Td [(Hel) -20 (lo) -800 (World)] TJ ET",
		"BT /F2 12 Tf 1 0 0 1 72 700 Tm <4E2D658700200031> Tj 14 TL T* /F3 9 Tf <0100002000310032> Tj ET",
		"q 0 1 -1 0 300 0 cm BT /F4 10 Tf 3 Tr 2 Tc 1 Tw 90 Tz 2 Ts (\\200\\201\\202 a b) ' 1 2 (x) \" ET Q",
		"BI /W 2 /H 1 /BPC 8 /CS /G ID \x00\xff EI /Im1 Do /Fm1 Do << /A [1 (b) <0c>] >> BDC EMC % comment\n",
		"BT /F1 1e9 Tf 1e300 1e300 Td (overflow) Tj ET",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		p := ingesttest.PDF{Pages: []ingesttest.PDFPage{
			{Raw: raw},
			{Texts: []ingesttest.PDFText{{Font: "F1", Size: 12, X: 72, Y: 700, Text: "page two"}}},
		}}
		check(t, p.Bytes())
	})
}

func check(t *testing.T, data []byte) {
	t.Helper()
	lim := DefaultLimits(20, 5000)
	lim.MaxStreamBytes, lim.MaxPageBytes, lim.MaxCMapBytes = 1<<20, 2<<20, 64<<10
	doc, err := Read(data, lim)
	again, err2 := Read(data, lim)
	if !reflect.DeepEqual(doc, again) || !reflect.DeepEqual(err, err2) {
		t.Fatalf("two reads differ: %v / %v", err, err2)
	}
	if err != nil {
		var e *Error
		if doc != nil || !errors.As(err, &e) {
			t.Fatalf("document %v with error %v", doc, err)
		}
		return
	}
	if len(doc.Pages) == 0 || len(doc.Pages) > lim.MaxPages {
		t.Fatalf("%d pages", len(doc.Pages))
	}
	glyphs := 0
	for _, p := range doc.Pages {
		for _, l := range p.Lines {
			if l.Text == "" || l.Text != strings.TrimSpace(l.Text) || strings.ContainsAny(l.Text, "\n\r\x00") {
				t.Fatalf("line %q", l.Text)
			}
			glyphs += len([]rune(l.Text))
		}
	}
	if glyphs > 2*lim.MaxGlyphs {
		t.Fatalf("%d characters from at most %d glyphs", glyphs, lim.MaxGlyphs)
	}
}
