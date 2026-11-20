// Package ingest turns a business file into a deterministic extract with locators, under the
// restricted input profile Q01.upload.v0.1 (baseline 14.2; WBS S4-04; decision E-8). Parsing is
// candidate generation in the AI layer, while EVD keeps the file (ADR-BR-010): an extract is never
// a business fact, and the human verifies every value against the file through its locator
// (BC-AT27).
//
// A file outside the profile is refused with INPUT_PROFILE_UNSUPPORTED and goes to the manual
// path; nothing is truncated, and a refusal never comes with a partial extract. What a reader reads
// but cannot show as plain values (a formula's cached result, a hidden sheet, invisible PDF text,
// images, drawings, comments) is disclosed as a Note of the extract.
//
// The readers are pure and deterministic: the same bytes give the same extract and the same
// SHA-256 for one extractor version. Text is read here; XLSX by the values-only reader in
// ingest/xlsx (standard library only); PDF by the adapter in ingest/pdftext, the only package that
// imports the PDF library.
package ingest

import (
	"fmt"
	"strconv"
	"strings"

	"go.idp.xyz/idp-brant/internal/ai/codes"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Profile is a restricted input profile: the limits a task's files must keep (baseline 14.2). The
// limits are an experimental configuration, not measured capacity; a change makes a new profile ID.
type Profile struct {
	ID              string
	MaxAttachments  int // files per task
	MaxFileBytes    int // bytes per file
	MaxPDFPages     int // pages per PDF
	MaxSheets       int // sheets per workbook (every <sheet> of the workbook, hidden ones included)
	MaxSheetRows    int // rows holding at least one value, per sheet
	MaxSheetColumns int // columns holding at least one value, per sheet
	MaxCharacters   int // extracted characters per file and per task (Extract.Characters)
	MaxQuoteLines   int // lines of one quote: the candidate lines generated from a task's files
}

// Q01Upload is Q01.upload.v0.1: text (zh/en), PDF with a text layer of at most 20 pages, XLSX cell
// values only (at most 3 sheets of 500 rows and 40 columns), at most 3 attachments of 10 MiB per
// task with at most 50,000 extracted characters in all, and at most 200 quote lines.
var Q01Upload = Profile{
	ID:              "Q01.upload.v0.1",
	MaxAttachments:  3,
	MaxFileBytes:    10 << 20,
	MaxPDFPages:     20,
	MaxSheets:       3,
	MaxSheetRows:    500,
	MaxSheetColumns: 40,
	MaxCharacters:   50000,
	MaxQuoteLines:   200,
}

// Limit names the rule an input broke.
type Limit string

const (
	LimitType        Limit = "type"        // not text, PDF or XLSX, or the bytes are not of that format
	LimitFileBytes   Limit = "file_bytes"  // file larger than MaxFileBytes, or empty
	LimitEmpty       Limit = "empty"       // the file yields no text at all
	LimitAttachments Limit = "attachments" // more files in a task than MaxAttachments
	LimitCharacters  Limit = "characters"  // more extracted characters than MaxCharacters
	LimitPages       Limit = "pages"       // more PDF pages than MaxPDFPages
	LimitSheets      Limit = "sheets"      // more sheets than MaxSheets
	LimitRows        Limit = "rows"        // a sheet with more rows holding values than MaxSheetRows
	LimitColumns     Limit = "columns"     // a sheet with more columns holding values than MaxSheetColumns
	LimitQuoteLines  Limit = "quote_lines" // more candidate lines than MaxQuoteLines
	LimitEncoding    Limit = "encoding"    // text that is not UTF-8, control characters, PDF text without Unicode
	LimitTextLayer   Limit = "text_layer"  // a PDF or page without an extractable text layer (scan, images)
	LimitMacros      Limit = "macros"      // a workbook with macros (VBA, Excel 4 macro or dialog sheets)
	LimitFormula     Limit = "formula"     // a formula cell without a cached value
	LimitEncrypted   Limit = "encrypted"   // a PDF that needs a password or forbids extracting its text
	LimitStructure   Limit = "structure"   // malformed, or beyond a reader's technical bounds (zip bomb, nesting)
)

// Unsupported refuses an input outside the profile (INPUT_PROFILE_UNSUPPORTED). It says which limit
// was broken, where and by how much, so that the refusal can show the limit and the manual path
// (baseline 14.2: 不静默截断，展示限制及人工处理路径). errors.As finds the *kernel.Rejection in it.
type Unsupported struct {
	Profile string
	Limit   Limit
	Where   string // a locator, sheet name or file part; empty for the file as a whole
	Actual  int64  // the measured count, for a count limit
	Max     int64  // the profile's or reader's bound, for a count limit
	AtLeast bool   // the reader stopped counting at the bound: Actual is a lower bound
}

func (u *Unsupported) Error() string {
	return "ingest: input outside " + u.Profile + ": " + u.reason()
}

// Unwrap returns the rejection, so that kernel.AsRejection(err) finds it.
func (u *Unsupported) Unwrap() error { return u.Rejection() }

// Rejection is the refusal transports answer: INPUT_PROFILE_UNSUPPORTED (422) at /file or at
// /file/<where>, with the limit and the counts as reason.
func (u *Unsupported) Rejection() *kernel.Rejection {
	path := "/file"
	if u.Where != "" {
		path += "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(u.Where)
	}
	category, _ := codes.Category(codes.InputProfileUnsupported)
	return kernel.RejectAs(codes.InputProfileUnsupported, category, path, u.reason())
}

func (u *Unsupported) reason() string {
	var b strings.Builder
	b.WriteString("limit=" + string(u.Limit))
	if u.Where != "" {
		b.WriteString(" where=" + strconv.Quote(u.Where))
	}
	if u.Max > 0 {
		op := "="
		if u.AtLeast {
			op = ">="
		}
		fmt.Fprintf(&b, " actual%s%d max=%d", op, u.Actual, u.Max)
	}
	return b.String()
}

func (p Profile) refuse(limit Limit, where string) *Unsupported {
	return &Unsupported{Profile: p.ID, Limit: limit, Where: where}
}

func (p Profile) over(limit Limit, where string, actual, max int64, atLeast bool) *Unsupported {
	return &Unsupported{Profile: p.ID, Limit: limit, Where: where, Actual: actual, Max: max, AtLeast: atLeast}
}

// CheckAttachments refuses a task with more files than the profile allows; the caller checks it
// before reading any of them.
func (p Profile) CheckAttachments(n int) error {
	if n > p.MaxAttachments {
		return p.over(LimitAttachments, "", int64(n), int64(p.MaxAttachments), false)
	}
	return nil
}

// CheckBundle checks a task's extracts together: at most MaxAttachments files, extracted under this
// profile, with at most MaxCharacters characters in all.
func (p Profile) CheckBundle(extracts ...*Extract) error {
	if err := p.CheckAttachments(len(extracts)); err != nil {
		return err
	}
	total := 0
	for _, e := range extracts {
		if e == nil || e.Profile != p.ID {
			return p.refuse(LimitType, "")
		}
		total += e.Characters
	}
	if total > p.MaxCharacters {
		return p.over(LimitCharacters, "", int64(total), int64(p.MaxCharacters), false)
	}
	return nil
}

// CheckQuoteLines refuses more candidate lines than one quote may hold (报价至多 200 行). The limit
// counts quote lines generated from the files (unit B4), not lines of text: a sheet of 500 rows is
// within the profile.
func (p Profile) CheckQuoteLines(n int) error {
	if n > p.MaxQuoteLines {
		return p.over(LimitQuoteLines, "", int64(n), int64(p.MaxQuoteLines), false)
	}
	return nil
}
