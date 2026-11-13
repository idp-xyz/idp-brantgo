package ingest

import (
	"errors"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/codes"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// inquiry is a synthetic zh/en inquiry text (no customer data), with CR LF line ends, a BOM and an
// empty line.
const inquiry = "\ufeff客户：Synthetic Trading Co.\r\n" +
	"询价 RFQ-T01\r\n" +
	"\r\n" +
	"1. 针织T恤 款号 ST-100，数量 120 PCS\r\n" +
	"2. Polo shirt, style SP-200, qty 2 DOZEN\r\n"

// Text: line:col locators, line numbers of the file, columns in code points (WBS S4-04, BC-AT27).
func TestExtractTextLocatesLinesAndColumns(t *testing.T) {
	e, err := Q01Upload.ExtractText([]byte(inquiry))
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != KindText || e.Extractor != TextExtractor || e.Profile != "Q01.upload.v0.1" || len(e.FileSHA256) != 64 {
		t.Fatalf("header %+v", e)
	}
	want := []Segment{
		{"1:1", "客户：Synthetic Trading Co."},
		{"2:1", "询价 RFQ-T01"},
		{"3:1", ""},
		{"4:1", "1. 针织T恤 款号 ST-100，数量 120 PCS"},
		{"5:1", "2. Polo shirt, style SP-200, qty 2 DOZEN"},
	}
	if len(e.Segments) != len(want) {
		t.Fatalf("segments %q", e.Segments)
	}
	for i, s := range want {
		if e.Segments[i] != s {
			t.Errorf("segment %d = %q, want %q", i, e.Segments[i], s)
		}
	}
	if e.Characters != len([]rune(e.Text())) || strings.Contains(e.Text(), "\r") {
		t.Fatalf("characters %d for %q", e.Characters, e.Text())
	}
	// "120" starts at the 22nd code point of line 4.
	if got := e.Find("120"); len(got) != 1 || got[0] != "4:22" {
		t.Fatalf("Find(120) = %v", got)
	}
	if !e.Verify("4:22", "120 PCS") || e.Verify("4:23", "120") || e.Verify("4:22", "") {
		t.Fatal("Verify at 4:22")
	}
	if rest, err := e.At("5:22"); err != nil || rest != "SP-200, qty 2 DOZEN" {
		t.Fatalf("At(5:22) = %q, %v", rest, err)
	}
	if rest, err := e.At("3:1"); err != nil || rest != "" {
		t.Fatalf("At(3:1) = %q, %v", rest, err)
	}
	for _, bad := range []string{"6:1", "3:2", "0:1", "4:0", "4", "04:1", "4:1:1", "Sheet1!A1", "1:1x"} {
		if _, err := e.At(bad); !errors.Is(err, ErrLocator) {
			t.Errorf("At(%q) = %v", bad, err)
		}
	}
	// Two occurrences on one line and one each on two others: Find returns all four, in order.
	if got := e.Find("S"); strings.Join(got, ",") != "1:4,4:12,4:28,5:22" {
		t.Fatalf("Find(S) = %v", got)
	}
}

// The same bytes give the same extract and SHA-256; any other byte gives another SHA-256 (WBS
// S4-04: deterministic output with a SHA-256 of the extract).
func TestExtractTextIsDeterministic(t *testing.T) {
	a, err := Q01Upload.ExtractText([]byte(inquiry))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Q01Upload.ExtractText([]byte(inquiry))
	if a.SHA256 != b.SHA256 || string(a.Canonical()) != string(b.Canonical()) {
		t.Fatal("two reads differ")
	}
	c, _ := Q01Upload.ExtractText([]byte(strings.Replace(inquiry, "120", "118", 1)))
	if c.SHA256 == a.SHA256 {
		t.Fatal("a changed quantity keeps the SHA-256")
	}
	// The same text with LF line ends is another file (FileSHA256), with the same segments.
	d, _ := Q01Upload.ExtractText([]byte(strings.ReplaceAll(strings.TrimPrefix(inquiry, "\ufeff"), "\r\n", "\n")))
	if d.FileSHA256 == a.FileSHA256 || d.SHA256 == a.SHA256 || d.Text() != a.Text() {
		t.Fatal("line ends")
	}
}

// Q01.upload.v0.1: text beyond 50,000 characters, not UTF-8, with control characters or empty is
// refused with INPUT_PROFILE_UNSUPPORTED, never truncated (baseline 14.2).
func TestExtractTextRefusesOutsideTheProfile(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
		limit   Limit
		path    string
		reason  string
	}{
		{"empty", nil, LimitFileBytes, "/file", "limit=file_bytes actual=0 max=10485760"},
		{"over 10 MiB", []byte(strings.Repeat("a", 10<<20+1)), LimitFileBytes, "/file", "limit=file_bytes actual=10485761 max=10485760"},
		{"50,001 characters", []byte(strings.Repeat("询", 50001)), LimitCharacters, "/file", "limit=characters actual=50001 max=50000"},
		{"not UTF-8", []byte("qty \xff"), LimitEncoding, "/file", "limit=encoding"},
		{"NUL", []byte("line one\nqty\x00 120"), LimitEncoding, "/file/2:4", `limit=encoding where="2:4"`},
		{"form feed", []byte("a\fb"), LimitEncoding, "/file/1:2", `limit=encoding where="1:2"`},
		{"C1 control", []byte("a\u0085b"), LimitEncoding, "/file/1:2", `limit=encoding where="1:2"`},
		{"white space only", []byte(" \r\n\t\n"), LimitEmpty, "/file", "limit=empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := Q01Upload.ExtractText(tc.content)
			var u *Unsupported
			if e != nil || !errors.As(err, &u) || u.Limit != tc.limit {
				t.Fatalf("extract %v, error %v", e, err)
			}
			r, ok := kernel.AsRejection(err)
			if !ok || r.Code != codes.InputProfileUnsupported || r.Category != kernel.CategoryInvalid ||
				r.Fields[0].Path != tc.path || r.Fields[0].Reason != tc.reason {
				t.Fatalf("rejection %+v", r)
			}
		})
	}
	// Exactly 50,000 characters, tabs and a final line end are within the profile.
	ok := strings.Repeat("询", 49998) + "\tA\n"
	if e, err := Q01Upload.ExtractText([]byte(ok)); err != nil || e.Characters != 50000 {
		t.Fatalf("%v, %v", e, err)
	}
}
