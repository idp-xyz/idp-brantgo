// Package api is the contract of Brant's reference data for transports and for the bounded
// contexts that use it. Reference data is not a bounded context: it has no upstream, it holds
// facts imported under control from outside sources, and contexts read it only through this
// package (dependency rule D1).
//
// Screening lists (S1-11, ADR-BR-017) are imported per company: each company imports the list
// versions it uses, with their source, publication date, import time, importer, the SHA-256 of the
// source file and the entry count. Imported entries never change; a new version of a list becomes
// its current version, and the contexts that screened against the previous one see that through
// the list guard (ScreeningListGuardFunction). Country programme lists (S5 E-3) follow the same
// pattern; see programmes.go.
package api

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	// Contract names the request and response shapes of reference data commands.
	Contract = "brant.ref/0.1.0"
	// Source identifies reference data as the producer of integration events.
	Source = "idp-brant.ref"

	EventScreeningListImported = "idp-brant.ref.screening_list_imported.v1"

	// CommandImportScreeningList imports one version of a screening list (human only). The role
	// template that holds it is compliance: the compliance lead confirms list sources (S1-09).
	CommandImportScreeningList = "screening_list.import"

	StatusImported = "imported"
)

// Jurisdictions of a screening list (ADR-BR-017): China's lists, the UN Security Council
// consolidated list, and foreign lists, which a tenant enables only with its legal confirmation.
const (
	JurisdictionCN      = "CN"
	JurisdictionUN      = "UN"
	JurisdictionForeign = "foreign"
)

// Entity types of a list entry.
const (
	EntityTypeEntity     = "entity"
	EntityTypeIndividual = "individual"
	EntityTypeVessel     = "vessel"
	EntityTypeAircraft   = "aircraft"
	EntityTypeOther      = "other"
)

// Limits of the contract.
const (
	// MaxListCodeLength and MaxVersionLength keep "<list_code>:<version>" a 64-character identifier.
	MaxListCodeLength = 32
	MaxVersionLength  = 31
	MaxEntries        = 50000
	MaxAliases        = 100
	MaxNameCharacters = 500
	MaxMatchVersions  = 100
	MaxMatchNames     = 20

	maxSourceNameCharacters = 200
	maxSourceURLCharacters  = 2000
)

// Normalization names the matching rule Match applies (name-key-v1): Unicode compatibility
// decomposition (NFKD); combining marks dropped, so diacritics do not matter; lower case, with ß as
// ss; apostrophes and full stops removed; every other character that is not a letter or a digit
// separates words; words joined by one space; the result recomposed (NFC). Two names match when
// their keys are equal. There is no fuzzy, phonetic, transliteration or AI matching: a name in
// another script or with other words does not match. Screening conclusions record this name.
const Normalization = "name-key-v1"

// ScreeningListGuardFunction is the SQL function reference data publishes for guard functions of
// downstream contexts (ADR-BR-004 option B):
//
//	brant_ref.screening_list_current_v1(p_list_code text, p_lock boolean) RETURNS text
//
// It returns the current version of the list in the scope of app.tenant_id and app.company_id, or
// NULL when the company has imported none. With p_lock it holds the list row FOR SHARE until the
// caller's transaction ends, so an import of a new version either committed before or waits. It is
// SECURITY DEFINER with a fixed search_path; brant_app may execute it.
const (
	ScreeningListGuardFunction = "brant_ref.screening_list_current_v1"
	ScreeningListGuardSQL      = "SELECT brant_ref.screening_list_current_v1($1, true)"
)

// CodeBusinessKeyConflict refuses a second import of a list version (error catalog V0.1).
const CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"

var (
	jurisdictions = []string{JurisdictionCN, JurisdictionUN, JurisdictionForeign}
	entityTypes   = []string{EntityTypeEntity, EntityTypeIndividual, EntityTypeVessel, EntityTypeAircraft, EntityTypeOther}
)

// ImportScreeningList imports one version of a screening list as a structured list of entries
// (no PDF or spreadsheet parsing). FileSHA256 is the SHA-256 of the source file the entries were
// taken from; EntryCount is the number of entries that file holds and must equal len(Entries).
// PublishedOn is the source's publication date ("2026-11-01"). The importer and the import time
// come from the verified caller and the clock, never from the request.
type ImportScreeningList struct {
	IdempotencyKey string
	ListCode       string
	Jurisdiction   string
	Version        string
	SourceName     string
	SourceURL      string
	PublishedOn    string
	FileSHA256     string
	EntryCount     int
	Entries        []ScreeningEntry
}

// ScreeningEntry is one entry of a list as its source publishes it. EntryID is the source's
// identifier of the entry; Country is an ISO 3166-1 alpha-2 code or empty.
type ScreeningEntry struct {
	EntryID     string
	PrimaryName string
	Aliases     []string
	EntityType  string
	Country     string
}

