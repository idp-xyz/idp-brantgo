// Package locate places the fields of RFQ line candidates in the file extracts they come from
// (WBS S4-04b, source locating; BC-AT27 字段定位; AN01; ADR-BR-010: parsing and locating are
// candidate generation in the AI layer). A model proposes lines with, for each field, the source's
// own words (the quote) and where it saw them (the locator); nothing it says is taken on trust
// (AI-INV-004, SD-07). For every field locator the extract is consulted:
//
//   - the quote stands at the locator: verified, kept (in the extract's own notation);
//   - it stands elsewhere, at exactly one place: relocated to that place;
//   - it stands at several places and the locator is not one of them: every place is kept (at most
//     MaxPlaces) and the field is pending ambiguous (SOURCE_AMBIGUOUS until a human verifies it);
//   - it stands nowhere: the locator is dropped and the field is pending unlocated;
//   - a place carries a reader's note that questions the value (a formula's cached value, a date
//     shown from a serial number, an error value, a hidden sheet, row or column, invisible text,
//     undecodable glyphs): the field is pending noted.
//
// A field with a value but no locator in a line whose source is a file extract is pending
// unlocated. Extracts are read through EVD as the caller, so access to the file is re-checked on
// every use (CT16). The files a draft's lines cite must keep the input profile together (at most 3
// files and 50,000 characters, baseline 14.2: INPUT_PROFILE_UNSUPPORTED).
package locate

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"go.idp.xyz/idp-brant/internal/ai/codes"
	"go.idp.xyz/idp-brant/internal/ai/ingest"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Extracts reads sources and the extracts of file_extract sources as the caller (EVD's
// evdapi.Queries and evdapi.ExtractQueries; both reads are audited at EVD).
type Extracts interface {
	GetSource(ctx context.Context, sourceID string) (evdapi.SourceView, error)
	GetExtract(ctx context.Context, sourceID string) (evdapi.ExtractView, error)
}

// MaxPlaces bounds the alternative places one field keeps: four fields of four places fill a
// line's MaxLineLocators.
const MaxPlaces = inqapi.MaxLineLocators / 4

// Statuses of a field locator.
const (
	Verified  = "verified"  // the quote stands at the locator
	Relocated = "relocated" // the quote stands at exactly one other place
	Ambiguous = "ambiguous" // the quote stands at several places, the locator is none of them
	Unlocated = "unlocated" // the quote stands nowhere, or the field has a value and no locator
)

// questioning are the note kinds that put a value's reading in question; the others (images,
// drawings, comments, external links, embedded objects, chart sheets) disclose what was not read.
var questioning = []string{ingest.NoteFormulaCachedValue, ingest.NoteDateFromSerial, ingest.NoteErrorValue, ingest.NoteHiddenSheet,
	ingest.NoteHiddenRow, ingest.NoteHiddenColumn, ingest.NoteInvisibleText, ingest.NoteUndecodableGlyphs}

// Canonical returns locator in the notation e writes ("sheet1!c3" → "sheet1!C3"), or false when it
// is not a locator of e's kind.
func Canonical(e *ingest.Extract, locator string) (string, bool) {
	switch e.Kind {
	case ingest.KindText:
		line, col, err := ingest.ParseTextLocator(locator)
		return ingest.TextLocator(line, col), err == nil
	case ingest.KindPDF:
		page, line, err := ingest.ParsePDFLocator(locator)
		return ingest.PDFLocator(page, line), err == nil
	case ingest.KindXLSX:
		sheet, row, col, err := ingest.ParseCellLocator(locator)
		return ingest.CellLocator(sheet, row, col), err == nil
	}
	return "", false
}

// Find returns the places quote stands at in e: hint alone when the quote stands there; otherwise
// every place it stands. Several places are SOURCE_AMBIGUOUS, returned with the places; no place
// is neither.
func Find(e *ingest.Extract, quote, hint string) ([]string, error) {
	if hint != "" {
		if c, ok := Canonical(e, hint); ok && e.Verify(c, quote) {
			return []string{c}, nil
		}
	}
	places := e.Find(quote)
	if len(places) > 1 {
		return places, kernel.RejectAs(codes.SourceAmbiguous, kernel.CategoryInvalid, "/quote",
			strconv.Itoa(len(places))+" places: "+strings.Join(places[:min(len(places), MaxPlaces)], ", "))
	}
	return places, nil
}

// Notes returns the kinds of the notes at place, its row, column or sheet (XLSX) that put the value
// there in question.
func Notes(e *ingest.Extract, place string) []string {
	at := []string{place}
	if e.Kind == ingest.KindXLSX {
		if sheet, row, col, err := ingest.ParseCellLocator(place); err == nil {
			prefix := ingest.SheetPrefix(sheet)
			r, c := strconv.Itoa(row), ingest.ColumnName(col)
			at = append(at, prefix+"!"+r+":"+r, prefix+"!"+c+":"+c, prefix)
		}
	}
	var out []string
	for _, n := range e.Notes {
		if slices.Contains(at, n.Locator) && slices.Contains(questioning, n.Kind) && !slices.Contains(out, n.Kind) {
			out = append(out, n.Kind)
		}
	}
	return out
}

// Finding is what the extract says about one field of a line.
type Finding struct {
	Line    int // the line's index
	LineID  string
	Field   string
	Locator string // as the line states it; "" for a field without a locator
	Quote   string
	Status  string   // verified, relocated, ambiguous or unlocated
	Places  []string // where the quote stands, in the extract's notation
	Notes   []string // note kinds at the places that question the value
}

// Locator checks and completes candidate lines against the extracts of their sources, under
// Profile (ingest.Q01Upload when zero).
type Locator struct {
	Extracts Extracts
	Profile  ingest.Profile
}

func (l Locator) profile() ingest.Profile {
	if l.Profile.ID == "" {
		return ingest.Q01Upload
	}
	return l.Profile
}

// Check returns, for every line whose source is a file extract the caller may read, a finding per
// field locator and per field with a value but no locator, in line order. Lines of other sources
// have no findings; a line with locators whose source is not a readable file extract is
// REFERENCE_INVALID (PERMISSION_DENIED without evd.read). It is the reviewer's verification read as
// well (BC-AT27: field locating verifiable), and it writes nothing.
func (l Locator) Check(ctx context.Context, lines []inqapi.RfqLine) ([]Finding, error) {
	extracts, err := l.extracts(ctx, lines)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for i, line := range lines {
		e := extracts[line.SourceID]
		if e == nil {
			continue
		}
		located := map[string]bool{}
		for _, x := range line.Locators {
			located[x.Field] = true
			f := Finding{Line: i, LineID: line.LineID, Field: x.Field, Locator: x.Locator, Quote: x.Quote}
			places, err := Find(e, x.Quote, x.Locator)
			f.Places = places
			switch {
			case len(places) == 0:
				f.Status = Unlocated
			case err != nil:
				f.Status = Ambiguous
			case len(places) == 1 && places[0] == canonicalOr(e, x.Locator):
				f.Status = Verified
			default:
				f.Status = Relocated
			}
			for _, p := range places {
				for _, n := range Notes(e, p) {
					if !slices.Contains(f.Notes, n) {
						f.Notes = append(f.Notes, n)
					}
				}
			}
			out = append(out, f)
		}
		for _, field := range inqapi.Fields {
			if !located[field] && value(line, field) != "" {
				out = append(out, Finding{Line: i, LineID: line.LineID, Field: field, Status: Unlocated})
			}
		}
	}
	return out, nil
}

// Lines returns the lines with their field locators verified and their pending fields completed
// from Check's findings: verified and relocated places kept, the places of an ambiguous field kept
// (at most MaxPlaces) with the field pending ambiguous, unlocated quotes dropped with the field
// pending unlocated, questioned places pending noted. A pending field the model named keeps its
// candidates, and its reason unless the extract gives a weightier one (ambiguous, conflict,
// unlocated, noted, missing, in that order). Lines of other sources are returned as they are.
func (l Locator) Lines(ctx context.Context, lines []inqapi.RfqLine) ([]inqapi.RfqLine, error) {
	if err := l.profile().CheckQuoteLines(len(lines)); err != nil {
		return nil, profileRefusal(err)
	}
	findings, err := l.Check(ctx, lines)
	if err != nil {
		return nil, err
	}
	out := slices.Clone(lines)
	byLine := map[int][]Finding{}
	for _, f := range findings {
		byLine[f.Line] = append(byLine[f.Line], f)
	}
	for i, fs := range byLine {
		out[i] = complete(lines[i], fs)
	}
	return out, nil
}

// complete rebuilds one line's locators and pending fields from its findings.
func complete(line inqapi.RfqLine, findings []Finding) inqapi.RfqLine {
	reasons := map[string]string{}
	places := map[string][]inqapi.FieldLocator{}
	var order []string
	raise := func(field, reason string) {
		if weight(reason) > weight(reasons[field]) {
			reasons[field] = reason
		}
	}
	for _, f := range findings {
		if !slices.Contains(order, f.Field) {
			order = append(order, f.Field)
		}
		switch f.Status {
		case Unlocated:
			raise(f.Field, inqapi.PendingUnlocated)
			continue
		case Ambiguous:
			raise(f.Field, inqapi.PendingAmbiguous)
		}
		if len(f.Notes) > 0 {
			raise(f.Field, inqapi.PendingNoted)
		}
		for _, p := range f.Places {
			x := inqapi.FieldLocator{Field: f.Field, Locator: p, Quote: f.Quote}
			if len(places[f.Field]) < MaxPlaces && !slices.ContainsFunc(places[f.Field], func(y inqapi.FieldLocator) bool { return y.Locator == p }) {
				places[f.Field] = append(places[f.Field], x)
			}
		}
		if len(places[f.Field]) > 1 {
			raise(f.Field, inqapi.PendingAmbiguous)
		}
	}
	line.Locators = nil
	for _, field := range order {
		line.Locators = append(line.Locators, places[field]...)
	}
	given := map[string]inqapi.PendingField{}
	for _, p := range line.Pending {
		given[p.Field] = p
		raise(p.Field, p.Reason)
	}
	line.Pending = nil
	for _, field := range inqapi.Fields {
		if reasons[field] == "" {
			continue
		}
		line.Pending = append(line.Pending, inqapi.PendingField{Field: field, Reason: reasons[field], Candidates: given[field].Candidates})
	}
	return line
}

// weight orders pending reasons: the weightiest one names a field's pending entry.
func weight(reason string) int {
	switch reason {
	case inqapi.PendingAmbiguous:
		return 5
	case inqapi.PendingConflict:
		return 4
	case inqapi.PendingUnlocated:
		return 3
	case inqapi.PendingNoted:
		return 2
	case inqapi.PendingMissing:
		return 1
	}
	return 0
}

// extracts reads the extract of every source a line cites, as the caller, and checks them as one
// task's files. A source that is not a file extract maps to nil unless a line places fields in it.
func (l Locator) extracts(ctx context.Context, lines []inqapi.RfqLine) (map[string]*ingest.Extract, error) {
	out := map[string]*ingest.Extract{}
	var bundle []*ingest.Extract
	for i, line := range lines {
		if line.SourceID == "" {
			continue
		}
		e, seen := out[line.SourceID]
		if !seen {
			var err error
			if e, err = l.read(ctx, line.SourceID); err != nil {
				return nil, err
			}
			out[line.SourceID] = e
			if e != nil {
				bundle = append(bundle, e)
			}
		}
		if e == nil && len(line.Locators) > 0 {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/Lines/"+strconv.Itoa(i)+"/SourceID",
				"Locators point into a file extract the caller may read")
		}
	}
	if len(bundle) > 0 {
		if err := l.profile().CheckBundle(bundle...); err != nil {
			return nil, profileRefusal(err)
		}
	}
	return out, nil
}

