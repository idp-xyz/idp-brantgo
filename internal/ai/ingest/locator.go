package ingest

import (
	"errors"
	"strconv"
	"strings"
	"unicode"
)

// Locators point into an extract (WBS S4-04: text line:col, PDF page:line, XLSX sheet!cell). They
// are read with the extractor version of the extract that holds them: a reader whose line grouping
// or rendering changes gets a new version (Extract.Extractor).
//
//   - text: "line:col", 1-based; col counts Unicode code points from the start of the line.
//   - PDF: "page:line", 1-based; lines are the visual lines of the page from top to bottom, as the
//     PDF reader groups them.
//   - XLSX: "Sheet!B3", with the sheet name quoted as Excel does ('Q1 2026'!B3, 'It''s'!A1) when it
//     is not a plain name.

// ErrLocator is a locator that is malformed or points at nothing in the extract.
var ErrLocator = errors.New("ingest: no such locator")

// TextLocator returns "line:col".
func TextLocator(line, col int) string { return strconv.Itoa(line) + ":" + strconv.Itoa(col) }

// PDFLocator returns "page:line".
func PDFLocator(page, line int) string { return strconv.Itoa(page) + ":" + strconv.Itoa(line) }

// ParseTextLocator reads "line:col".
func ParseTextLocator(s string) (line, col int, err error) { return pair(s) }

// ParsePDFLocator reads "page:line".
func ParsePDFLocator(s string) (page, line int, err error) { return pair(s) }

func pair(s string) (int, int, error) {
	a, b, ok := strings.Cut(s, ":")
	x, okA := positive(a)
	y, okB := positive(b)
	if !ok || !okA || !okB {
		return 0, 0, ErrLocator
	}
	return x, y, nil
}

// positive reads a decimal integer from 1 to 10^9 without sign or leading zero.
func positive(s string) (int, bool) {
	if s == "" || len(s) > 9 || s[0] == '0' {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

// Excel's grid: columns A to XFD, rows 1 to 1,048,576.
const (
	MaxColumn = 16384
	MaxRow    = 1048576
)

// ColumnName returns the letters of a 1-based column: 1 is A, 27 is AA, 16384 is XFD.
func ColumnName(col int) string {
	var b []byte
	for col > 0 {
		col--
		b = append(b, byte('A'+col%26))
		col /= 26
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// CellRef returns the A1 reference of a cell: CellRef(3, 2) is "B3".
func CellRef(row, col int) string { return ColumnName(col) + strconv.Itoa(row) }

// ParseCellRef reads an A1 reference ("B3", case-insensitive, no $) within Excel's grid.
func ParseCellRef(ref string) (row, col int, err error) {
	i := 0
	for i < len(ref) && i < 3 && isLetter(ref[i]) {
		col = col*26 + int(upper(ref[i])-'A'+1)
		i++
	}
	row, ok := positive(ref[i:])
	if i == 0 || !ok || col > MaxColumn || row > MaxRow {
		return 0, 0, ErrLocator
	}
	return row, col, nil
}

func isLetter(c byte) bool { return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') }

func upper(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - 'a' + 'A'
	}
	return c
}

// SheetPrefix returns a sheet name as it precedes "!" in a reference: plain names as they are,
// others in single quotes with quotes doubled.
func SheetPrefix(name string) string {
	if plainSheetName(name) {
		return name
	}
	return "'" + strings.ReplaceAll(name, "'", "''") + "'"
}

// plainSheetName: a letter or underscore, then letters, digits, underscores and periods, and not
// something Excel would read as a cell or R1C1 reference or a boolean.
func plainSheetName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		ok := unicode.IsLetter(r) || r == '_' || (i > 0 && (unicode.IsDigit(r) || r == '.'))
		if !ok {
			return false
		}
	}
	if _, _, err := ParseCellRef(name); err == nil {
		return false
	}
	u := strings.ToUpper(name)
	if u == "TRUE" || u == "FALSE" {
		return false
	}
	return !r1c1(u)
}

// r1c1 reports whether u (upper case) reads as an R1C1 reference: R, C, Rn, Cn, RnCn.
func r1c1(u string) bool {
	digits := func(s string) string { return strings.TrimLeft(s, "0123456789") }
	switch {
	case strings.HasPrefix(u, "R"):
		rest := digits(u[1:])
		return rest == "" || (rest[0] == 'C' && digits(rest[1:]) == "")
	case strings.HasPrefix(u, "C"):
		return digits(u[1:]) == ""
	}
	return false
}

// CellLocator returns "Sheet!B3".
func CellLocator(sheet string, row, col int) string {
	return SheetPrefix(sheet) + "!" + CellRef(row, col)
}

// ParseCellLocator reads "Sheet!B3" or "'Sheet name'!B3".
func ParseCellLocator(s string) (sheet string, row, col int, err error) {
	var ref string
	if strings.HasPrefix(s, "'") {
		var b strings.Builder
		i := 1
		for {
			j := strings.IndexByte(s[i:], '\'')
			if j < 0 {
				return "", 0, 0, ErrLocator
			}
			b.WriteString(s[i : i+j])
			i += j + 1
			if strings.HasPrefix(s[i:], "'") {
				b.WriteByte('\'')
				i++
				continue
			}
			break
		}
		if !strings.HasPrefix(s[i:], "!") {
			return "", 0, 0, ErrLocator
		}
		sheet, ref = b.String(), s[i+1:]
	} else {
		var ok bool
		if sheet, ref, ok = strings.Cut(s, "!"); !ok || !plainSheetName(sheet) {
			return "", 0, 0, ErrLocator
		}
	}
	if sheet == "" {
		return "", 0, 0, ErrLocator
	}
	row, col, err = ParseCellRef(ref)
	return sheet, row, col, err
}

// rowLocator and columnLocator name a whole row or column of a sheet, for notes ("Sheet1!5:5",
// "Sheet1!C:C").
func rowLocator(sheet string, row int) string {
	return SheetPrefix(sheet) + "!" + strconv.Itoa(row) + ":" + strconv.Itoa(row)
}

func columnLocator(sheet string, col int) string {
	return SheetPrefix(sheet) + "!" + ColumnName(col) + ":" + ColumnName(col)
}
