package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Kind is the type of the file an extract comes from.
type Kind string

const (
	KindText Kind = "text"
	KindPDF  Kind = "pdf"
	KindXLSX Kind = "xlsx"
)

// Segment is one located piece of an extract: a line of text, a visual line of a PDF page, or the
// value of one cell. Text holds no line break, except a cell value written on several lines.
type Segment struct {
	Locator string
	Text    string
}

// Note discloses what a reader read but does not show as a plain value, or did not read, at a
// locator (or a sheet, row, column or page). The note kinds are the Note* constants.
type Note struct {
	Locator string
	Kind    string
}

// Note kinds.
const (
	NoteFormulaCachedValue     = "formula_cached_value"        // the cell's value is the result Excel stored; the formula was not evaluated
	NoteDateFromSerial         = "date_from_serial"            // a date or time format: the serial number is shown as ISO 8601
	NoteErrorValue             = "error_value"                 // the cell holds an Excel error such as #DIV/0!
	NoteHiddenSheet            = "hidden_sheet"                // the sheet is hidden in Excel
	NoteHiddenRow              = "hidden_row"                  // the row holds values but is hidden in Excel
	NoteHiddenColumn           = "hidden_column"               // the column holds values but is hidden in Excel
	NoteChartsheet             = "chartsheet"                  // a chart sheet: no cell values
	NoteExternalLinksNotFollow = "external_links_not_followed" // the workbook links other workbooks; only cached values are shown
	NoteEmbeddedNotOpened      = "embedded_objects_not_opened" // embedded objects or ActiveX controls were not opened
	NoteDrawingsNotExtracted   = "drawings_not_extracted"      // text boxes, shapes and charts are not cell values
	NoteCommentsNotExtracted   = "comments_not_extracted"      // cell comments and notes are not cell values
	NoteImagesNotExtracted     = "images_not_extracted"        // the page shows images; text in them is not read
	NoteInvisibleText          = "invisible_text"              // the line holds text drawn invisibly, below 2 points or outside the page
	NoteUndecodableGlyphs      = "undecodable_glyphs"          // the line holds glyphs without a Unicode mapping (U+FFFD)
)

// Extract is the deterministic result of reading one file under a profile. It is a candidate source
// (ADR-BR-010), registered with EVD by unit B4 with its SHA-256 and extractor version.
type Extract struct {
	Kind       Kind
	Extractor  string   // the reader and its version, e.g. "brant.ingest.pdf/1"; it defines the locators
	Profile    string   // the profile the file was read under
	FileSHA256 string   // SHA-256 of the file's bytes, lower-case hex
	Characters int      // runes of Text(): the measure of the profile's character limit
	Pages      int      // PDF: pages of the document, blank ones included
	Sheets     []string // XLSX: every sheet of the workbook in order, chart sheets included
	Segments   []Segment
	Notes      []Note
	SHA256     string // SHA-256 of Canonical(), lower-case hex
}

// Text returns the extracted text: the segments' texts joined by line feeds.
func (e *Extract) Text() string {
	parts := make([]string, len(e.Segments))
	for i, s := range e.Segments {
		parts[i] = s.Text
	}
	return strings.Join(parts, "\n")
}

// canonicalHeader names the canonical form's version.
const canonicalHeader = "brant-extract/1"

// Canonical returns the bytes the extract's SHA-256 is taken over: a header line, one tab-separated
// record per field, segment and note, every value escaped (backslash, tab, line feed, carriage
// return), so that the form is unambiguous and readable.
func (e *Extract) Canonical() []byte {
	var b strings.Builder
	rec := func(fields ...string) {
		for i, f := range fields {
			if i > 0 {
				b.WriteByte('\t')
			}
			b.WriteString(escape(f))
		}
		b.WriteByte('\n')
	}
	b.WriteString(canonicalHeader + "\n")
	rec("kind", string(e.Kind))
	rec("extractor", e.Extractor)
	rec("profile", e.Profile)
	rec("file_sha256", e.FileSHA256)
	rec("characters", strconv.Itoa(e.Characters))
	if e.Kind == KindPDF {
		rec("pages", strconv.Itoa(e.Pages))
	}
	for _, s := range e.Sheets {
		rec("sheet", s)
	}
	for _, s := range e.Segments {
		rec("segment", s.Locator, s.Text)
	}
	for _, n := range e.Notes {
		rec("note", n.Kind, n.Locator)
	}
	return []byte(b.String())
}