// Validate checks the closed contract.
func (c ImportScreeningList) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !ValidListCode(c.ListCode):
		return schema("/args/list_code")
	case !slices.Contains(jurisdictions, c.Jurisdiction):
		return schema("/args/jurisdiction")
	case !ValidVersion(c.Version):
		return schema("/args/version")
	case !text(c.SourceName, maxSourceNameCharacters):
		return schema("/args/source_name")
	case !sourceURL(c.SourceURL):
		return schema("/args/source_url")
	case !date(c.PublishedOn):
		return schema("/args/published_on")
	case !sha256Hex(c.FileSHA256):
		return schema("/args/file_sha256")
	case c.EntryCount < 1 || c.EntryCount > MaxEntries:
		return schema("/args/entry_count")
	case len(c.Entries) != c.EntryCount:
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/entry_count", "The entries do not match the declared count")
	}
	seen := make(map[string]bool, len(c.Entries))
	for i, e := range c.Entries {
		at := "/args/entries/" + strconv.Itoa(i)
		switch {
		case !kernel.ValidIdentifier(e.EntryID) || seen[e.EntryID]:
			return schema(at + "/entry_id")
		case !Name(e.PrimaryName):
			return schema(at + "/primary_name")
		case len(e.Aliases) > MaxAliases:
			return schema(at + "/aliases")
		case !slices.Contains(entityTypes, e.EntityType):
			return schema(at + "/entity_type")
		case e.Country != "" && !countryCode(e.Country):
			return schema(at + "/country")
		}
		seen[e.EntryID] = true
		for j, a := range e.Aliases {
			if !Name(a) || slices.Index(e.Aliases, a) != j {
				return schema(at + "/aliases/" + strconv.Itoa(j))
			}
		}
	}
	return nil
}

// Receipt is the closed success answer of a command. ObjectID is "<list_code>:<version>",
// RowVersion the list's version counter after the import.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Commands is the command entry point of reference data. Errors are *kernel.Rejection for refusals.
type Commands interface {
	ImportScreeningList(ctx context.Context, cmd ImportScreeningList) (Receipt, error)
}

// Auditor records a request an inbound adapter refused before it became a command.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// ListVersion is one imported version of a screening list. ContentHash is the SHA-256 Brant
// computed over the canonical entries; FileSHA256 the declared hash of the source file.
type ListVersion struct {
	ListCode     string
	Jurisdiction string
	Version      string
	SourceName   string
	SourceURL    string
	PublishedOn  string
	FileSHA256   string
	ContentHash  string
	EntryCount   int
	ImportSeq    int64 // 1 for the list's first import, then +1 per import
	ImportedAt   time.Time
	ImportedBy   kernel.ActorRef
	Current      bool
}

// ListVersionRef names one version of one list.
type ListVersionRef struct {
	ListCode string
	Version  string
}

// Entry is an imported entry. ContentHash is the SHA-256 of its canonical content (identifier,
// names with aliases sorted, entity type and country); it changes when the source changes the entry.
type Entry struct {
	EntryID     string
	PrimaryName string
	Aliases     []string // sorted
	EntityType  string
	Country     string
	ContentHash string
}

// Hit is an entry of a list version one of the queried names matches under Normalization.
type Hit struct {
	ListCode    string
	Version     string
	Entry       Entry
	QueryName   string // the queried name
	MatchedName string // the entry's primary name or alias whose key equals the queried name's
	NameKey     string
}

// ListView is a list and every version the company imported, oldest first.
type ListView struct {
	ListCode       string
	Jurisdiction   string
	CurrentVersion string
	RowVersion     int64
	Versions       []ListVersion
}

// ListVersionView is one version with its entries, ordered by entry ID.
type ListVersionView struct {
	ListVersion
	Entries []Entry
}

// Queries is the read side of reference data.
type Queries interface {
	// CurrentVersions answers lookups of downstream contexts: the current version of each list
	// code the company imported; codes without one are absent. It needs no permission beyond an
	// active grant. Ask before opening your own transaction.
	CurrentVersions(ctx context.Context, listCodes []string) (map[string]ListVersion, error)
	// Match returns the entries of the given list versions that one of names matches under
	// Normalization, ordered by list, entry and name. It needs no permission beyond an active
	// grant. Ask before opening your own transaction.
	Match(ctx context.Context, versions []ListVersionRef, names []string) ([]Hit, error)
	// GetList returns a list and its versions, audited as screening_list.get.
	GetList(ctx context.Context, listCode string) (ListView, error)
	// GetListVersion returns one version and its entries, audited as screening_list.get.
	GetListVersion(ctx context.Context, listCode, version string) (ListVersionView, error)
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
}

// ObjectID is how receipts, the audit trail and events name a list version.
func ObjectID(listCode, version string) string { return listCode + ":" + version }

// ValidListCode reports whether code is an identifier of at most MaxListCodeLength characters.
func ValidListCode(code string) bool {
	return len(code) <= MaxListCodeLength && kernel.ValidIdentifier(code)
}

// ValidVersion reports whether v is an identifier of at most MaxVersionLength characters.
func ValidVersion(v string) bool { return len(v) <= MaxVersionLength && kernel.ValidIdentifier(v) }

// ValidJurisdiction reports whether j is CN, UN or foreign.
func ValidJurisdiction(j string) bool { return slices.Contains(jurisdictions, j) }

// Name reports whether s is a name that can be matched: valid UTF-8 of 1 to MaxNameCharacters
// characters with at least one letter or digit, so its key is never empty.
func Name(s string) bool {
	return text(s, MaxNameCharacters) && strings.IndexFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

func text(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s) && strings.TrimSpace(s) != ""
}

func sourceURL(s string) bool {
	if len(s) > maxSourceURLCharacters || !utf8.ValidString(s) {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil
}

func date(s string) bool {
	t, err := time.Parse(time.DateOnly, s)
	return err == nil && t.Format(time.DateOnly) == s
}

func sha256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && (s[i] < 'a' || s[i] > 'f') {
			return false
		}
	}
	return true
}

func countryCode(s string) bool {
	return len(s) == 2 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

// Reject returns a rejection with a code reference data declares.
func Reject(code, path, reason string) *kernel.Rejection {
	if code != CodeBusinessKeyConflict {
		panic("ref: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, kernel.CategoryConflict, path, reason)
}
