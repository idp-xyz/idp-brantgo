package pdftext

import (
	"math"

	"github.com/digitorus/pdf"
)

// matrix is a PDF transformation matrix [a b c d e f] (ISO 32000-1 8.3.4).
type matrix [6]float64

var identity = matrix{1, 0, 0, 1, 0, 0}

// mul returns m then n: m x n.
func (m matrix) mul(n matrix) matrix {
	return matrix{
		m[0]*n[0] + m[1]*n[2],
		m[0]*n[1] + m[1]*n[3],
		m[2]*n[0] + m[3]*n[2],
		m[2]*n[1] + m[3]*n[3],
		m[4]*n[0] + m[5]*n[2] + n[4],
		m[4]*n[1] + m[5]*n[3] + n[5],
	}
}

func (m matrix) apply(x, y float64) (float64, float64) {
	return m[0]*x + m[2]*y + m[4], m[1]*x + m[3]*y + m[5]
}

func translate(x, y float64) matrix { return matrix{1, 0, 0, 1, x, y} }

// glyph is one shown glyph in display orientation: origin, advance along the line, size.
type glyph struct {
	x, y, w, size float64
	text          string
	invisible     bool
	undecodable   bool
}

// gstate is the part of the graphics state the text layer needs (ISO 32000-1 8.4, 9.3).
type gstate struct {
	ctm                        matrix
	tc, tw, th, tl, tfs, trise float64
	tr                         int64
	font                       *font
}

// pageRun interprets one page.
type pageRun struct {
	d      *document
	number int
	rotate matrix // user space to display orientation
	box    [4]float64
	hasBox bool
	bytes  int64 // decompressed bytes read for the page
	glyphs []glyph
	images bool
}

func (d *document) page(number int, p pageRef) (Page, error) {
	r := &pageRun{d: d, number: number, rotate: identity}
	switch ((p.rotate % 360) + 360) % 360 {
	case 90:
		r.rotate = matrix{0, -1, 1, 0, 0, 0}
	case 180:
		r.rotate = matrix{-1, 0, 0, -1, 0, 0}
	case 270:
		r.rotate = matrix{0, 1, -1, 0, 0, 0}
	}
	for _, b := range []pdf.Value{p.cropBox, p.media} {
		if b.Len() == 4 {
			x0, y0, x1, y1 := b.Index(0).Float64(), b.Index(1).Float64(), b.Index(2).Float64(), b.Index(3).Float64()
			r.box = [4]float64{math.Min(x0, x1), math.Min(y0, y1), math.Max(x0, x1), math.Max(y0, y1)}
			r.hasBox = true
			break
		}
	}
	var content []byte
	contents := p.v.Key("Contents")
	switch contents.Kind() {
	case pdf.Stream:
		data, err := r.read(contents)
		if err != nil {
			return Page{}, err
		}
		content = data
	case pdf.Array:
		for i := 0; i < contents.Len(); i++ {
			data, err := r.read(contents.Index(i))
			if err != nil {
				return Page{}, err
			}
			content = append(append(content, data...), '\n')
		}
	case pdf.Null:
	default:
		return Page{}, r.fail(ReasonStructure)
	}
	gs := gstate{ctm: identity, th: 1}
	if err := r.interpret(content, p.resources, gs, 0); err != nil {
		return Page{}, err
	}
	page := Page{Lines: lines(r.glyphs), Images: r.images}
	readable, undecodable := false, false
	for _, g := range r.glyphs {
		readable = readable || !g.undecodable && !blank(g.text)
		undecodable = undecodable || g.undecodable
	}
	switch {
	case undecodable && !readable:
		return Page{}, r.fail(ReasonEncoding)
	case len(page.Lines) == 0 && r.images:
		return Page{}, r.fail(ReasonTextLayer)
	}
	return page, nil
}

func (r *pageRun) fail(reason Reason) *Error { return &Error{Reason: reason, Page: r.number} }