var escaper = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

func escape(s string) string { return escaper.Replace(s) }

// seal counts the characters, drops repeated notes and takes the SHA-256.
func (e *Extract) seal() *Extract {
	n := 0
	for i, s := range e.Segments {
		if i > 0 {
			n++
		}
		n += utf8.RuneCountInString(s.Text)
	}
	e.Characters = n
	seen := make(map[Note]bool, len(e.Notes))
	notes := e.Notes[:0]
	for _, x := range e.Notes {
		if !seen[x] {
			seen[x] = true
			notes = append(notes, x)
		}
	}
	e.Notes = notes
	sum := sha256.Sum256(e.Canonical())
	e.SHA256 = hex.EncodeToString(sum[:])
	return e
}

// At returns the text a locator points at: for text, the rest of the line from the column on; for
// PDF, the line; for XLSX, the cell's value. A locator in another notation, malformed or pointing
// at nothing is ErrLocator.
func (e *Extract) At(locator string) (string, error) {
	switch e.Kind {
	case KindText:
		line, col, err := ParseTextLocator(locator)
		if err != nil || line > len(e.Segments) {
			return "", ErrLocator
		}
		text := e.Segments[line-1].Text
		for i := range text {
			if col == 1 {
				return text[i:], nil
			}
			col--
		}
		if col == 1 {
			return "", nil // just past the end of the line
		}
		return "", ErrLocator
	case KindPDF:
		page, line, err := ParsePDFLocator(locator)
		if err != nil {
			return "", ErrLocator
		}
		return e.find(PDFLocator(page, line))
	case KindXLSX:
		sheet, row, col, err := ParseCellLocator(locator)
		if err != nil {
			return "", ErrLocator
		}
		return e.find(CellLocator(sheet, row, col))
	}
	return "", ErrLocator
}

func (e *Extract) find(locator string) (string, error) {
	for _, s := range e.Segments {
		if s.Locator == locator {
			return s.Text, nil
		}
	}
	return "", ErrLocator
}

// Verify reports whether quote stands at the locator: at the column of a text line, anywhere in a
// PDF line or a cell's value. An empty quote verifies nothing.
func (e *Extract) Verify(locator, quote string) bool {
	text, err := e.At(locator)
	if err != nil || quote == "" {
		return false
	}
	if e.Kind == KindText {
		return strings.HasPrefix(text, quote)
	}
	return strings.Contains(text, quote)
}

// Find returns the locators of every occurrence of quote in document order: for text, the line and
// column of each occurrence; for PDF and XLSX, each line or cell that contains it. More than one
// locator means the quote alone is ambiguous (SOURCE_AMBIGUOUS when a candidate rests on it).
func (e *Extract) Find(quote string) []string {
	if quote == "" {
		return nil
	}
	var out []string
	for i, s := range e.Segments {
		if e.Kind != KindText {
			if strings.Contains(s.Text, quote) {
				out = append(out, s.Locator)
			}
			continue
		}
		for from := 0; ; {
			j := strings.Index(s.Text[from:], quote)
			if j < 0 {
				break
			}
			col := utf8.RuneCountInString(s.Text[:from+j]) + 1
			out = append(out, TextLocator(i+1, col))
			_, size := utf8.DecodeRuneInString(s.Text[from+j:])
			from += j + size
		}
	}
	return out
}
