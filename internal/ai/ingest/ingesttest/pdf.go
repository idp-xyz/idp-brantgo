package ingesttest

import (
	"bytes"
	"compress/zlib"
	"crypto/md5"
	"crypto/rc4"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/text/encoding/charmap"
)

// PDF is a synthetic PDF document. Its fonts are resources of every page:
//
//   - F1: Helvetica, WinAnsiEncoding, no widths (a standard 14 font)
//   - F2: STSong-Light, UniGB-UCS2-H, no ToUnicode (Chinese text by its code, as iText writes it)
//   - F3: an Identity-H font with a ToUnicode CMap (bfrange, bfchar and an array bfrange) and widths
//   - F4: Helvetica with Differences: 128 is Euro, 129 is uni4E2D (中), 130 is g7 (no Unicode)
//
// and XObjects Im1 (a 1x1 image) and, when a page has FormTexts, Fm1 (a form drawing them).
type PDF struct {
	Pages         []PDFPage
	Compress      bool   // content, form and CMap streams with FlateDecode
	ObjectStreams bool   // dictionaries in an object stream, a cross-reference stream (PDF 1.5)
	Encrypt       bool   // RC4 40-bit (standard security handler revision 2), empty user password
	Permissions   int32  // P of the encryption dictionary; 0 is -4 (everything allowed)
	BadPassword   bool   // an encryption dictionary no empty password opens
	Version       string // header version; "1.7" when empty
}

// PDFPage is one A4 page.
type PDFPage struct {
	Rotate    int
	Texts     []PDFText
	FormTexts []PDFText // drawn through the form XObject Fm1
	Image     bool      // draws Im1
	Raw       string    // content appended as written
}

// PDFText is a run of text at a position.
type PDFText struct {
	Font      string // F1, F2, F3 or F4
	Size      float64
	X, Y      float64
	Text      string
	Invisible bool // rendering mode 3
}

type pdfObject struct {
	body   string
	stream []byte // nil for a dictionary or other object
}

// Bytes returns the document.
func (p PDF) Bytes() []byte {
	vocab := p.vocabulary()
	objs := map[int]*pdfObject{}
	put := func(id int, body string, stream []byte) { objs[id] = &pdfObject{body: body, stream: stream} }
	streamDict := func(extra string, data []byte) (string, []byte) {
		if p.Compress {
			var b bytes.Buffer
			w := zlib.NewWriter(&b)
			_, _ = w.Write(data)
			_ = w.Close()
			return "<< " + extra + " /Filter /FlateDecode /Length %d >>", b.Bytes()
		}
		return "<< " + extra + " /Length %d >>", data
	}

	put(3, `<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>`, nil)
	put(4, `<< /Type /Font /Subtype /Type0 /BaseFont /STSong-Light /Encoding /UniGB-UCS2-H /DescendantFonts [5 0 R] >>`, nil)
	put(5, `<< /Type /Font /Subtype /CIDFontType0 /BaseFont /STSong-Light /CIDSystemInfo << /Registry (Adobe) /Ordering (GB1) /Supplement 4 >> /FontDescriptor << /Type /FontDescriptor /FontName /STSong-Light /Flags 6 /FontBBox [0 -200 1000 900] /ItalicAngle 0 /Ascent 880 /Descent -120 /CapHeight 880 /StemV 93 >> /DW 1000 >>`, nil)
	put(6, `<< /Type /Font /Subtype /Type0 /BaseFont /BrantSynthetic /Encoding /Identity-H /DescendantFonts [7 0 R] /ToUnicode 8 0 R >>`, nil)
	put(7, `<< /Type /Font /Subtype /CIDFontType2 /BaseFont /BrantSynthetic /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /DW 1000 /W [32 126 500] >>`, nil)
	d, s := streamDict("", toUnicode(vocab))
	put(8, d, s)
	put(9, `<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding << /Type /Encoding /BaseEncoding /WinAnsiEncoding /Differences [128 /Euro /uni4E2D /g7] >> >>`, nil)
	put(10, `<< /Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceGray /BitsPerComponent 8 /Length %d >>`, []byte{0x80})
	fonts := `/Font << /F1 3 0 R /F2 4 0 R /F3 6 0 R /F4 9 0 R >>`
	next := 11
	var kids []string
	for _, pg := range p.Pages {
		page, content := next, next+1
		next += 2
		xobjects := `/Im1 10 0 R`
		if len(pg.FormTexts) > 0 {
			form := next
			next++
			d, s := streamDict(`/Type /XObject /Subtype /Form /BBox [0 0 595 842] /Resources << `+fonts+` >>`, []byte(texts(pg.FormTexts, vocab)))
			put(form, d, s)
			xobjects += fmt.Sprintf(` /Fm1 %d 0 R`, form)
		}
		var c strings.Builder
		c.WriteString(texts(pg.Texts, vocab))
		if len(pg.FormTexts) > 0 {
			c.WriteString("q /Fm1 Do Q\n")
		}
		if pg.Image {
			c.WriteString("q 200 0 0 200 100 300 cm /Im1 Do Q\n")
		}
		c.WriteString(pg.Raw)
		d, s := streamDict("", []byte(c.String()))
		put(content, d, s)
		rotate := ""
		if pg.Rotate != 0 {
			rotate = fmt.Sprintf(" /Rotate %d", pg.Rotate)
		}
		put(page, fmt.Sprintf(`<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842]%s /Resources << %s /XObject << %s >> >> /Contents %d 0 R >>`,
			rotate, fonts, xobjects, content), nil)
		kids = append(kids, fmt.Sprintf("%d 0 R", page))
	}
	put(1, `<< /Type /Catalog /Pages 2 0 R >>`, nil)
	put(2, fmt.Sprintf(`<< /Type /Pages /Kids [%s] /Count %d >>`, strings.Join(kids, " "), len(kids)), nil)
	return p.write(objs, next)
}