// read returns a content stream or form, within the stream and page bounds.
func (r *pageRun) read(v pdf.Value) ([]byte, error) {
	max := min(r.d.lim.MaxStreamBytes, r.d.lim.MaxPageBytes-r.bytes)
	data, err := r.d.stream(v, max)
	if err != nil {
		return nil, r.fail(ReasonStructure)
	}
	r.bytes += int64(len(data))
	return data, nil
}

const maxOperands = 64

// interpret runs a content stream with its resources from the graphics state gs.
func (r *pageRun) interpret(data []byte, res pdf.Value, gs gstate, depth int) error {
	l := &lexer{b: data}
	var ops []obj
	var stack []gstate
	tm, tlm := identity, identity
	for {
		o, ok, err := l.next()
		if err != nil {
			return r.fail(ReasonStructure)
		}
		if !ok {
			return nil
		}
		if o.k != kOp {
			if len(ops) == maxOperands {
				ops = append(ops[:0], ops[1:]...)
			}
			ops = append(ops, o)
			continue
		}
		switch o.s {
		case "q":
			if len(stack) < 1024 {
				stack = append(stack, gs)
			}
		case "Q":
			if n := len(stack); n > 0 {
				gs, stack = stack[n-1], stack[:n-1]
			}
		case "cm":
			if m, ok := matrixOf(ops); ok {
				gs.ctm = m.mul(gs.ctm)
			}
		case "BT":
			tm, tlm = identity, identity
		case "Tc", "Tw", "Tz", "TL", "Ts", "Tr":
			if v, ok := number(ops, 1, 0); ok {
				switch o.s {
				case "Tc":
					gs.tc = v
				case "Tw":
					gs.tw = v
				case "Tz":
					gs.th = v / 100
				case "TL":
					gs.tl = v
				case "Ts":
					gs.trise = v
				case "Tr":
					gs.tr = int64(v)
				}
			}
		case "Tf":
			if args := last(ops, 2); args != nil && args[0].k == kName && args[1].k == kNum {
				f, err := r.font(res, args[0].s)
				if err != nil {
					return err
				}
				gs.font, gs.tfs = f, args[1].num
			}
		case "Td", "TD":
			x, okX := number(ops, 2, 0)
			y, okY := number(ops, 2, 1)
			if okX && okY {
				if o.s == "TD" {
					gs.tl = -y
				}
				tlm = translate(x, y).mul(tlm)
				tm = tlm
			}
		case "Tm":
			if m, ok := matrixOf(ops); ok {
				tm, tlm = m, m
			}
		case "T*":
			tlm = translate(0, -gs.tl).mul(tlm)
			tm = tlm
		case "Tj", "'", "\"":
			args := last(ops, 1)
			if args == nil || args[0].k != kStr {
				break
			}
			if o.s == "\"" {
				aw, okW := number(ops, 3, 0)
				ac, okC := number(ops, 3, 1)
				if okW && okC {
					gs.tw, gs.tc = aw, ac
				}
			}
			if o.s != "Tj" {
				tlm = translate(0, -gs.tl).mul(tlm)
				tm = tlm
			}
			if err := r.show(&tm, gs, args[0].s); err != nil {
				return err
			}
		case "TJ":
			args := last(ops, 1)
			if args == nil || args[0].k != kArray {
				break
			}
			for _, x := range args[0].arr {
				switch x.k {
				case kStr:
					if err := r.show(&tm, gs, x.s); err != nil {
						return err
					}
				case kNum:
					tm = translate(-x.num/1000*gs.tfs*gs.th, 0).mul(tm)
				}
			}
		case "Do":
			if args := last(ops, 1); args != nil && args[0].k == kName {
				if err := r.xobject(res, args[0].s, gs, depth); err != nil {
					return err
				}
			}
		case "ID":
			r.images = true
			if !l.skipInlineImage() {
				return r.fail(ReasonStructure)
			}
		}
		ops = ops[:0]
	}
}

// last returns the last n operands, or nil when there are fewer.
func last(ops []obj, n int) []obj {
	if len(ops) < n {
		return nil
	}
	return ops[len(ops)-n:]
}

