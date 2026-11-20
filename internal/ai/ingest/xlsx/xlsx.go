// Package xlsx reads the cell values of an XLSX workbook with the standard library only
// (archive/zip and encoding/xml; decision E-8). It evaluates nothing: no formula (a formula cell
// shows the value Excel stored with it, and one without a stored value is refused), no macro (a
// workbook with VBA, Excel 4 macro sheets or dialog sheets is refused), no external link, no
// embedded object. The package bounds what it reads (zip entries, uncompressed bytes per part and
// in all, sheets, rows, columns and characters) and stops at the first bound it meets, so that a
// zip bomb or an oversized workbook costs at most the bounds.
//
// XML is read with encoding/xml, which expands no external entity and no DTD; documents in another
// encoding than UTF-8 are refused.
package xlsx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Limits bound a read. Zero values are not defaults: every field must be set.
type Limits struct {
	MaxSheets     int   // sheets of the workbook
	MaxRows       int   // rows holding a value, per sheet
	MaxColumns    int   // columns holding a value, per sheet
	MaxCharacters int   // characters of all values, counted as values joined by line feeds
	MaxEntries    int   // entries of the zip archive
	MaxPartBytes  int64 // uncompressed bytes read from one part
	MaxTotalBytes int64 // uncompressed bytes read from all parts
}

// DefaultLimits are the technical bounds of the reader under the given profile limits: 1,000 zip
// entries, 32 MiB per part and 96 MiB in all (a sheet of 500 rows and 40 columns takes about 1 MiB
// of XML; styled empty cells take more).
func DefaultLimits(sheets, rows, columns, characters int) Limits {
	return Limits{
		MaxSheets: sheets, MaxRows: rows, MaxColumns: columns, MaxCharacters: characters,
		MaxEntries: 1000, MaxPartBytes: 32 << 20, MaxTotalBytes: 96 << 20,
	}
}

// Reason is the rule a workbook broke.
type Reason string

const (
	ReasonType       Reason = "type"       // not an XLSX package (no workbook, binary or template of another kind)
	ReasonMacros     Reason = "macros"     // VBA project, macro-enabled workbook, macro or dialog sheet
	ReasonSheets     Reason = "sheets"     // more sheets than MaxSheets
	ReasonRows       Reason = "rows"       // a sheet with more rows holding values than MaxRows
	ReasonColumns    Reason = "columns"    // a sheet with more columns holding values than MaxColumns
	ReasonCharacters Reason = "characters" // more characters than MaxCharacters
	ReasonFormula    Reason = "formula"    // a formula without a cached value
	ReasonEncoding   Reason = "encoding"   // a value holding a control character
	ReasonStructure  Reason = "structure"  // malformed package, part or XML, or a part beyond the byte bounds
)

// Error is a refusal. Where names a part, or Sheet with Row and Col (1-based) a cell, Sheet alone a
// sheet; Actual and Max are set for counts, AtLeast when reading stopped at the bound.
type Error struct {
	Reason      Reason
	Part        string
	Sheet       string
	Row, Col    int
	Actual, Max int64
	AtLeast     bool
}

func (e *Error) Error() string {
	s := "xlsx: " + string(e.Reason)
	if e.Part != "" {
		s += " in " + e.Part
	}
	if e.Sheet != "" {
		s += " in sheet " + strconv.Quote(e.Sheet)
	}
	if e.Row > 0 {
		s += fmt.Sprintf(" at row %d column %d", e.Row, e.Col)
	}
	if e.Max > 0 {
		s += fmt.Sprintf(" (%d > %d)", e.Actual, e.Max)
	}
	return s
}

// Workbook is what the reader returns: the sheets in workbook order and the parts it did not read.
type Workbook struct {
	Sheets        []Sheet
	ExternalLinks bool // links to other workbooks (only cached values are shown)
	Embedded      bool // embedded objects or ActiveX controls
	Drawings      bool // drawings, charts, text boxes or shapes
	Comments      bool // cell comments, notes or threaded comments
}

// Sheet is one sheet.
type Sheet struct {
	Name          string
	Chartsheet    bool   // a chart sheet: no cells
	Hidden        bool   // state hidden or veryHidden
	Cells         []Cell // cells holding a value, by row then column
	HiddenRows    []int  // hidden rows holding values, ascending
	HiddenColumns []int  // hidden columns holding values, ascending
}

// Cell is a cell holding a value. Value is the value as Excel stores it, rendered as text: shared
// and inline strings as written (with _xHHHH_ escapes decoded and line ends as LF); numbers with at
// most 15 significant digits (Excel's precision), in plain notation; booleans TRUE or FALSE; errors
// as written (#N/A); a number in a date or time format as ISO 8601 (2026-11-13, 2026-11-13T08:30:00
// or 08:30:00).
type Cell struct {
	Row, Col int
	Value    string
	Formula  bool // the value is the result stored with a formula; the formula was not evaluated
	Date     bool // the value was a serial number in a date or time format
	Error    bool // the value is an Excel error
}

