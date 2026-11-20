package pdftext

import (
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/digitorus/pdf"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// replacement is the text of a glyph without a Unicode mapping.
const replacement = "\ufffd"

// font decodes the codes of a string shown in one font to text and widths (ISO 32000-1 9.6-9.10).
// Text comes from the font's ToUnicode CMap first, then from a predefined Unicode or Chinese CMap
// (composite fonts) or from the font's encoding with its Differences (simple fonts). A code without
// a mapping is U+FFFD.
type font struct {
	composite bool
	space     codespace           // composite fonts: code lengths
	toUnicode *cmap               // may be nil
	cjk       func([]byte) string // composite fonts: text of a code by a predefined CMap
	simple    [256]string         // simple fonts: text by code ("" unmapped)
	widths    map[uint32]float64  // glyph widths in thousandths of text space, by code (simple) or CID (composite)
	dw        float64             // composite fonts: default width
	identity  bool                // composite fonts: CID = code
	missing   float64             // width of a code without one
	scale     float64             // glyph space to text space: 0.001, or a Type3 font's FontMatrix
}

// code returns the length of the code at the start of s.
func (f *font) code(s string) int {
	if !f.composite {
		return 1
	}
	return f.space.length(s)
}

// text returns the text of a code; ok is false for U+FFFD.
func (f *font) text(c string) (string, bool) {
	if f.toUnicode != nil {
		if t, ok := f.toUnicode.lookup(c); ok {
			return t, true
		}
	}
	var t string
	switch {
	case f.composite && f.cjk != nil:
		t = f.cjk([]byte(c))
	case !f.composite:
		t = f.simple[c[0]]
	}
	if t, ok := clean(t); ok {
		return t, true
	}
	return replacement, false
}

// width returns the horizontal displacement of a code in text space units per unit of font size.
func (f *font) width(c string) float64 {
	if f.composite {
		w := f.dw
		if f.identity {
			if x, ok := f.widths[beUint(c)]; ok {
				w = x
			}
		}
		return w * f.scale
	}
	if w, ok := f.widths[uint32(c[0])]; ok && w > 0 {
		return w * f.scale
	}
	return f.missing * f.scale
}

func beUint(s string) uint32 {
	var v uint32
	for i := 0; i < len(s) && i < 4; i++ {
		v = v<<8 | uint32(s[i])
	}
	return v
}

// loadFont reads a font dictionary. CMap streams are read within limit bytes.
func (d *document) loadFont(v pdf.Value) (*font, error) {
	f := &font{scale: 0.001, dw: 1000}
	if tu := v.Key("ToUnicode"); tu.Kind() == pdf.Stream {
		data, err := d.stream(tu, d.lim.MaxCMapBytes)
		if err != nil {
			return nil, err
		}
		f.toUnicode = parseCMap(data)
	}
	switch v.Key("Subtype").Name() {
	case "Type0":
		f.composite = true
		d.loadComposite(f, v)
	default:
		d.loadSimple(f, v)
	}
	return f, nil
}

func (d *document) loadComposite(f *font, v pdf.Value) {
	enc := v.Key("Encoding")
	name := enc.Name()
	switch {
	case enc.Kind() == pdf.Stream:
		if data, err := d.stream(enc, d.lim.MaxCMapBytes); err == nil {
			f.space = parseCMap(data).space
		}
	case name == "Identity-H" || name == "Identity-V":
		f.identity = true
	case strings.Contains(name, "UCS2") || strings.Contains(name, "UTF16"):
		f.cjk = utf16Text
	case strings.HasPrefix(name, "GBK2K"):
		f.space = gb18030Space
		f.cjk = decoder(simplifiedchinese.GB18030)
	case strings.HasPrefix(name, "GB"): // GB-EUC, GBpc-EUC, GBK-EUC, GBKp-EUC
		f.space = eucSpace
		f.cjk = decoder(simplifiedchinese.GBK)
	case strings.Contains(name, "B5"): // B5pc, ETen-B5, ETenms-B5, HKscs-B5
		f.space = big5Space
		f.cjk = decoder(traditionalchinese.Big5)
	}
	if len(f.space) == 0 {
		switch {
		case strings.Contains(name, "UTF16"):
			f.space = utf16Space
		case f.toUnicode != nil && len(f.toUnicode.space) > 0 && !f.identity:
			f.space = f.toUnicode.space
		default:
			f.space = twoBytes
		}
	}
	desc := v.Key("DescendantFonts").Index(0)
	if dw := desc.Key("DW"); dw.Kind() == pdf.Integer || dw.Kind() == pdf.Real {
		f.dw = dw.Float64()
	}
	f.widths = cidWidths(desc.Key("W"))
}

// cidWidths reads a CIDFont's W array: c [w1 w2 ...] or c_first c_last w, at most 65,536 widths.
func cidWidths(w pdf.Value) map[uint32]float64 {
	out := map[uint32]float64{}
	n := w.Len()
	for i := 0; i < n && len(out) < 1<<16; {
		first := w.Index(i)
		if i+1 >= n || first.Kind() != pdf.Integer {
			break
		}
		next := w.Index(i + 1)
		if next.Kind() == pdf.Array {
			for j := 0; j < next.Len() && len(out) < 1<<16; j++ {
				out[uint32(first.Int64())+uint32(j)] = next.Index(j).Float64()
			}
			i += 2
			continue
		}
		if i+2 >= n {
			break
		}
		lo, hi, width := first.Int64(), next.Int64(), w.Index(i+2).Float64()
		for c := lo; c <= hi && c >= 0 && len(out) < 1<<16; c++ {
			out[uint32(c)] = width
		}
		i += 3
	}
	return out
}

func (d *document) loadSimple(f *font, v pdf.Value) {
	base := v.Key("BaseFont").Name()
	if v.Key("Subtype").Name() == "Type3" {
		if m := v.Key("FontMatrix"); m.Len() == 6 && m.Index(0).Float64() != 0 {
			f.scale = m.Index(0).Float64()
		}
	}
	// The base encoding, then the differences.
	var baseRune func(byte) (rune, bool)
	enc := v.Key("Encoding")
	encName := enc.Name()
	if enc.Kind() == pdf.Dict {
		encName = enc.Key("BaseEncoding").Name()
	}
	switch encName {
	case "WinAnsiEncoding":
		baseRune = charmapRune(charmap.Windows1252)
	case "MacRomanEncoding":
		baseRune = charmapRune(charmap.Macintosh)
	case "StandardEncoding":
		baseRune = standardRune
	case "PDFDocEncoding":
		baseRune = pdfDocRune
	default:
		// No or an unknown base encoding: the font's built-in encoding, which only the font program
		// knows; StandardEncoding for Type 1 fonts and WinAnsiEncoding otherwise approximate it.
		if v.Key("Subtype").Name() == "Type1" && !strings.Contains(base, "Symbol") && !strings.Contains(base, "Dingbats") {
			baseRune = standardRune
		} else {
			baseRune = charmapRune(charmap.Windows1252)
		}
	}
	for c := 0; c < 256; c++ {
		if r, ok := baseRune(byte(c)); ok && r != utf8.RuneError {
			f.simple[c] = string(r)
		}
	}
	if enc.Kind() == pdf.Dict {
		diff := enc.Key("Differences")
		code := -1
		for i := 0; i < diff.Len() && i < 4096; i++ {
			x := diff.Index(i)
			switch x.Kind() {
			case pdf.Integer:
				code = int(x.Int64())
			case pdf.Name:
				if code >= 0 && code < 256 {
					t, _ := glyphText(x.Name())
					f.simple[code] = t
				}
				code++
			}
		}
	}
	f.widths = map[uint32]float64{}
	first := int(v.Key("FirstChar").Int64())
	ws := v.Key("Widths")
	for i := 0; i < ws.Len() && first+i < 256; i++ {
		if first+i >= 0 {
			f.widths[uint32(first+i)] = ws.Index(i).Float64()
		}
	}
	f.missing = v.Key("FontDescriptor").Key("MissingWidth").Float64()
	if f.missing <= 0 {
		// Standard 14 fonts carry no widths: an average width keeps glyph positions plausible.
		f.missing = 500
		switch {
		case strings.Contains(base, "Courier"):
			f.missing = 600
		case strings.Contains(base, "Helvetica") || strings.Contains(base, "Arial"):
			f.missing = 556
		}
		if f.scale != 0.001 {
			f.missing = 0.5 / f.scale // half the size of a Type3 glyph space unit square
		}
	}
}

func charmapRune(cm *charmap.Charmap) func(byte) (rune, bool) {
	return func(c byte) (rune, bool) {
		if c < 0x20 || c == 0x7F {
			return 0, false
		}
		r := cm.DecodeByte(c)
		return r, r != utf8.RuneError
	}
}

func decoder(e encoding.Encoding) func([]byte) string {
	return func(b []byte) string {
		out, err := e.NewDecoder().Bytes(b)
		if err != nil {
			return ""
		}
		return string(out)
	}
}

// utf16Text reads a code of a UCS-2 or UTF-16 CMap (UniGB-UCS2-H and the like) as UTF-16BE.
func utf16Text(b []byte) string {
	if len(b)%2 != 0 {
		return ""
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	return string(utf16.Decode(u))
}

// codespace holds the code space ranges of a CMap by length.
type codespace []spaceRange

type spaceRange struct{ lo, hi string }

var (
	twoBytes     = codespace{{"\x00\x00", "\xff\xff"}}
	utf16Space   = codespace{{"\x00\x00", "\xd7\xff"}, {"\xe0\x00", "\xff\xff"}, {"\xd8\x00\xdc\x00", "\xdb\xff\xdf\xff"}}
	eucSpace     = codespace{{"\x00", "\x80"}, {"\x81\x40", "\xfe\xfe"}}
	gb18030Space = codespace{{"\x00", "\x80"}, {"\x81\x30\x81\x30", "\xfe\x39\xfe\x39"}, {"\x81\x40", "\xfe\xfe"}}
	big5Space    = codespace{{"\x00", "\x80"}, {"\xa1\x40", "\xfe\xfe"}}
)

// length returns the length of the code at the start of s: the shortest range whose bytes each lie
// between its bounds; 1 when none matches (ISO 32000-1 9.7.6.3 leaves the choice to the reader).
func (cs codespace) length(s string) int {
	best := 0
	for _, r := range cs {
		n := len(r.lo)
		if n > len(s) || (best != 0 && n >= best) {
			continue
		}
		in := true
		for i := 0; i < n; i++ {
			if s[i] < r.lo[i] || s[i] > r.hi[i] {
				in = false
				break
			}
		}
		if in {
			best = n
		}
	}
	if best == 0 {
		return 1
	}
	return best
}

// cmap is a ToUnicode CMap: code space ranges and mappings from codes to UTF-16 text.
type cmap struct {
	space  codespace
	chars  map[string]string
	ranges []bfRange // sorted by length, then lo
}

type bfRange struct {
	n      int
	lo, hi uint32
	dst    []rune   // base text: its last rune increases with the code
	arr    []string // or one text per code
}

// lookup returns the text of a code.
func (m *cmap) lookup(c string) (string, bool) {
	if t, ok := m.chars[c]; ok {
		return t, t != ""
	}
	v, n := beUint(c), len(c)
	i := sort.Search(len(m.ranges), func(i int) bool {
		r := m.ranges[i]
		return r.n > n || (r.n == n && r.lo > v)
	})
	if i == 0 {
		return "", false
	}
	r := m.ranges[i-1]
	if r.n != n || v > r.hi || v < r.lo {
		return "", false
	}
	off := v - r.lo
	if r.arr != nil {
		if int(off) < len(r.arr) && r.arr[off] != "" {
			return r.arr[off], true
		}
		return "", false
	}
	if len(r.dst) == 0 {
		return "", false
	}
	t := append([]rune(nil), r.dst...)
	t[len(t)-1] += rune(off)
	return clean(string(t))
}

// clean refuses text that is no glyph's: empty, control characters or invalid code points.
func clean(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	for _, r := range s {
		if r < 0x20 && r != '\t' || r == 0x7F || (r >= 0x80 && r <= 0x9F) || r == utf8.RuneError || r > 0x10FFFF {
			return "", false
		}
	}
	return s, true
}

// unicodeText decodes a CMap destination: UTF-16BE bytes, or a glyph name.
func unicodeText(o obj) string {
	switch o.k {
	case kStr:
		return utf16Text([]byte(o.s))
	case kName:
		t, _ := glyphText(o.s)
		return t
	}
	return ""
}

// parseCMap reads the code space ranges and the bfchar and bfrange mappings of a CMap; other
// operators are passed over, and a CMap that stops halfway keeps what it read.
func parseCMap(data []byte) *cmap {
	m := &cmap{chars: map[string]string{}}
	l := &lexer{b: data}
	var operands []obj
	for {
		o, ok, err := l.next()
		if err != nil || !ok {
			break
		}
		if o.k != kOp {
			if len(operands) < 3*1024 {
				operands = append(operands, o)
			}
			continue
		}
		switch o.s {
		case "endcodespacerange":
			for i := 0; i+1 < len(operands); i += 2 {
				lo, hi := operands[i].s, operands[i+1].s
				if operands[i].k == kStr && operands[i+1].k == kStr && len(lo) == len(hi) && len(lo) >= 1 && len(lo) <= 4 {
					m.space = append(m.space, spaceRange{lo, hi})
				}
			}
		case "endbfchar":
			for i := 0; i+1 < len(operands); i += 2 {
				if src := operands[i]; src.k == kStr && len(src.s) >= 1 && len(src.s) <= 4 && len(m.chars) < 1<<17 {
					t, _ := clean(unicodeText(operands[i+1]))
					m.chars[src.s] = t
				}
			}
		case "endbfrange":
			for i := 0; i+2 < len(operands); i += 3 {
				lo, hi, dst := operands[i], operands[i+1], operands[i+2]
				if lo.k != kStr || hi.k != kStr || len(lo.s) != len(hi.s) || len(lo.s) < 1 || len(lo.s) > 4 || len(m.ranges) >= 1<<16 {
					continue
				}
				r := bfRange{n: len(lo.s), lo: beUint(lo.s), hi: beUint(hi.s)}
				if r.hi < r.lo {
					continue
				}
				switch dst.k {
				case kStr:
					r.dst = []rune(utf16Text([]byte(dst.s)))
				case kArray:
					for j := 0; j < len(dst.arr) && j < 1<<16; j++ {
						t, _ := clean(unicodeText(dst.arr[j]))
						r.arr = append(r.arr, t)
					}
				default:
					continue
				}
				m.ranges = append(m.ranges, r)
			}
		}
		operands = operands[:0]
	}
	sort.SliceStable(m.ranges, func(i, j int) bool {
		a, b := m.ranges[i], m.ranges[j]
		return a.n < b.n || (a.n == b.n && a.lo < b.lo)
	})
	return m
}