// number returns operand i of the last n operands as a number.
func number(ops []obj, n, i int) (float64, bool) {
	args := last(ops, n)
	if args == nil || args[i].k != kNum || math.IsNaN(args[i].num) || math.IsInf(args[i].num, 0) {
		return 0, false
	}
	return args[i].num, true
}

func matrixOf(ops []obj) (matrix, bool) {
	var m matrix
	for i := range m {
		v, ok := number(ops, 6, i)
		if !ok {
			return identity, false
		}
		m[i] = v
	}
	return m, true
}

// font returns the font a resource name selects; nil when there is none.
func (r *pageRun) font(res pdf.Value, name string) (*font, error) {
	v := res.Key("Font").Key(name)
	if v.Kind() != pdf.Dict {
		return nil, nil
	}
	f, err := r.d.font(name, v)
	if err != nil {
		return nil, r.fail(ReasonStructure)
	}
	return f, nil
}

// show places the glyphs of a string (ISO 32000-1 9.4.4): each glyph at the text rendering
// matrix's origin, then the text matrix moved by its displacement.
func (r *pageRun) show(tm *matrix, gs gstate, s string) error {
	for i := 0; i < len(s); {
		n, text, ok, w := 1, replacement, false, 0.5
		if f := gs.font; f != nil {
			n = min(f.code(s[i:]), len(s)-i)
			text, ok = f.text(s[i : i+n])
			w = f.width(s[i : i+n])
		}
		code := s[i : i+n]
		i += n
		if r.d.glyphs++; r.d.glyphs > r.d.lim.MaxGlyphs {
			return &Error{Reason: ReasonCharacters, Page: r.number, Actual: int64(r.d.glyphs), Max: int64(r.d.lim.MaxGlyphs), AtLeast: true}
		}
		m := tm.mul(gs.ctm)
		x0, y0 := m.apply(0, gs.trise)
		x1, y1 := m.apply(w*gs.tfs*gs.th, gs.trise)
		dx0, dy0 := r.rotate.apply(x0, y0)
		dx1, _ := r.rotate.apply(x1, y1)
		size := math.Abs(gs.tfs) * math.Hypot(m[2], m[3])
		if math.IsNaN(size) || math.IsInf(size, 0) || math.IsNaN(dx0+dy0+dx1) || math.IsInf(dx0+dy0+dx1, 0) {
			return r.fail(ReasonStructure)
		}
		r.glyphs = append(r.glyphs, glyph{
			x: dx0, y: dy0, w: dx1 - dx0, size: size, text: text, undecodable: !ok,
			invisible: gs.tr == 3 || gs.tr == 7 || size < minVisibleSize || r.outside(x0, y0),
		})
		tx := (w*gs.tfs + gs.tc) * gs.th
		if code == " " {
			tx += gs.tw * gs.th
		}
		*tm = translate(tx, 0).mul(*tm)
	}
	return nil
}

// minVisibleSize is the font size, in points of user space, below which text counts as invisible:
// no reader of the page sees it, while a model would.
const minVisibleSize = 2

// outside reports a point beyond the crop box (or media box) by more than a point.
func (r *pageRun) outside(x, y float64) bool {
	return r.hasBox && (x < r.box[0]-1 || x > r.box[2]+1 || y < r.box[1]-1 || y > r.box[3]+1)
}

// xobject runs a form XObject, or notes an image.
func (r *pageRun) xobject(res pdf.Value, name string, gs gstate, depth int) error {
	xo := res.Key("XObject").Key(name)
	switch xo.Key("Subtype").Name() {
	case "Image":
		r.images = true
	case "Form":
		if depth >= r.d.lim.MaxFormDepth {
			return r.fail(ReasonStructure)
		}
		data, err := r.read(xo)
		if err != nil {
			return err
		}
		formRes := xo.Key("Resources")
		if formRes.IsNull() {
			formRes = res
		}
		var m matrix
		for i := range m {
			m[i] = identity[i]
			if v := xo.Key("Matrix"); v.Len() == 6 {
				m[i] = v.Index(i).Float64()
			}
		}
		gs.ctm = m.mul(gs.ctm)
		return r.interpret(data, formRes, gs, depth+1)
	}
	return nil
}
