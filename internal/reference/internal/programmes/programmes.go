// Package programmes holds the rules of country programme reference data (S5 A3b, E-3): the
// canonical form and content hash of a programme and of a version, and what an import may do to a
// list. It follows the screening lists of package lists: a version is imported once, a list keeps
// its jurisdiction, and a version is never older than the current one. It has no SQL and no I/O.
package programmes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Programme is an imported programme in canonical form: countries sorted, the content hash over
// the canonical content.
type Programme struct {
	ID        string
	Name      string
	Measure   string
	Countries []string
	Scope     string // empty: the source states none
	Hash      string
}

// NewProgramme returns the canonical programme.
func NewProgramme(id, name, measure string, countries []string, scope string) Programme {
	p := Programme{ID: id, Name: name, Measure: measure, Countries: slices.Sorted(slices.Values(countries)), Scope: scope}
	if p.Countries == nil {
		p.Countries = []string{}
	}
	p.Hash = p.contentHash()
	return p
}

// contentHash is the SHA-256 of the canonical JSON of the programme; field order is fixed by the
// type, countries are sorted and an absent scope is the empty string.
func (p Programme) contentHash() string {
	doc, _ := json.Marshal(struct {
		ProgrammeID string   `json:"programme_id"`
		Name        string   `json:"name"`
		Measure     string   `json:"measure"`
		Countries   []string `json:"countries"`
		Scope       string   `json:"scope"`
	}{p.ID, p.Name, p.Measure, p.Countries, p.Scope})
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:])
}

// ContentHash is the SHA-256 of a version's programmes: the canonical JSON array of
// [programme_id, content_hash] pairs ordered by programme ID.
func ContentHash(programmes []Programme) string {
	pairs := make([][2]string, len(programmes))
	for i, p := range programmes {
		pairs[i] = [2]string{p.ID, p.Hash}
	}
	slices.SortFunc(pairs, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	doc, _ := json.Marshal(pairs)
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:])
}

// Countries returns the distinct countries programmes name, sorted.
func Countries(programmes []Programme) []string {
	var out []string
	for _, p := range programmes {
		out = append(out, p.Countries...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// List is a country programme list of one company: its jurisdiction, which never changes, and its
// current version. RowVersion counts its imports.
type List struct {
	Code         string
	Jurisdiction string
	Current      string
	RowVersion   int64
}

// Version is one imported version of a list.
type Version struct {
	ListCode       string
	Version        string
	SourceName     string
	SourceURL      string
	PublishedOn    time.Time // a date at 00:00 UTC
	FileSHA256     string
	ContentHash    string
	ProgrammeCount int
	CountryCount   int
	ImportSeq      int64
	ImportedBy     kernel.ActorRef
	Epoch          int64
	ImportedAt     time.Time
}

// CodeBusinessKeyConflict is the code of a second import of a version; package api declares the
// same code for callers.
const CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"

// Import decides whether v may become the current version of list (nil for a list the company has
// never imported): a version is imported once (BUSINESS_KEY_CONFLICT), a list keeps its
// jurisdiction, and a version is not older than the current one by publication date. It returns the
// list after the import and sets v's import sequence.
func Import(list *List, jurisdiction string, exists bool, current *Version, v *Version) (*List, error) {
	switch {
	case exists:
		return nil, kernel.RejectAs(CodeBusinessKeyConflict, kernel.CategoryConflict, "/args/version",
			"This version of the programme list is already imported")
	case list != nil && list.Jurisdiction != jurisdiction:
		return nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/jurisdiction", "A programme list keeps its jurisdiction")
	case current != nil && v.PublishedOn.Before(current.PublishedOn):
		return nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/published_on", "A version older than the current one is not imported")
	}
	next := &List{Code: v.ListCode, Jurisdiction: jurisdiction}
	if list != nil {
		next.RowVersion = list.RowVersion
	}
	next.RowVersion++
	next.Current = v.Version
	v.ImportSeq = next.RowVersion
	return next, nil
}
