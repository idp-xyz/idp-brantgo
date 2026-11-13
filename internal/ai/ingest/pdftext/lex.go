package pdftext

import (
	"bytes"
	"errors"
	"strconv"
)

// The lexer reads content streams and CMaps (ISO 32000-1 7.2, 7.3, 8.9.7, 9.10.3) from bytes the
// adapter has already decompressed within its bounds. It is ours rather than the library's so that
// nesting, inline image data and malformed input are handled within known limits.

type kind uint8

const (
	kNull kind = iota
	kBool
	kNum
	kStr  // string bytes
	kName // name without the slash
	kArray
	kDict
	kOp // operator or other keyword
)

type obj struct {
	k    kind
	num  float64
	s    string
	b    bool
	arr  []obj
	dict map[string]obj
}

const maxNesting = 32

var errSyntax = errors.New("pdftext: content syntax")

type lexer struct {
	b   []byte
	pos int
}

func isWhite(c byte) bool {
	return c == 0 || c == '\t' || c == '\n' || c == '\f' || c == '\r' || c == ' '
}

func isDelim(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

// skip passes white space and comments.
func (l *lexer) skip() {
	for l.pos < len(l.b) {
		c := l.b[l.pos]
		switch {
		case isWhite(c):
			l.pos++
		case c == '%':
			for l.pos < len(l.b) && l.b[l.pos] != '\n' && l.b[l.pos] != '\r' {
				l.pos++
			}
		default:
			return
		}
	}
}

// next returns the next object or operator; ok is false at the end of the input. Stray closing
// delimiters and braces at the top level are passed over, as viewers do.
func (l *lexer) next() (o obj, ok bool, err error) {
	for {
		l.skip()
		if l.pos >= len(l.b) {
			return obj{}, false, nil
		}
		switch l.b[l.pos] {
		case ')', '>', ']', '{', '}':
			l.pos++
			continue
		}
		o, err = l.object(0)
		return o, err == nil, err
	}
}

func (l *lexer) object(depth int) (obj, error) {
	if depth > maxNesting {
		return obj{}, errSyntax
	}
	l.skip()
	if l.pos >= len(l.b) {
		return obj{}, errSyntax
	}
	switch c := l.b[l.pos]; c {
	case '(':
		return l.literal()
	case '<':
		if l.pos+1 < len(l.b) && l.b[l.pos+1] == '<' {
			l.pos += 2
			return l.dictionary(depth + 1)
		}
		return l.hex()
	case '[':
		l.pos++
		return l.array(depth + 1)
	case '/':
		l.pos++
		return obj{k: kName, s: l.name()}, nil
	case ')', '>', ']', '{', '}':
		l.pos++
		return obj{}, errSyntax
	}
	return l.regular(), nil
}

func (l *lexer) array(depth int) (obj, error) {
	a := obj{k: kArray}
	for {
		l.skip()
		if l.pos >= len(l.b) {
			return obj{}, errSyntax
		}
		if l.b[l.pos] == ']' {
			l.pos++
			return a, nil
		}
		x, err := l.object(depth)
		if err != nil {
			return obj{}, err
		}
		a.arr = append(a.arr, x)
	}
}

func (l *lexer) dictionary(depth int) (obj, error) {
	d := obj{k: kDict, dict: map[string]obj{}}
	for {
		l.skip()
		if l.pos+1 < len(l.b) && l.b[l.pos] == '>' && l.b[l.pos+1] == '>' {
			l.pos += 2
			return d, nil
		}
		if l.pos >= len(l.b) || l.b[l.pos] != '/' {
			return obj{}, errSyntax
		}
		l.pos++
		key := l.name()
		v, err := l.object(depth)
		if err != nil {
			return obj{}, err
		}
		d.dict[key] = v
	}
}

func (l *lexer) name() string {
	var b []byte
	for l.pos < len(l.b) {
		c := l.b[l.pos]
		if isWhite(c) || isDelim(c) {
			break
		}
		if c == '#' && l.pos+2 < len(l.b) {
			if v, err := strconv.ParseUint(string(l.b[l.pos+1:l.pos+3]), 16, 8); err == nil {
				b = append(b, byte(v))
				l.pos += 3
				continue
			}
		}
		b = append(b, c)
		l.pos++
	}
	return string(b)
}

func (l *lexer) literal() (obj, error) {
	l.pos++ // (
	var b []byte
	depth := 1
	for l.pos < len(l.b) {
		c := l.b[l.pos]
		l.pos++
		switch c {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return obj{k: kStr, s: string(b)}, nil
			}
		case '\\':
			if l.pos >= len(l.b) {
				return obj{}, errSyntax
			}
			e := l.b[l.pos]
			l.pos++
			switch e {
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 't':
				c = '\t'
			case 'b':
				c = '\b'
			case 'f':
				c = '\f'
			case '\r':
				if l.pos < len(l.b) && l.b[l.pos] == '\n' {
					l.pos++
				}
				continue
			case '\n':
				continue
			case '0', '1', '2', '3', '4', '5', '6', '7':
				v := int(e - '0')
				for i := 0; i < 2 && l.pos < len(l.b) && l.b[l.pos] >= '0' && l.b[l.pos] <= '7'; i++ {
					v = v*8 + int(l.b[l.pos]-'0')
					l.pos++
				}
				c = byte(v)
			default:
				c = e // \( \) \\ and any other: the character itself
			}
		case '\r':
			// an end of line in a string is read as LF
			if l.pos < len(l.b) && l.b[l.pos] == '\n' {
				l.pos++
			}
			c = '\n'
		}
		b = append(b, c)
	}
	return obj{}, errSyntax
}