// Read reads the cell values of the workbook in data within the limits.
func Read(data []byte, lim Limits) (wb *Workbook, err error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		// archive/zip refuses insecure names (zip.ErrInsecurePath) and malformed archives alike.
		return nil, &Error{Reason: ReasonStructure}
	}
	if len(zr.File) > lim.MaxEntries {
		return nil, &Error{Reason: ReasonStructure, Actual: int64(len(zr.File)), Max: int64(lim.MaxEntries)}
	}
	r := &reader{lim: lim, total: lim.MaxTotalBytes, parts: map[string]*zip.File{}}
	wb = &Workbook{}
	for _, f := range zr.File {
		name := strings.ToLower(f.Name)
		if _, dup := r.parts[name]; dup || strings.HasSuffix(name, "/") && f.UncompressedSize64 > 0 {
			return nil, &Error{Reason: ReasonStructure, Part: f.Name}
		}
		r.parts[name] = f
		switch {
		case strings.HasSuffix(name, "vbaproject.bin"), strings.Contains(name, "macrosheets/"),
			strings.Contains(name, "dialogsheets/"):
			return nil, &Error{Reason: ReasonMacros, Part: f.Name}
		case strings.HasPrefix(name, "xl/externallinks/"):
			wb.ExternalLinks = true
		case strings.HasPrefix(name, "xl/embeddings/"), strings.HasPrefix(name, "xl/activex/"),
			strings.HasPrefix(name, "xl/ctrlprops/"):
			wb.Embedded = true
		case strings.HasPrefix(name, "xl/drawings/"), strings.HasPrefix(name, "xl/charts/"):
			wb.Drawings = true
		case strings.HasPrefix(name, "xl/comments"), strings.HasPrefix(name, "xl/threadedcomments/"),
			strings.HasPrefix(name, "xl/persons/"):
			wb.Comments = true
		}
	}
	main, err := r.mainPart()
	if err != nil {
		return nil, err
	}
	if err = r.checkContentType(main); err != nil {
		return nil, err
	}
	sheets, err := r.workbook(main)
	if err != nil {
		return nil, err
	}
	if len(sheets) > lim.MaxSheets {
		return nil, &Error{Reason: ReasonSheets, Actual: int64(len(sheets)), Max: int64(lim.MaxSheets)}
	}
	if len(sheets) == 0 {
		return nil, &Error{Reason: ReasonStructure, Part: main}
	}
	rels, err := r.rels(main)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rels))
	for id := range rels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sharedRead, stylesRead := false, false
	for _, id := range ids {
		rel := rels[id]
		switch {
		case rel.is("sharedStrings") && !rel.external:
			if sharedRead {
				return nil, &Error{Reason: ReasonStructure, Part: main}
			}
			if r.shared, err = r.sharedStrings(rel.target); err != nil {
				return nil, err
			}
			sharedRead = true
		case rel.is("styles") && !rel.external:
			if stylesRead {
				return nil, &Error{Reason: ReasonStructure, Part: main}
			}
			if r.dates, err = r.styles(rel.target); err != nil {
				return nil, err
			}
			stylesRead = true
		}
	}
	seen := map[string]bool{}
	for _, s := range sheets {
		if seen[strings.ToLower(s.name)] {
			return nil, &Error{Reason: ReasonStructure, Sheet: s.name}
		}
		seen[strings.ToLower(s.name)] = true
		rel, ok := rels[s.rid]
		if !ok || rel.external {
			return nil, &Error{Reason: ReasonStructure, Sheet: s.name}
		}
		sheet := Sheet{Name: s.name, Hidden: s.hidden}
		switch {
		case rel.is("worksheet"):
			if err = r.worksheet(rel.target, &sheet); err != nil {
				return nil, err
			}
		case rel.is("chartsheet"):
			sheet.Chartsheet = true
		case rel.is("dialogsheet"), rel.is("macrosheet"), rel.is("xlMacrosheet"), rel.is("xlIntlMacrosheet"):
			return nil, &Error{Reason: ReasonMacros, Sheet: s.name}
		default:
			return nil, &Error{Reason: ReasonStructure, Sheet: s.name}
		}
		wb.Sheets = append(wb.Sheets, sheet)
	}
	return wb, nil
}

var errTooLarge = errors.New("xlsx: part beyond the byte bounds")

type reader struct {
	lim      Limits
	total    int64 // uncompressed bytes left to read in all
	parts    map[string]*zip.File
	shared   []string         // the shared string table
	dates    map[int]dateKind // cell style index -> date or time format
	date1904 bool
	chars    int64 // characters of the values read so far, with one separator each
}

