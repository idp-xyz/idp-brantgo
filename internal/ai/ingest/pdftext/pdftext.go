// Package pdftext reads the text layer of a PDF as lines per page (decision E-8: one vetted
// pure-Go library behind an adapter). It is the only package that imports the PDF library,
// github.com/digitorus/pdf (BSD-3-Clause, no dependencies), and uses it for the object layer only:
// cross-reference tables and streams, objects, object streams and stream filters, each within the
// library's own bounds. The text layer is read here: content streams and form XObjects with our
// lexer, fonts and their ToUnicode CMaps, predefined Unicode and Chinese CMaps and simple encodings,
// glyph positions, and visual lines.
//
// Everything read is bounded (pages, decompressed bytes per stream and per page, CMap bytes, form
// nesting, glyphs). Documents that need a password or forbid extracting their text, pages without a
// text layer (scans: images and no text) and text without a Unicode mapping are refused; nothing is
// cut. A document encrypted with an empty user password that allows extraction is read. Lines are deterministic for one
// version (Version) of this package on one architecture.
package pdftext

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/digitorus/pdf"
)

// Version names this reader's line grouping and decoding; a change of either makes a new version.
const Version = "1"

// Limits bound a read.
type Limits struct {
	MaxPages       int   // pages of the document
	MaxGlyphs      int   // glyphs shown in all pages
	MaxStreamBytes int64 // decompressed bytes of one content stream or form
	MaxPageBytes   int64 // decompressed bytes of one page's content streams and forms
	MaxCMapBytes   int64 // decompressed bytes of one CMap
	MaxFormDepth   int   // form XObjects within form XObjects
}

// DefaultLimits are the reader's bounds for a profile of pages and characters: four glyphs per
// character, 8 MiB per stream, 16 MiB per page, 1 MiB per CMap, forms eight deep.
func DefaultLimits(pages, characters int) Limits {
	return Limits{
		MaxPages: pages, MaxGlyphs: 4*characters + 10000, MaxStreamBytes: 8 << 20, MaxPageBytes: 16 << 20,
		MaxCMapBytes: 1 << 20, MaxFormDepth: 8,
	}
}

// Reason is the rule a document broke.
type Reason string

const (
	ReasonType       Reason = "type"       // not a PDF the library opens
	ReasonEncrypted  Reason = "encrypted"  // needs a password, or forbids extracting its text
	ReasonPages      Reason = "pages"      // more pages than MaxPages
	ReasonTextLayer  Reason = "text_layer" // a page with images and no text, or a document without text
	ReasonEncoding   Reason = "encoding"   // a page whose text has no Unicode mapping
	ReasonCharacters Reason = "characters" // more glyphs than MaxGlyphs
	ReasonStructure  Reason = "structure"  // malformed, or beyond the byte, nesting or node bounds
)

// Error is a refusal. Page is 1-based when the refusal concerns one page.
type Error struct {
	Reason      Reason
	Page        int
	Actual, Max int64
	AtLeast     bool
}

func (e *Error) Error() string {
	s := "pdftext: " + string(e.Reason)
	if e.Page > 0 {
		s += fmt.Sprintf(" on page %d", e.Page)
	}
	if e.Max > 0 {
		s += fmt.Sprintf(" (%d > %d)", e.Actual, e.Max)
	}
	return s
}

// Document is the text layer of a PDF.
type Document struct {
	Pages []Page
}

// Page is one page: its lines from top to bottom, as displayed (the page's /Rotate applied).
type Page struct {
	Lines  []Line
	Images bool // the page shows images (XObjects or inline); text in them is not read
}

// Line is a visual line: glyphs on one baseline, left to right, with a space where the gap between
// two glyphs is wider than a fifth of the font size and a tab where it is wider than two font sizes.
type Line struct {
	Text        string
	Invisible   bool // holds glyphs drawn invisibly (rendering mode 3 or 7), below 2 points or outside the page
	Undecodable bool // holds glyphs without a Unicode mapping (U+FFFD)
}

// strictPanics makes Read re-panic instead of refusing; tests and fuzz targets set it so that a
// panic in this package or the library is seen, not swallowed.
var strictPanics = false

