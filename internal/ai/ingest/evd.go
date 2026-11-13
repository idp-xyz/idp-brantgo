package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"

	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
)

// EVD is the extractor EVD registers file_extract sources with (evdapi.Extractor, WBS S4-04b):
// bootstrap gives it to EVD, which holds no parsing code (Context Map EVD boundary; ADR-BR-010).
// It reads a business file under Profile; a file outside the profile is the refusal
// INPUT_PROFILE_UNSUPPORTED with the limit in its reason, never a partial extract.
type EVD struct{ Profile Profile }

var _ evdapi.Extractor = EVD{}

// Extract reads content of mimeType into EVD's form of an extract.
func (x EVD) Extract(mimeType string, content []byte) (evdapi.FileExtract, error) {
	e, err := x.Profile.Extract(mimeType, content)
	if err != nil {
		var u *Unsupported
		if errors.As(err, &u) {
			return evdapi.FileExtract{}, u.Rejection()
		}
		return evdapi.FileExtract{}, err
	}
	return evdapi.FileExtract{ExtractRef: e.Ref(), Text: e.Text(), LocatedForm: string(e.Canonical())}, nil
}

// Ref is the extract's identity in EVD's terms (the file ID is EVD's to fill).
func (e *Extract) Ref() evdapi.ExtractRef {
	return evdapi.ExtractRef{FileSHA256: e.FileSHA256, Kind: string(e.Kind), Extractor: e.Extractor, Profile: e.Profile,
		Characters: e.Characters, Pages: e.Pages, Sheets: slices.Clone(e.Sheets), SHA256: e.SHA256}
}

// ErrLocatedForm is a located form that is not one Canonical writes, or that does not match what
// it claims to be.
var ErrLocatedForm = errors.New("ingest: not a located form of this extract")

// FromEVD reads the extract of a file_extract source back from EVD's view (ExtractQueries
// .GetExtract), so that its locators can be resolved (At, Verify, Find). The located form must be
// exactly one Canonical writes, and agree with the view: SHA-256, file, kind, extractor, profile.
func FromEVD(v evdapi.ExtractView) (*Extract, error) {
	e, err := ParseCanonical([]byte(v.LocatedForm))
	if err != nil {
		return nil, err
	}
	if e.Ref().SHA256 != v.SHA256 || e.FileSHA256 != v.FileSHA256 || string(e.Kind) != v.Kind || e.Extractor != v.Extractor ||
		e.Profile != v.Profile || e.Characters != v.Characters || e.Pages != v.Pages || !slices.Equal(e.Sheets, v.Sheets) {
		return nil, ErrLocatedForm
	}
	return e, nil
}

// ParseCanonical reads a located form (Canonical's output) into an extract and seals it: the
// inverse of Canonical. It refuses anything Canonical would not write byte for byte, so the
// SHA-256 of the result is that of form.
func ParseCanonical(form []byte) (*Extract, error) {
	text, ok := strings.CutSuffix(string(form), "\n")
	if !ok {
		return nil, ErrLocatedForm
	}
	lines := strings.Split(text, "\n")
	if lines[0] != canonicalHeader {
		return nil, ErrLocatedForm
	}
	e := &Extract{}
	header := map[string]*string{}
	var kind, characters, pages string
	header["kind"], header["extractor"], header["profile"], header["file_sha256"], header["characters"] =
		&kind, &e.Extractor, &e.Profile, &e.FileSHA256, &characters
	header["pages"] = &pages
	for _, line := range lines[1:] {
		fields := strings.Split(line, "\t")
		for i, f := range fields {
			v, ok := unescape(f)
			if !ok {
				return nil, ErrLocatedForm
			}
			fields[i] = v
		}
		switch {
		case fields[0] == "sheet" && len(fields) == 2:
			e.Sheets = append(e.Sheets, fields[1])
		case fields[0] == "segment" && len(fields) == 3:
			e.Segments = append(e.Segments, Segment{Locator: fields[1], Text: fields[2]})
		case fields[0] == "note" && len(fields) == 3:
			e.Notes = append(e.Notes, Note{Kind: fields[1], Locator: fields[2]})
		case header[fields[0]] != nil && len(fields) == 2:
			*header[fields[0]] = fields[1]
		default:
			return nil, ErrLocatedForm
		}
	}
	e.Kind = Kind(kind)
	var err1, err2 error
	e.Characters, err1 = strconv.Atoi(characters)
	if e.Kind == KindPDF {
		e.Pages, err2 = strconv.Atoi(pages)
	}
	if err1 != nil || err2 != nil {
		return nil, ErrLocatedForm
	}
	claimed := e.Characters
	e.seal()
	if e.Characters != claimed || !bytes.Equal(e.Canonical(), form) {
		return nil, ErrLocatedForm
	}
	sum := sha256.Sum256(form)
	if e.SHA256 != hex.EncodeToString(sum[:]) {
		return nil, ErrLocatedForm
	}
	return e, nil
}

// unescape undoes escape; a backslash before anything but a backslash, t, n or r, a trailing
// backslash, or a raw tab, line feed or carriage return is not Canonical's.
func unescape(s string) (string, bool) {
	if !strings.ContainsAny(s, "\\\t\n\r") {
		return s, true
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\t', '\n', '\r':
			return "", false
		case '\\':
			if i+1 == len(s) {
				return "", false
			}
			i++
			switch s[i] {
			case '\\':
				b.WriteByte('\\')
			case 't':
				b.WriteByte('\t')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			default:
				return "", false
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}
