package pdftext

import (
	"math"
	"sort"
	"strings"
)

// Line grouping (Version 1): glyphs are taken from the top of the page down by baseline, in
// content order where baselines are equal; a glyph whose baseline lies more than half a font size
// (the larger of the line's first glyph and its own) below the line's first baseline opens a new
// line. Within a line glyphs go left to right, in content order where they share a position; a
// glyph drawn again over the previous one (simulated bold) is read once; a gap wider than a fifth of
// the font size reads as a space, wider than two font sizes as a tab. Lines with nothing but white
// space are dropped.
func lines(glyphs []glyph) []Line {
	if len(glyphs) == 0 {
		return nil
	}
	sorted := append([]glyph(nil), glyphs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].y > sorted[j].y })
	var out []Line
	var cur []glyph
	var top, size float64
	flush := func() {
		if l, ok := build(cur); ok {
			out = append(out, l)
		}
		cur = nil
	}
	for _, g := range sorted {
		s := math.Max(g.size, 1)
		if len(cur) > 0 && top-g.y > 0.5*math.Max(size, s) {
			flush()
		}
		if len(cur) == 0 {
			top, size = g.y, s
		}
		cur = append(cur, g)
	}
	flush()
	return out
}

func build(glyphs []glyph) (Line, bool) {
	sort.SliceStable(glyphs, func(i, j int) bool { return glyphs[i].x < glyphs[j].x })
	var b strings.Builder
	var line Line
	var prev *glyph
	for i := range glyphs {
		g := &glyphs[i]
		if prev != nil {
			em := math.Max(math.Max(prev.size, g.size), 1)
			if g.text == prev.text && math.Abs(g.x-prev.x) < 0.1*em && math.Abs(g.y-prev.y) < 0.1*em {
				continue
			}
			if gap := g.x - (prev.x + prev.w); !blank(prev.text) && !blank(g.text) {
				switch {
				case gap > 2*em:
					b.WriteByte('\t')
				case gap > 0.2*em:
					b.WriteByte(' ')
				}
			}
		}
		b.WriteString(g.text)
		line.Invisible = line.Invisible || g.invisible
		line.Undecodable = line.Undecodable || g.undecodable
		prev = g
	}
	line.Text = strings.TrimSpace(b.String())
	return line, line.Text != ""
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }
