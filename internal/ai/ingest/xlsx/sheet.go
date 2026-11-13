package xlsx

import (
	"encoding/xml"
	"errors"
	"io"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Excel's grid.
const (
	maxColumn = 16384
	maxRow    = 1048576
)

// worksheet reads the cells holding values of one worksheet part into sheet, with hidden rows and
// columns, refusing the sheet as soon as it has more rows or columns holding values than the limits,
// or the workbook more characters.
func (r *reader) worksheet(name string, sheet *Sheet) error {
	dec, closeFn, err := r.open(name)
	if err != nil {
		return err
	}
	defer closeFn()
	type span struct{ min, max int }
	var hiddenCols []span
	hiddenRows := map[int]bool{}
	rows, cols := map[int]bool{}, map[int]bool{}
	taken := map[int64]bool{}
	row, col := 0, 0
	inData, inCols := false, false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return partError(name, err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			switch t.Name.Local {
			case "sheetData":
				inData = false
			case "cols":
				inCols = false
			}
		case xml.StartElement:
			switch {
			case t.Name.Local == "sheetData":
				inData = true
			case t.Name.Local == "cols":
				inCols = true
			case t.Name.Local == "col" && inCols:
				lo, okLo := index(t, "min", maxColumn)
				hi, okHi := index(t, "max", maxColumn)
				if !okLo || !okHi || lo > hi {
					return &Error{Reason: ReasonStructure, Part: name}
				}
				if v, _ := attr(t, "hidden"); truthy(v) {
					hiddenCols = append(hiddenCols, span{lo, hi})
				}
			case t.Name.Local == "row" && inData:
				if _, ok := attr(t, "r"); ok {
					n, ok := index(t, "r", maxRow)
					if !ok {
						return &Error{Reason: ReasonStructure, Part: name}
					}
					row = n
				} else if row++; row > maxRow {
					return &Error{Reason: ReasonStructure, Part: name}
				}
				col = 0
				if v, _ := attr(t, "hidden"); truthy(v) {
					hiddenRows[row] = true
				}
			case t.Name.Local == "c" && inData:
				if ref, ok := attr(t, "r"); ok {
					rr, cc, ok := cellRef(ref)
					if !ok || (row > 0 && rr != row) {
						return &Error{Reason: ReasonStructure, Part: name, Sheet: sheet.Name}
					}
					row, col = rr, cc
				} else if col++; row == 0 || col > maxColumn {
					return &Error{Reason: ReasonStructure, Part: name, Sheet: sheet.Name}
				}
				c, err := r.cell(dec, t)
				if err != nil {
					var e *Error
					if errors.As(err, &e) {
						e.Sheet, e.Row, e.Col = sheet.Name, row, col
						return e
					}
					return partError(name, err)
				}
				if c.Value == "" {
					continue
				}
				key := int64(row)*maxColumn + int64(col)
				if taken[key] {
					return &Error{Reason: ReasonStructure, Sheet: sheet.Name, Row: row, Col: col}
				}
				taken[key] = true
				for _, ch := range c.Value {
					if control(ch) {
						return &Error{Reason: ReasonEncoding, Sheet: sheet.Name, Row: row, Col: col}
					}
				}
				c.Row, c.Col = row, col
				rows[row], cols[col] = true, true
				if len(rows) > r.lim.MaxRows {
					return &Error{Reason: ReasonRows, Sheet: sheet.Name, Actual: int64(len(rows)), Max: int64(r.lim.MaxRows), AtLeast: true}
				}
				if len(cols) > r.lim.MaxColumns {
					return &Error{Reason: ReasonColumns, Sheet: sheet.Name, Actual: int64(len(cols)), Max: int64(r.lim.MaxColumns), AtLeast: true}
				}
				r.chars += int64(utf8.RuneCountInString(c.Value)) + 1
				if r.chars-1 > int64(r.lim.MaxCharacters) {
					return &Error{Reason: ReasonCharacters, Actual: r.chars - 1, Max: int64(r.lim.MaxCharacters), AtLeast: true}
				}
				sheet.Cells = append(sheet.Cells, c)
			}
		}
	}
	sort.Slice(sheet.Cells, func(i, j int) bool {
		a, b := sheet.Cells[i], sheet.Cells[j]
		return a.Row < b.Row || (a.Row == b.Row && a.Col < b.Col)
	})
	for _, n := range sortedKeys(rows) {
		if hiddenRows[n] {
			sheet.HiddenRows = append(sheet.HiddenRows, n)
		}
	}
	for _, n := range sortedKeys(cols) {
		for _, s := range hiddenCols {
			if n >= s.min && n <= s.max {
				sheet.HiddenColumns = append(sheet.HiddenColumns, n)
				break
			}
		}
	}
	return nil
}