// Read reads the text layer of the PDF in data within the limits.
func Read(data []byte, lim Limits) (doc *Document, err error) {
	defer func() {
		if p := recover(); p != nil {
			if strictPanics {
				panic(p)
			}
			doc, err = nil, &Error{Reason: ReasonStructure}
		}
	}()
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return nil, &Error{Reason: ReasonType}
	}
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	switch {
	case errors.Is(err, pdf.ErrInvalidPassword):
		return nil, &Error{Reason: ReasonEncrypted}
	case err != nil:
		return nil, &Error{Reason: ReasonType}
	}
	if enc := r.Trailer().Key("Encrypt"); !enc.IsNull() || r.IsEncrypted() {
		// Opened with the empty user password: read only when the document lets its text be
		// extracted (ISO 32000-1 Table 22, bit 5), as pdftotext does.
		if p := enc.Key("P"); !r.IsEncrypted() || p.Kind() != pdf.Integer || p.Int64()&16 == 0 {
			return nil, &Error{Reason: ReasonEncrypted}
		}
	}
	d := &document{lim: lim, fonts: map[fontKey]*font{}}
	pages, err := d.pageTree(r.Trailer().Key("Root").Key("Pages"))
	if err != nil {
		return nil, err
	}
	doc = &Document{}
	text := false
	for i, p := range pages {
		page, err := d.page(i+1, p)
		if err != nil {
			return nil, err
		}
		text = text || len(page.Lines) > 0
		doc.Pages = append(doc.Pages, page)
	}
	if !text {
		return nil, &Error{Reason: ReasonTextLayer}
	}
	return doc, nil
}

type document struct {
	lim    Limits
	fonts  map[fontKey]*font
	glyphs int
	nodes  int
}

// pageRef is a leaf of the page tree with its inherited attributes.
type pageRef struct {
	v                         pdf.Value
	resources, media, cropBox pdf.Value
	rotate                    int64
}

const (
	maxTreeDepth = 32
	maxTreeNodes = 1024
)

// pageTree walks the page tree from the root, carrying the inheritable attributes down, within a
// depth and node bound (a cycle in /Kids ends at a bound).
func (d *document) pageTree(root pdf.Value) ([]pageRef, error) {
	if root.Kind() != pdf.Dict {
		return nil, &Error{Reason: ReasonStructure}
	}
	if n := root.Key("Count").Int64(); n > int64(d.lim.MaxPages) {
		return nil, &Error{Reason: ReasonPages, Actual: n, Max: int64(d.lim.MaxPages)}
	}
	var pages []pageRef
	var walk func(node pdf.Value, inh pageRef, depth int) error
	walk = func(node pdf.Value, inh pageRef, depth int) error {
		if d.nodes++; depth > maxTreeDepth || d.nodes > maxTreeNodes || node.Kind() != pdf.Dict {
			return &Error{Reason: ReasonStructure}
		}
		if v := node.Key("Resources"); !v.IsNull() {
			inh.resources = v
		}
		if v := node.Key("MediaBox"); !v.IsNull() {
			inh.media = v
		}
		if v := node.Key("CropBox"); !v.IsNull() {
			inh.cropBox = v
		}
		if v := node.Key("Rotate"); v.Kind() == pdf.Integer {
			inh.rotate = v.Int64()
		}
		kids := node.Key("Kids")
		typ := node.Key("Type").Name()
		if typ == "Pages" || (typ == "" && kids.Kind() == pdf.Array) {
			for i := 0; i < kids.Len(); i++ {
				if err := walk(kids.Index(i), inh, depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		inh.v = node
		pages = append(pages, inh)
		if len(pages) > d.lim.MaxPages {
			return &Error{Reason: ReasonPages, Actual: int64(len(pages)), Max: int64(d.lim.MaxPages), AtLeast: true}
		}
		return nil
	}
	if err := walk(root, pageRef{}, 0); err != nil {
		return nil, err
	}
	return pages, nil
}

// stream returns the decompressed bytes of a stream, refusing more than max bytes or a stream the
// library cannot decode (an unsupported filter, a damaged stream).
func (d *document) stream(v pdf.Value, max int64) ([]byte, error) {
	if v.Kind() != pdf.Stream {
		return nil, &Error{Reason: ReasonStructure}
	}
	rc := v.Reader()
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil || int64(len(data)) > max {
		return nil, &Error{Reason: ReasonStructure}
	}
	return data, nil
}

// fontKey identifies a font dictionary: the number of the object that holds it (the font's own for
// an indirect font, the container's for a direct one) and its resource name.
type fontKey struct {
	id   uint32
	name string
}

// font returns the font of a font dictionary, read once.
func (d *document) font(name string, v pdf.Value) (*font, error) {
	key := fontKey{v.GetPtr().GetID(), name}
	if f, ok := d.fonts[key]; ok {
		return f, nil
	}
	f, err := d.loadFont(v)
	if err != nil {
		return nil, err
	}
	d.fonts[key] = f
	return f, nil
}