// open returns a decoder over a part, bounded per part and in all. Part names are case-insensitive
// (OPC).
func (r *reader) open(name string) (*xml.Decoder, func(), error) {
	f, ok := r.parts[strings.ToLower(name)]
	if !ok {
		return nil, nil, &Error{Reason: ReasonStructure, Part: name}
	}
	rc, err := f.Open()
	if err != nil {
		return nil, nil, &Error{Reason: ReasonStructure, Part: name}
	}
	lr := &bounded{r: rc, part: r.lim.MaxPartBytes, total: &r.total}
	dec := xml.NewDecoder(lr)
	dec.Strict = true
	return dec, func() { _ = rc.Close() }, nil
}

// bounded fails a read beyond the per-part bound or the bytes left in all.
type bounded struct {
	r     io.Reader
	part  int64
	total *int64
}

func (b *bounded) Read(p []byte) (int, error) {
	left := min(b.part, *b.total)
	if left <= 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > left {
		p = p[:left]
	}
	n, err := b.r.Read(p)
	b.part -= int64(n)
	*b.total -= int64(n)
	return n, err
}

// partError turns a read or XML error into a refusal of the part.
func partError(name string, err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Reason: ReasonStructure, Part: name}
}

// walk calls fn for each start element of a part, with the decoder positioned after it.
func (r *reader) walk(name string, fn func(dec *xml.Decoder, start xml.StartElement) error) error {
	dec, closeFn, err := r.open(name)
	if err != nil {
		return err
	}
	defer closeFn()
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return partError(name, err)
		}
		if start, ok := tok.(xml.StartElement); ok {
			if err = fn(dec, start); err != nil {
				return partError(name, err)
			}
		}
	}
}

func attr(start xml.StartElement, local string) (string, bool) {
	for _, a := range start.Attr {
		if a.Name.Local == local && a.Name.Space == "" {
			return a.Value, true
		}
	}
	return "", false
}

// relID returns the r:id attribute: local name id in the relationships namespace (transitional or
// strict).
func relID(start xml.StartElement) string {
	for _, a := range start.Attr {
		if a.Name.Local == "id" && strings.HasSuffix(a.Name.Space, "relationships") {
			return a.Value
		}
	}
	return ""
}

func truthy(s string) bool { return s == "1" || s == "true" }

type relationship struct {
	typ, target string
	external    bool
}

// is reports whether the relationship has the type name (transitional or strict namespace).
func (rel relationship) is(name string) bool { return strings.HasSuffix(rel.typ, "/"+name) }

// relsName returns the relationships part of a part: xl/workbook.xml -> xl/_rels/workbook.xml.rels.
func relsName(part string) string {
	dir, file := path.Split(part)
	return dir + "_rels/" + file + ".rels"
}

// resolve returns the part a relationship target names, relative to the source part's directory or
// absolute from the package root; a target outside the package is refused.
func resolve(source, target string) (string, bool) {
	var p string
	if strings.HasPrefix(target, "/") {
		p = path.Clean(target[1:])
	} else {
		p = path.Clean(path.Join(path.Dir(source), target))
	}
	if p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
		return "", false
	}
	return p, true
}

// rels reads the relationships of a part, by ID.
func (r *reader) rels(source string) (map[string]relationship, error) {
	out := map[string]relationship{}
	name := relsName(source)
	if source == "" {
		name = "_rels/.rels"
	}
	err := r.walk(name, func(_ *xml.Decoder, start xml.StartElement) error {
		if start.Name.Local != "Relationship" {
			return nil
		}
		id, _ := attr(start, "Id")
		typ, _ := attr(start, "Type")
		target, _ := attr(start, "Target")
		mode, _ := attr(start, "TargetMode")
		if _, dup := out[id]; dup || id == "" {
			return &Error{Reason: ReasonStructure, Part: name}
		}
		rel := relationship{typ: typ, external: mode == "External"}
		if !rel.external {
			p, ok := resolve(source, target)
			if !ok {
				return &Error{Reason: ReasonStructure, Part: name}
			}
			rel.target = p
		}
		out[id] = rel
		return nil
	})
	return out, err
}

// mainPart returns the workbook part the package relationships name.
func (r *reader) mainPart() (string, error) {
	rels, err := r.rels("")
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Part == "_rels/.rels" && e.Reason == ReasonStructure {
			if _, ok := r.parts["_rels/.rels"]; !ok {
				return "", &Error{Reason: ReasonType}
			}
		}
		return "", err
	}
	ids := make([]string, 0, len(rels))
	for id := range rels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if rel := rels[id]; rel.is("officeDocument") && !rel.external {
			return rel.target, nil
		}
	}
	return "", &Error{Reason: ReasonType}
}