// vocabulary returns the non-ASCII characters shown in F3, sorted: F3 shows them with codes from
// 0x0100 on.
func (p PDF) vocabulary() []rune {
	seen := map[rune]bool{}
	for _, pg := range p.Pages {
		for _, t := range append(append([]PDFText(nil), pg.Texts...), pg.FormTexts...) {
			if t.Font == "F3" {
				for _, r := range t.Text {
					if r > 0x7e {
						seen[r] = true
					}
				}
			}
		}
	}
	out := make([]rune, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// toUnicode maps F3's codes: ASCII by a bfrange, the first half of the vocabulary by bfchar, the
// rest by an array bfrange.
func toUnicode(vocab []rune) []byte {
	var b strings.Builder
	b.WriteString("/CIDInit /ProcSet findresource begin 12 dict begin begincmap\n/CMapName /Brant-UCS def /CMapType 2 def\n")
	b.WriteString("1 begincodespacerange <0000> <FFFF> endcodespacerange\n")
	b.WriteString("1 beginbfrange <0020> <007E> <0020> endbfrange\n")
	half := len(vocab) / 2
	if half > 0 {
		fmt.Fprintf(&b, "%d beginbfchar\n", half)
		for i, r := range vocab[:half] {
			fmt.Fprintf(&b, "<%04X> <%s>\n", 0x100+i, utf16Hex(r))
		}
		b.WriteString("endbfchar\n")
	}
	if rest := vocab[half:]; len(rest) > 0 {
		fmt.Fprintf(&b, "1 beginbfrange <%04X> <%04X> [", 0x100+half, 0x100+len(vocab)-1)
		for _, r := range rest {
			b.WriteString("<" + utf16Hex(r) + ">")
		}
		b.WriteString("] endbfrange\n")
	}
	b.WriteString("endcmap CMapName currentdict /CMap defineresource pop end end\n")
	return []byte(b.String())
}

func utf16Hex(r rune) string {
	if r >= 0x10000 {
		r -= 0x10000
		return fmt.Sprintf("%04X%04X", 0xd800+(r>>10), 0xdc00+(r&0x3ff))
	}
	return fmt.Sprintf("%04X", r)
}

// texts writes text runs as content.
func texts(ts []PDFText, vocab []rune) string {
	var b strings.Builder
	for _, t := range ts {
		mode := 0
		if t.Invisible {
			mode = 3
		}
		fmt.Fprintf(&b, "BT /%s %g Tf %d Tr %g %g Td %s Tj ET\n", t.Font, t.Size, mode, t.X, t.Y, encode(t, vocab))
	}
	return b.String()
}

func encode(t PDFText, vocab []rune) string {
	switch t.Font {
	case "F2":
		var h strings.Builder
		for _, r := range t.Text {
			h.WriteString(utf16Hex(r))
		}
		return "<" + h.String() + ">"
	case "F3":
		var h strings.Builder
		for _, r := range t.Text {
			if r <= 0x7e {
				fmt.Fprintf(&h, "%04X", r)
				continue
			}
			i := sort.Search(len(vocab), func(i int) bool { return vocab[i] >= r })
			fmt.Fprintf(&h, "%04X", 0x100+i)
		}
		return "<" + h.String() + ">"
	case "F4":
		var raw []byte
		for _, r := range t.Text {
			switch r {
			case 0x20ac:
				raw = append(raw, 128)
			case 0x4e2d:
				raw = append(raw, 129)
			case 0xfffd:
				raw = append(raw, 130)
			default:
				raw = append(raw, byte(r))
			}
		}
		return literal(raw)
	}
	raw, err := charmap.Windows1252.NewEncoder().Bytes([]byte(t.Text))
	if err != nil {
		panic("ingesttest: not WinAnsi text: " + t.Text)
	}
	return literal(raw)
}

func literal(b []byte) string {
	var s strings.Builder
	s.WriteByte('(')
	for _, c := range b {
		switch {
		case c == '(' || c == ')' || c == '\\':
			s.WriteByte('\\')
			s.WriteByte(c)
		case c < 0x20 || c >= 0x7f:
			fmt.Fprintf(&s, "\\%03o", c)
		default:
			s.WriteByte(c)
		}
	}
	s.WriteByte(')')
	return s.String()
}

var fileID = []byte("brant-ingest-synthetic-id")

// write lays the objects out with a cross-reference table, or with an object stream and a
// cross-reference stream.
func (p PDF) write(objs map[int]*pdfObject, size int) []byte {
	version := p.Version
	if version == "" {
		version = "1.7"
	}
	var out bytes.Buffer
	out.WriteString("%PDF-" + version + "\n%\xe2\xe3\xcf\xd3\n")
	offsets := map[int]int{}
	var key []byte
	encryptID := 0
	if p.Encrypt || p.BadPassword {
		encryptID = size
		size++
		perms := p.Permissions
		if perms == 0 {
			perms = -4
		}
		var o, u []byte
		key, o, u = rc4Keys(perms)
		if p.BadPassword {
			u = bytes.Repeat([]byte{7}, 32)
		}
		objs[encryptID] = &pdfObject{body: fmt.Sprintf("<< /Filter /Standard /V 1 /R 2 /Length 40 /P %d /O <%x> /U <%x> >>", perms, o, u)}
	}
	emit := func(id int, o *pdfObject) {
		offsets[id] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", id)
		if o.stream == nil {
			out.WriteString(o.body + "\nendobj\n")
			return
		}
		data := o.stream
		if key != nil && id != encryptID {
			data = rc4Object(key, id, data)
		}
		fmt.Fprintf(&out, o.body+"\nstream\n", len(data))
		out.Write(data)
		out.WriteString("\nendstream\nendobj\n")
	}
	ids := make([]int, 0, len(objs))
	for id := range objs {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	trailer := fmt.Sprintf("/Root 1 0 R /ID [<%x> <%x>]", fileID, fileID)
	if encryptID != 0 {
		trailer += fmt.Sprintf(" /Encrypt %d 0 R", encryptID)
	}
	if !p.ObjectStreams {
		for _, id := range ids {
			emit(id, objs[id])
		}
		xref := out.Len()
		fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", size)
		for id := 1; id < size; id++ {
			if off, ok := offsets[id]; ok {
				fmt.Fprintf(&out, "%010d 00000 n \n", off)
			} else {
				out.WriteString("0000000000 65535 f \n")
			}
		}
		fmt.Fprintf(&out, "trailer\n<< /Size %d %s >>\nstartxref\n%d\n%%%%EOF\n", size, trailer, xref)
		return out.Bytes()
	}
	// Dictionaries go into object stream `size`, streams stay top level; the cross-reference stream
	// is object size+1.
	objStm, xrefID := size, size+1
	var header, body strings.Builder
	index := map[int]int{}
	n := 0
	for _, id := range ids {
		if o := objs[id]; o.stream == nil {
			fmt.Fprintf(&header, "%d %d ", id, body.Len())
			body.WriteString(o.body + "\n")
			index[id] = n
			n++
		} else {
			emit(id, o)
		}
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	_, _ = zw.Write([]byte(header.String() + body.String()))
	_ = zw.Close()
	emit(objStm, &pdfObject{body: fmt.Sprintf("<< /Type /ObjStm /N %d /First %d /Filter /FlateDecode /Length %%d >>", n, header.Len()), stream: z.Bytes()})
	var x bytes.Buffer
	entry := func(t byte, a uint32, b uint16) {
		x.WriteByte(t)
		_ = binary.Write(&x, binary.BigEndian, a)
		_ = binary.Write(&x, binary.BigEndian, b)
	}
	offsets[xrefID] = out.Len()
	for id := 0; id <= xrefID; id++ {
		switch i, inStream := index[id]; {
		case inStream:
			entry(2, uint32(objStm), uint16(i))
		case id == xrefID || offsets[id] != 0:
			entry(1, uint32(offsets[id]), 0)
		default:
			entry(0, 0, 65535)
		}
	}
	fmt.Fprintf(&out, "%d 0 obj\n<< /Type /XRef /Size %d /W [1 4 2] %s /Length %d >>\nstream\n", xrefID, xrefID+1, trailer, x.Len())
	out.Write(x.Bytes())
	fmt.Fprintf(&out, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", offsets[xrefID])
	return out.Bytes()
}

var padding = []byte{0x28, 0xBF, 0x4E, 0x5E, 0x4E, 0x75, 0x8A, 0x41, 0x64, 0x00, 0x4E, 0x56, 0xFF, 0xFA, 0x01, 0x08,
	0x2E, 0x2E, 0x00, 0xB6, 0xD0, 0x68, 0x3E, 0x80, 0x2F, 0x0C, 0xA9, 0xFE, 0x64, 0x53, 0x69, 0x7A}

// rc4Keys returns the file key, O and U of revision 2 with empty owner and user passwords and the
// permissions p (ISO 32000-1 7.6.3.3, algorithms 2 to 4).
func rc4Keys(p int32) (key, o, u []byte) {
	sum := md5.Sum(padding)
	o = rc4Bytes(sum[:5], padding)
	h := md5.New()
	h.Write(padding)
	h.Write(o)
	_ = binary.Write(h, binary.LittleEndian, p)
	h.Write(fileID)
	key = h.Sum(nil)[:5]
	return key, o, rc4Bytes(key, padding)
}

func rc4Object(key []byte, id int, data []byte) []byte {
	k := append(append([]byte(nil), key...), byte(id), byte(id>>8), byte(id>>16), 0, 0)
	sum := md5.Sum(k)
	return rc4Bytes(sum[:min(len(key)+5, 16)], data)
}

func rc4Bytes(key, data []byte) []byte {
	c, err := rc4.NewCipher(key)
	if err != nil {
		panic(err)
	}
	out := make([]byte, len(data))
	c.XORKeyStream(out, data)
	return out
}