func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// index reads a 1-based integer attribute up to max.
func index(start xml.StartElement, name string, max int) (int, bool) {
	v, _ := attr(start, name)
	n, err := strconv.Atoi(v)
	return n, err == nil && n >= 1 && n <= max && v[0] != '+'
}

// cellRef reads an A1 reference without $.
func cellRef(ref string) (row, col int, ok bool) {
	i := 0
	for i < len(ref) && i < 3 && ref[i] >= 'A' && ref[i] <= 'Z' {
		col = col*26 + int(ref[i]-'A'+1)
		i++
	}
	digits := ref[i:]
	if i == 0 || digits == "" || digits[0] == '0' || len(digits) > 7 {
		return 0, 0, false
	}
	for j := 0; j < len(digits); j++ {
		if digits[j] < '0' || digits[j] > '9' {
			return 0, 0, false
		}
		row = row*10 + int(digits[j]-'0')
	}
	return row, col, row <= maxRow && col <= maxColumn
}

// cell reads a <c> element up to its end and returns its value (empty for no value).
func (r *reader) cell(dec *xml.Decoder, start xml.StartElement) (Cell, error) {
	typ, _ := attr(start, "t")
	var c Cell
	var v strings.Builder
	var inline string
	hasV, inV := false, false
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return Cell{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case depth == 1 && t.Name.Local == "v":
				hasV, inV = true, true
				depth++
			case depth == 1 && t.Name.Local == "f":
				c.Formula = true
				if err := dec.Skip(); err != nil {
					return Cell{}, err
				}
			case depth == 1 && t.Name.Local == "is":
				s, err := richText(dec)
				if err != nil {
					return Cell{}, err
				}
				inline = s
			default:
				if err := dec.Skip(); err != nil {
					return Cell{}, err
				}
			}
		case xml.EndElement:
			depth--
			inV = false
		case xml.CharData:
			if inV {
				v.Write(t)
			}
		}
	}
	raw := v.String()
	if c.Formula && !hasV && typ != "inlineStr" {
		return Cell{}, &Error{Reason: ReasonFormula}
	}
	switch typ {
	case "s":
		if !hasV {
			return c, nil
		}
		i, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || i < 0 || i >= len(r.shared) {
			return Cell{}, &Error{Reason: ReasonStructure}
		}
		c.Value = r.shared[i]
	case "inlineStr":
		c.Value = inline
	case "str":
		c.Value = unescape(raw)
	case "b":
		switch strings.TrimSpace(raw) {
		case "1", "true":
			c.Value = "TRUE"
		case "0", "false":
			c.Value = "FALSE"
		case "":
		default:
			return Cell{}, &Error{Reason: ReasonStructure}
		}
	case "e":
		c.Value, c.Error = strings.TrimSpace(raw), true
	case "d":
		c.Value = strings.TrimSpace(raw)
	case "", "n":
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return c, nil
		}
		n, ok := number(raw)
		if !ok {
			return Cell{}, &Error{Reason: ReasonStructure}
		}
		c.Value = n
		if s, ok := attr(start, "s"); ok {
			if xf, err := strconv.Atoi(s); err == nil {
				if kind := r.dates[xf]; kind != notDate {
					if d, ok := date(n, kind, r.date1904); ok {
						c.Value, c.Date = d, true
					}
				}
			}
		}
	default:
		return Cell{}, &Error{Reason: ReasonStructure}
	}
	return c, nil
}

// number reads a number as Excel stores it (xsd:double lexical form) and returns it with at most 15
// significant digits, rounded half up, in plain notation when its decimal exponent lies between -15
// and 20 and in scientific notation otherwise. It never goes through a float.
func number(v string) (string, bool) {
	neg := false
	switch {
	case strings.HasPrefix(v, "-"):
		neg, v = true, v[1:]
	case strings.HasPrefix(v, "+"):
		v = v[1:]
	}
	mant, exp, hasE := v, 0, false
	if i := strings.IndexAny(v, "eE"); i >= 0 {
		e, err := strconv.Atoi(v[i+1:])
		if err != nil || e < -400 || e > 400 {
			return "", false
		}
		mant, exp, hasE = v[:i], e, true
	}
	whole, frac, _ := strings.Cut(mant, ".")
	if whole == "" && frac == "" || !digitsOnly(whole) || !digitsOnly(frac) || hasE && mant == "" {
		return "", false
	}
	// value = 0.<digits> x 10^point
	digits := strings.TrimLeft(whole+frac, "0")
	point := len(whole) + exp - (len(whole+frac) - len(digits))
	if digits == "" {
		return "0", true
	}
	if len(digits) > 15 {
		up := digits[15] >= '5'
		b := []byte(digits[:15])
		for i := 14; up && i >= 0; i-- {
			if b[i] == '9' {
				b[i] = '0'
				continue
			}
			b[i]++
			up = false
		}
		digits = string(b)
		if up {
			digits = "1" + digits[:14]
			point++
		}
	}
	digits = strings.TrimRight(digits, "0")
	var out string
	switch {
	case point > 20 || point < -14:
		out = digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		out += "E" + strconv.Itoa(point-1)
	case point <= 0:
		out = "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		out = digits + strings.Repeat("0", point-len(digits))
	default:
		out = digits[:point] + "." + digits[point:]
	}
	if neg {
		out = "-" + out
	}
	return out, true
}

func digitsOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

type dateKind int

const (
	notDate dateKind = iota
	dateOnly
	dateTime
	timeOnly
)

// builtinDates are the built-in number formats that show dates or times (ECMA-376 18.8.30, with
// the East Asian built-in formats 27-36 and 50-58 Excel uses in zh-CN and other CJK locales). 46
// ([h]:mm:ss) is an elapsed time, not a time of day.
var builtinDates = map[int]dateKind{
	14: dateOnly, 15: dateOnly, 16: dateOnly, 17: dateOnly, 18: timeOnly, 19: timeOnly, 20: timeOnly,
	21: timeOnly, 22: dateTime, 45: timeOnly, 47: timeOnly,
	27: dateOnly, 28: dateOnly, 29: dateOnly, 30: dateOnly, 31: dateOnly, 32: timeOnly, 33: timeOnly,
	34: timeOnly, 35: timeOnly, 36: dateOnly, 50: dateOnly, 51: dateOnly, 52: dateOnly, 53: dateOnly,
	54: dateOnly, 55: timeOnly, 56: timeOnly, 57: dateOnly, 58: dateOnly,
}

// styles reads which cell formats (cellXfs, by index) show dates or times.
func (r *reader) styles(name string) (map[int]dateKind, error) {
	dec, closeFn, err := r.open(name)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	custom := map[int]string{}
	var xfs []int
	inXfs := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, partError(name, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "numFmt":
				id, err := strconv.Atoi(attrOf(t, "numFmtId"))
				if err != nil {
					return nil, &Error{Reason: ReasonStructure, Part: name}
				}
				custom[id] = attrOf(t, "formatCode")
			case "cellXfs":
				inXfs = true
			case "xf":
				if inXfs {
					id, _ := strconv.Atoi(attrOf(t, "numFmtId"))
					xfs = append(xfs, id)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "cellXfs" {
				inXfs = false
			}
		}
	}
	out := map[int]dateKind{}
	for i, id := range xfs {
		kind := builtinDates[id]
		if code, ok := custom[id]; ok {
			kind = classify(code)
		}
		if kind != notDate {
			out[i] = kind
		}
	}
	return out, nil
}

func attrOf(start xml.StartElement, name string) string {
	v, _ := attr(start, name)
	return v
}