// checkContentType refuses a main part that is not a workbook of cell values: macro-enabled
// workbooks (xlsm, xltm) as macros, binary workbooks (xlsb) and anything else as another type. The
// part's content type is its Override, or else the Default of its extension (OPC; some libraries
// declare the workbook by a Default for "xml").
func (r *reader) checkContentType(main string) error {
	var override, byDefault string
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(main), "."))
	err := r.walk("[Content_Types].xml", func(_ *xml.Decoder, start xml.StartElement) error {
		switch start.Name.Local {
		case "Override":
			if part, _ := attr(start, "PartName"); strings.EqualFold(strings.TrimPrefix(part, "/"), main) {
				override, _ = attr(start, "ContentType")
			}
		case "Default":
			if e, _ := attr(start, "Extension"); strings.EqualFold(e, ext) {
				byDefault, _ = attr(start, "ContentType")
			}
		}
		return nil
	})
	ct := override
	if ct == "" {
		ct = byDefault
	}
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Part == "[Content_Types].xml" {
			return &Error{Reason: ReasonType}
		}
		return err
	}
	switch strings.ToLower(ct) {
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.template.main+xml":
		return nil
	case "application/vnd.ms-excel.sheet.macroenabled.main+xml",
		"application/vnd.ms-excel.template.macroenabled.main+xml",
		"application/vnd.ms-excel.addin.macroenabled.main+xml":
		return &Error{Reason: ReasonMacros, Part: main}
	}
	return &Error{Reason: ReasonType, Part: main}
}

type sheetRef struct {
	name, rid string
	hidden    bool
}

// workbook reads the sheet list and the date system.
func (r *reader) workbook(main string) ([]sheetRef, error) {
	var sheets []sheetRef
	err := r.walk(main, func(_ *xml.Decoder, start xml.StartElement) error {
		switch start.Name.Local {
		case "workbookPr":
			v, _ := attr(start, "date1904")
			r.date1904 = truthy(v)
		case "sheet":
			name, _ := attr(start, "name")
			state, _ := attr(start, "state")
			if name == "" {
				return &Error{Reason: ReasonStructure, Part: main}
			}
			sheets = append(sheets, sheetRef{name: name, rid: relID(start), hidden: state == "hidden" || state == "veryHidden"})
		}
		return nil
	})
	return sheets, err
}

// sharedStrings reads the shared string table.
func (r *reader) sharedStrings(name string) ([]string, error) {
	var out []string
	err := r.walk(name, func(dec *xml.Decoder, start xml.StartElement) error {
		if start.Name.Local != "si" {
			return nil
		}
		s, err := richText(dec)
		if err != nil {
			return err
		}
		out = append(out, s)
		return nil
	})
	return out, err
}

// richText reads the text of a <si> or <is> element up to its end: the <t> elements directly in it
// or in its runs, without phonetic runs (<rPh>).
func richText(dec *xml.Decoder) (string, error) {
	var b strings.Builder
	depth, inT, phonetic := 1, 0, 0
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "t":
				inT++
			case "rPh":
				phonetic++
			}
		case xml.EndElement:
			depth--
			switch t.Name.Local {
			case "t":
				inT--
			case "rPh":
				phonetic--
			}
		case xml.CharData:
			if inT > 0 && phonetic == 0 {
				b.Write(t)
			}
		}
	}
	return unescape(b.String()), nil
}

// unescape decodes the _xHHHH_ escapes of ECMA-376 ST_Xstring (_x005F_ is a literal underscore; a
// surrogate pair is two escapes, a lone surrogate stays as written) and reads CR LF and CR as LF.
func unescape(s string) string {
	if strings.Contains(s, "_x") {
		var b strings.Builder
		for i := 0; i < len(s); {
			if v, ok := escaped(s, i); ok {
				switch {
				case v < 0xd800 || v > 0xdfff:
					b.WriteRune(v)
					i += 7
					continue
				case v < 0xdc00:
					if lo, ok := escaped(s, i+7); ok && lo >= 0xdc00 && lo <= 0xdfff {
						b.WriteRune(0x10000 + (v-0xd800)<<10 + (lo - 0xdc00))
						i += 14
						continue
					}
				}
			}
			b.WriteByte(s[i])
			i++
		}
		s = b.String()
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// escaped reads an _xHHHH_ escape at s[i:].
func escaped(s string, i int) (rune, bool) {
	if i+7 > len(s) || s[i] != '_' || s[i+1] != 'x' || s[i+6] != '_' {
		return 0, false
	}
	v, err := strconv.ParseUint(s[i+2:i+6], 16, 16)
	return rune(v), err == nil
}

// control reports a control character that is not text: C0 other than tab and line feed, DEL and
// C1.
func control(r rune) bool {
	return (r < 0x20 && r != '\t' && r != '\n') || (r >= 0x7f && r <= 0x9f)
}