// read returns the extract of a file_extract source, nil for a source that is of another kind or
// not visible (NOT_FOUND), or the refusal (PERMISSION_DENIED without evd.read).
func (l Locator) read(ctx context.Context, sourceID string) (*ingest.Extract, error) {
	if l.Extracts == nil {
		return nil, nil
	}
	s, err := l.Extracts.GetSource(ctx, sourceID)
	if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeNotFound {
		return nil, nil
	}
	if err != nil || s.SourceKind != evdapi.KindFileExtract {
		return nil, err
	}
	v, err := l.Extracts.GetExtract(ctx, sourceID)
	if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeNotFound {
		return nil, nil // the file went out of sight between the two reads
	}
	if err != nil {
		return nil, err
	}
	return ingest.FromEVD(v)
}

func profileRefusal(err error) error {
	rej, ok := kernel.AsRejection(err)
	if !ok {
		return err
	}
	reason := ""
	if len(rej.Fields) > 0 {
		reason = rej.Fields[0].Reason
	}
	return kernel.RejectAs(rej.Code, rej.Category, "/args/Lines", reason)
}

func canonicalOr(e *ingest.Extract, locator string) string {
	if c, ok := Canonical(e, locator); ok {
		return c
	}
	return locator
}

func value(l inqapi.RfqLine, field string) string {
	switch field {
	case inqapi.FieldRequestedItem:
		return l.RequestedItem
	case inqapi.FieldSkuID:
		return l.SkuID
	case inqapi.FieldQuantity:
		return l.Quantity
	case inqapi.FieldUOM:
		return l.UOM
	}
	return ""
}
