package ingest

import (
	"errors"
	"testing"
	"unicode/utf8"
)

func TestColumnsAndCellReferences(t *testing.T) {
	for col, name := range map[int]string{1: "A", 26: "Z", 27: "AA", 52: "AZ", 703: "AAA", 16384: "XFD"} {
		if got := ColumnName(col); got != name {
			t.Errorf("ColumnName(%d) = %s", col, got)
		}
		if r, c, err := ParseCellRef(name + "7"); err != nil || r != 7 || c != col {
			t.Errorf("ParseCellRef(%s7) = %d, %d, %v", name, r, c, err)
		}
	}
	if r, c, err := ParseCellRef("xfd1048576"); err != nil || r != MaxRow || c != MaxColumn {
		t.Fatalf("lower case at the grid's end: %d %d %v", r, c, err)
	}
	for _, bad := range []string{"", "A", "7", "1A", "A0", "A01", "XFE1", "A1048577", "$A$1", "AAAA1", "A1B", "A-1"} {
		if _, _, err := ParseCellRef(bad); !errors.Is(err, ErrLocator) {
			t.Errorf("ParseCellRef(%q) accepted", bad)
		}
	}
}

// Sheet names are quoted as Excel quotes them, and every locator reads back to the same cell.
func TestCellLocatorsQuoteLikeExcel(t *testing.T) {
	for name, prefix := range map[string]string{
		"Sheet1": "Sheet1", "询价单": "询价单", "_x": "_x", "a.b": "a.b", "Q1 2026": "'Q1 2026'", "It's": "'It''s'",
		"A1": "'A1'", "xfd9": "'xfd9'", "R1C1": "'R1C1'", "r": "'r'", "C3D": "C3D", "TRUE": "'TRUE'", "1st": "'1st'",
		"a-b": "'a-b'", "'": "''''",
	} {
		if got := SheetPrefix(name); got != prefix {
			t.Errorf("SheetPrefix(%q) = %s, want %s", name, got, prefix)
		}
		loc := CellLocator(name, 12, 3)
		if sheet, row, col, err := ParseCellLocator(loc); err != nil || sheet != name || row != 12 || col != 3 {
			t.Errorf("ParseCellLocator(%s) = %q %d %d %v", loc, sheet, row, col, err)
		}
	}
	for _, bad := range []string{"", "!A1", "''!A1", "'abc!A1", "'abc'A1", "Sheet 1!A1", "S!", "S!A", "'S'!B0", "A1!B2"} {
		if _, _, _, err := ParseCellLocator(bad); !errors.Is(err, ErrLocator) {
			t.Errorf("ParseCellLocator(%q) accepted", bad)
		}
	}
	if rowLocator("Q1 2026", 5) != "'Q1 2026'!5:5" || columnLocator("S", 28) != "S!AB:AB" {
		t.Fatal("row and column locators")
	}
}

func TestLinePairs(t *testing.T) {
	if l, c, err := ParseTextLocator("12:7"); err != nil || l != 12 || c != 7 || TextLocator(12, 7) != "12:7" {
		t.Fatal("text locator")
	}
	for _, bad := range []string{"", ":", "1:", ":1", "0:1", "1:0", "01:1", "+1:1", "1:1:1", "1:1000000000", "a:b"} {
		if _, _, err := ParsePDFLocator(bad); !errors.Is(err, ErrLocator) {
			t.Errorf("ParsePDFLocator(%q) accepted", bad)
		}
	}
}

// FuzzCellLocator: any sheet name and cell write and read back as the same cell; whatever parses
// writes back to a locator that parses to the same cell.
func FuzzCellLocator(f *testing.F) {
	f.Add("Q1 2026", 3, 2)
	f.Add("It's", 1048576, 16384)
	f.Add("询价单", 1, 1)
	f.Fuzz(func(t *testing.T, name string, row, col int) {
		if name == "" || !utf8.ValidString(name) || row < 1 || row > MaxRow || col < 1 || col > MaxColumn {
			return
		}
		loc := CellLocator(name, row, col)
		sheet, r, c, err := ParseCellLocator(loc)
		if err != nil || sheet != name || r != row || c != col {
			t.Fatalf("%q -> %q -> %q %d %d %v", name, loc, sheet, r, c, err)
		}
		if sheet, r, c, err := ParseCellLocator(name); err == nil {
			if again := CellLocator(sheet, r, c); again != name {
				if s2, r2, c2, err := ParseCellLocator(again); err != nil || s2 != sheet || r2 != r || c2 != c {
					t.Fatalf("%q parses to %q %d %d, written %q", name, sheet, r, c, again)
				}
			}
		}
	})
}