func (l *lexer) hex() (obj, error) {
	l.pos++ // <
	var b []byte
	var hi byte
	half := false
	for l.pos < len(l.b) {
		c := l.b[l.pos]
		l.pos++
		if c == '>' {
			if half {
				b = append(b, hi<<4)
			}
			return obj{k: kStr, s: string(b)}, nil
		}
		if isWhite(c) {
			continue
		}
		v, ok := unhex(c)
		if !ok {
			return obj{}, errSyntax
		}
		if half {
			b = append(b, hi<<4|v)
		} else {
			hi = v
		}
		half = !half
	}
	return obj{}, errSyntax
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// regular reads a number, a boolean, null or an operator.
func (l *lexer) regular() obj {
	start := l.pos
	for l.pos < len(l.b) && !isWhite(l.b[l.pos]) && !isDelim(l.b[l.pos]) {
		l.pos++
	}
	if l.pos == start {
		l.pos++ // a delimiter no object starts with
		return obj{k: kOp, s: string(l.b[start])}
	}
	tok := string(l.b[start:l.pos])
	switch tok {
	case "true":
		return obj{k: kBool, b: true}
	case "false":
		return obj{k: kBool}
	case "null":
		return obj{k: kNull}
	}
	if numeric(tok) {
		if v, err := strconv.ParseFloat(tok, 64); err == nil {
			return obj{k: kNum, num: v}
		}
	}
	return obj{k: kOp, s: tok}
}

// numeric reports the PDF number syntax: an optional sign, digits and at most one period.
func numeric(s string) bool {
	if s != "" && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	digits, dots := 0, 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] >= '0' && s[i] <= '9':
			digits++
		case s[i] == '.':
			dots++
		default:
			return false
		}
	}
	return digits > 0 && dots <= 1
}

// skipInlineImage passes the data of an inline image after its ID operator up to and including the
// EI operator: EI preceded by white space and followed by white space or the end. It reports false
// when no EI follows.
func (l *lexer) skipInlineImage() bool {
	if l.pos < len(l.b) && isWhite(l.b[l.pos]) {
		l.pos++
	}
	for i := l.pos; i+1 < len(l.b); {
		j := bytes.Index(l.b[i:], []byte("EI"))
		if j < 0 {
			return false
		}
		at := i + j
		before := at == l.pos || isWhite(l.b[at-1])
		after := at+2 == len(l.b) || isWhite(l.b[at+2])
		if before && after {
			l.pos = at + 2
			return true
		}
		i = at + 1
	}
	return false
}