// classify reads a custom number format code: the first section, without quoted text, escaped
// characters, AM/PM markers and bracketed locale or colour codes; y, d, or m without h or s make a
// date (e is the year of an era, as in [$-404]e/m/d), h or s a time; an elapsed time ([h], [m], [s]),
// General and scientific notation are not.
func classify(code string) dateKind {
	section, _, _ := strings.Cut(code, ";")
	var b strings.Builder
	for i := 0; i < len(section); i++ {
		switch c := section[i]; c {
		case '"':
			j := strings.IndexByte(section[i+1:], '"')
			if j < 0 {
				i = len(section)
			} else {
				i += j + 1
			}
		case '\\', '_', '*':
			i++
		case '[':
			j := strings.IndexByte(section[i:], ']')
			if j < 0 {
				return notDate
			}
			inner := strings.ToLower(section[i+1 : i+j])
			if inner != "" && strings.Trim(inner, "hms") == "" {
				return notDate // elapsed time
			}
			i += j
		default:
			b.WriteByte(c)
		}
	}
	s := strings.ToLower(b.String())
	if strings.Contains(s, "general") || strings.Contains(s, "e+") || strings.Contains(s, "e-") {
		return notDate // General, or scientific notation
	}
	s = strings.ReplaceAll(s, "am/pm", "")
	s = strings.ReplaceAll(s, "a/p", "")
	hasDate := strings.ContainsAny(s, "yde")
	hasTime := strings.ContainsAny(s, "hs")
	hasM := strings.Contains(s, "m")
	switch {
	case (hasDate || hasM && !hasTime) && hasTime:
		return dateTime
	case hasDate || hasM && !hasTime:
		return dateOnly
	case hasTime:
		return timeOnly
	}
	return notDate
}

// date renders a serial number in a date or time format as ISO 8601: 2026-11-13,
// 2026-11-13T08:30:00 or 08:30:00. In the 1900 date system serial 60 is Excel's 29 February 1900,
// which did not exist, and serial 0 its 0 January 1900: they stay numbers, as do negative serials and
// years after 9999. Seconds are rounded half up from the fraction of the day.
func date(n string, kind dateKind, date1904 bool) (string, bool) {
	whole, frac, _ := strings.Cut(n, ".")
	if strings.HasPrefix(n, "-") || strings.Contains(n, "E") || len(whole) > 7 {
		return "", false
	}
	days, _ := strconv.ParseInt(whole, 10, 64)
	secs := int64(0)
	if frac != "" {
		num, _ := new(big.Int).SetString(frac, 10)
		num.Mul(num, big.NewInt(2*86400))
		den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(frac))), nil)
		num.Add(num, den) // half up: (2 x frac x 86400 + 1) / 2, over 10^len
		num.Quo(num, den.Mul(den, big.NewInt(2)))
		secs = num.Int64()
	}
	if secs >= 86400 {
		days, secs = days+1, secs-86400
	}
	clock := strconv.FormatInt(100+secs/3600, 10)[1:] + ":" + strconv.FormatInt(100+secs/60%60, 10)[1:] + ":" +
		strconv.FormatInt(100+secs%60, 10)[1:]
	if kind == timeOnly {
		return clock, true
	}
	var unix int64 // days since 1970-01-01
	switch {
	case date1904:
		unix = days - 24107
	case days >= 61:
		unix = days - 25569
	case days >= 1 && days < 60:
		unix = days - 25568
	default:
		return "", false
	}
	y, m, d := civil(unix)
	if y > 9999 {
		return "", false
	}
	day := strconv.FormatInt(y, 10) + "-" + strconv.FormatInt(100+m, 10)[1:] + "-" + strconv.FormatInt(100+d, 10)[1:]
	if kind == dateTime {
		return day + "T" + clock, true
	}
	return day, true
}

// civil converts days since 1970-01-01 to a proleptic Gregorian date (H. Hinnant's algorithm).
func civil(z int64) (y, m, d int64) {
	z += 719468
	era := z / 146097
	if z < 0 {
		era = (z - 146096) / 146097
	}
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d = doy - (153*mp+2)/5 + 1
	m = mp + 3
	if mp >= 10 {
		m = mp - 9
	}
	y = yoe + era*400
	if m <= 2 {
		y++
	}
	return y, m, d
}
