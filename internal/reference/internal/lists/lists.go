// Package lists holds the rules of screening list reference data: the name normalization matching
// uses (api.Normalization), the canonical form and content hash of an entry and of a version, and
// what an import may do to a list. It has no SQL and no I/O.
package lists

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode"

	"go.idp.xyz/idp-brant/internal/kernel"
	"golang.org/x/text/unicode/norm"
)

// MaxKeyBytes bounds a name key, so it always fits the database index.
const MaxKeyBytes = 1000

// NameKey returns the matching key of name under name-key-v1: NFKD; combining marks (category Mn)
// dropped; lower case with ß as ss; apostrophes (' ’ ʼ ‘ `) and full stops (. ． 。) removed; every
// other rune that is not a letter or a digit separates words; words joined by one space; NFC. A
// name without letters or digits has the empty key, which never matches.
func NameKey(name string) string {
	var b strings.Builder
	gap := false
	for _, r := range norm.NFKD.String(name) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case r == '\'' || r == '’' || r == 'ʼ' || r == '‘' || r == '`' || r == '.' || r == '。':
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if gap && b.Len() > 0 {
				b.WriteByte(' ')
			}
			gap = false
			if r = unicode.ToLower(r); r == 'ß' {
				b.WriteString("ss")
			} else {
				b.WriteRune(r)
			}
		default:
			gap = true
		}
	}
	return norm.NFC.String(b.String())
}

// Entry is an imported entry in canonical form: aliases sorted, every name's key computed, the
// content hash over the canonical content.
type Entry struct {
	ID          string
	PrimaryName string
	Aliases     []string
	EntityType  string
	Country     string
	Hash        string
	Keys        []Key // distinct keys of the primary name and the aliases, in that order
}

// Key is the key of one of an entry's names.
type Key struct {
	Key  string
	Name string
}

// ErrKeyTooLong reports a name whose key is longer than MaxKeyBytes.
type ErrKeyTooLong struct{ Name string }

func (e ErrKeyTooLong) Error() string { return "lists: the key of a name is too long" }

// NewEntry returns the canonical entry. Names the contract admitted always have a key.
func NewEntry(id, primary string, aliases []string, entityType, country string) (Entry, error) {
	e := Entry{ID: id, PrimaryName: primary, Aliases: slices.Sorted(slices.Values(aliases)), EntityType: entityType, Country: country}
	if e.Aliases == nil {
		e.Aliases = []string{}
	}
	seen := map[string]bool{}
	for _, n := range append([]string{e.PrimaryName}, e.Aliases...) {
		k := NameKey(n)
		if len(k) > MaxKeyBytes {
			return Entry{}, ErrKeyTooLong{Name: n}
		}
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		e.Keys = append(e.Keys, Key{Key: k, Name: n})
	}
	e.Hash = e.contentHash()
	return e, nil
}

// contentHash is the SHA-256 of the canonical JSON of the entry; field order is fixed by the type.
func (e Entry) contentHash() string {
	doc, _ := json.Marshal(struct {
		EntryID     string   `json:"entry_id"`
		PrimaryName string   `json:"primary_name"`
		Aliases     []string `json:"aliases"`
		EntityType  string   `json:"entity_type"`
		Country     string   `json:"country"`
	}{e.ID, e.PrimaryName, e.Aliases, e.EntityType, e.Country})
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:])
}

// ContentHash is the SHA-256 of a version's entries: the canonical JSON array of [entry_id,
// content_hash] pairs ordered by entry ID.
func ContentHash(entries []Entry) string {
	pairs := make([][2]string, len(entries))
	for i, e := range entries {
		pairs[i] = [2]string{e.ID, e.Hash}
	}
	slices.SortFunc(pairs, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	doc, _ := json.Marshal(pairs)
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:])
}

// List is a screening list of one company: its jurisdiction, which never changes, and its current
// version. RowVersion counts its imports.
type List struct {
	Code         string
	Jurisdiction string
	Current      string
	RowVersion   int64
}

// Version is one imported version of a list.
type Version struct {
	ListCode    string
	Version     string
	SourceName  string
	SourceURL   string
	PublishedOn time.Time // a date at 00:00 UTC
	FileSHA256  string
	ContentHash string
	EntryCount  int
	ImportSeq   int64
	ImportedBy  kernel.ActorRef
	Epoch       int64
	ImportedAt  time.Time
}

// Codes of the rules below; package api declares the same codes for callers.
const CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"

// Import decides whether v may become the current version of list (nil for a list the company
// has never imported): a version is imported once (BUSINESS_KEY_CONFLICT), a list keeps its
// jurisdiction, and a version is not older than the current one by publication date. It returns
// the list after the import.
func Import(list *List, jurisdiction string, exists bool, current *Version, v *Version) (*List, error) {
	switch {
	case exists:
		return nil, kernel.RejectAs(CodeBusinessKeyConflict, kernel.CategoryConflict, "/args/version", "This version of the list is already imported")
	case list != nil && list.Jurisdiction != jurisdiction:
		return nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/jurisdiction", "A list keeps its jurisdiction")
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
