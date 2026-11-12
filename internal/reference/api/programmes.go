package api

import (
	"context"
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Country programmes (S5 A3b; owner decision E-3 of 2026-11-12, ADR-BR-017; UL CNX-EmbargoCheck):
// the embargo, sanctions and export-control programmes that name countries, which CNX's trade
// compliance check compares with the destination country and the parties' countries of an order
// revision (CNX-INV-001, D-8). A programme list is imported per company like a screening list: each
// version with its source, publication date, import time, importer, the SHA-256 of the source file
// and the declared programme and country counts. Imported programmes never change; a new version
// becomes the list's current version, and a check that used the previous one sees that through the
// list guard (CountryProgrammeListGuardFunction).
//
// Reference data records what a programme says, not whether it applies: a programme naming a
// country is a candidate finding for a human compliance decision, never a conclusion. Which lists a
// company uses is its published screening_regulation's to say (ADR-BR-017: CN and UN lists by
// default, a foreign list only with the tenant's legal confirmation); importing a list enables
// nothing. S5 uses synthetic CN and UN lists only.
const (
	EventCountryProgrammesImported = "idp-brant.ref.country_programmes_imported.v1"

	// CommandImportCountryProgrammes imports one version of a country programme list (human only).
	// The role template that holds it is compliance, exclusively (ADR-BR-017: the compliance lead
	// confirms programme sources).
	CommandImportCountryProgrammes = "country_programme.import"
)

// Measures a programme imposes, as its source classifies them.
const (
	MeasureEmbargo       = "embargo"        // a comprehensive embargo on trade with the country
	MeasureArmsEmbargo   = "arms_embargo"   // arms and related materiel
	MeasureSanctions     = "sanctions"      // sectoral or targeted restrictive measures
	MeasureExportControl = "export_control" // a licence requirement or ban for controlled goods
)

// Limits of the programme contract.
const (
	MaxProgrammes              = 500
	MaxProgrammeCountries      = 249 // every assigned ISO 3166-1 alpha-2 code
	MaxProgrammeScope          = 2000
	MaxProgrammeLists          = 100 // lists one ResolveCountryProgrammes call may name
	MaxProgrammeQueryCountries = 50  // countries one ResolveCountryProgrammes call may name
)

// CountryProgrammeListGuardFunction is the SQL function reference data publishes for the guard
// functions and transactions of downstream contexts (ADR-BR-004 option B):
//
//	brant_ref.country_programme_list_current_v1(p_list_code text, p_lock boolean) RETURNS text
//
// It returns the current version of the programme list in the scope of app.tenant_id and
// app.company_id, or NULL when the company has imported none. With p_lock it holds the list row
// FOR SHARE until the caller's transaction ends, so an import of a new version either committed
// before or waits. It is SECURITY DEFINER with a fixed search_path; brant_app may execute it.
const (
	CountryProgrammeListGuardFunction = "brant_ref.country_programme_list_current_v1"
	CountryProgrammeListGuardSQL      = "SELECT brant_ref.country_programme_list_current_v1($1, true)"
)

var measures = []string{MeasureEmbargo, MeasureArmsEmbargo, MeasureSanctions, MeasureExportControl}

// ImportCountryProgrammes imports one version of a country programme list as a structured list of
// programmes (no PDF or spreadsheet parsing). FileSHA256 is the SHA-256 of the source file the
// programmes were taken from; ProgrammeCount is the number of programmes that file holds and must
// equal len(Programmes); CountryCount is the number of distinct countries its programmes name and
// must equal that number in Programmes. PublishedOn is the source's publication date
// ("2026-11-01"). The importer and the import time come from the verified caller and the clock,
// never from the request.
type ImportCountryProgrammes struct {
	IdempotencyKey string
	ListCode       string
	Jurisdiction   string
	Version        string
	SourceName     string
	SourceURL      string
	PublishedOn    string
	FileSHA256     string
	ProgrammeCount int
	CountryCount   int
	Programmes     []CountryProgramme
}

// CountryProgramme is one programme of a list as its source publishes it. ProgrammeID is the
// source's identifier of the programme; Countries are the officially assigned ISO 3166-1 alpha-2
// codes (kernel.ValidCountryCode) of the countries it names, each once, in any order; Scope is
// optional: what the programme restricts (goods, sectors, end uses) in the source's words.
type CountryProgramme struct {
	ProgrammeID string
	Name        string
	Measure     string
	Countries   []string
	Scope       string
}

// Validate checks the closed contract.
func (c ImportCountryProgrammes) Validate() error {
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
	case c.ProgrammeCount < 1 || c.ProgrammeCount > MaxProgrammes:
		return schema("/args/programme_count")
	case c.CountryCount < 1 || c.CountryCount > MaxProgrammeCountries:
		return schema("/args/country_count")
	case len(c.Programmes) != c.ProgrammeCount:
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/programme_count", "The programmes do not match the declared count")
	}
	ids, countries := make(map[string]bool, len(c.Programmes)), map[string]bool{}
	for i, p := range c.Programmes {
		at := "/args/programmes/" + strconv.Itoa(i)
		switch {
		case !kernel.ValidIdentifier(p.ProgrammeID) || ids[p.ProgrammeID]:
			return schema(at + "/programme_id")
		case !Name(p.Name):
			return schema(at + "/name")
		case !slices.Contains(measures, p.Measure):
			return schema(at + "/measure")
		case len(p.Countries) < 1 || len(p.Countries) > MaxProgrammeCountries:
			return schema(at + "/countries")
		case p.Scope != "" && !text(p.Scope, MaxProgrammeScope):
			return schema(at + "/scope")
		}
		ids[p.ProgrammeID] = true
		for j, country := range p.Countries {
			if !kernel.ValidCountryCode(country) || slices.Index(p.Countries, country) != j {
				return schema(at + "/countries/" + strconv.Itoa(j))
			}
			countries[country] = true
		}
	}
	if len(countries) != c.CountryCount {
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/country_count", "The programmes do not name the declared number of countries")
	}
	return nil
}

// ValidMeasure reports whether m is one of the measures a programme imposes.
func ValidMeasure(m string) bool { return slices.Contains(measures, m) }

// ProgrammeCommands is the command entry point of country programme reference data. Errors are
// *kernel.Rejection for refusals. The receipt's ObjectID is "<list_code>:<version>", RowVersion the
// list's version counter after the import.
type ProgrammeCommands interface {
	ImportCountryProgrammes(ctx context.Context, cmd ImportCountryProgrammes) (Receipt, error)
}

// ProgrammeListVersion is one imported version of a country programme list. ContentHash is the
// SHA-256 Brant computed over the canonical programmes; FileSHA256 the declared hash of the source
// file.
type ProgrammeListVersion struct {
	ListCode       string
	Jurisdiction   string
	Version        string
	SourceName     string
	SourceURL      string
	PublishedOn    string
	FileSHA256     string
	ContentHash    string
	ProgrammeCount int
	CountryCount   int
	ImportSeq      int64 // 1 for the list's first import, then +1 per import
	ImportedAt     time.Time
	ImportedBy     kernel.ActorRef
	Current        bool
}

// Programme is an imported programme. Countries are sorted. ContentHash is the SHA-256 of its
// canonical content (identifier, name, measure, sorted countries, scope); it changes when the
// source changes the programme.
type Programme struct {
	ProgrammeID string
	Name        string
	Measure     string
	Countries   []string
	Scope       string
	ContentHash string
}

// ProgrammeListView is a country programme list and every version the company imported, oldest
// first.
type ProgrammeListView struct {
	ListCode       string
	Jurisdiction   string
	CurrentVersion string
	RowVersion     int64
	Versions       []ProgrammeListVersion
}

// ProgrammeListVersionView is one version with its programmes, ordered by programme ID.
type ProgrammeListVersionView struct {
	ProgrammeListVersion
	Programmes []Programme
}

// ResolveProgrammes asks which programmes name countries. Each list is a ListVersionRef: an empty
// Version asks for the list's current version, a stated one for exactly that version. Countries are
// ISO 3166-1 alpha-2 codes.
type ResolveProgrammes struct {
	Lists     []ListVersionRef
	Countries []string
}

// CountryProgrammeHit is a programme of a list version that names one of the queried countries,
// once per (programme, country).
type CountryProgrammeHit struct {
	ListCode     string
	Jurisdiction string
	Version      string
	Country      string // the queried country the programme names
	Programme    Programme
}

// ResolvedProgrammes answers ResolveProgrammes. Versions holds, per list code, the version the
// hits come from; a list the company has not imported, or whose stated version it has not
// imported, is absent and contributes no hit, so a caller cannot take its absence for a clear
// check. Hits are ordered by list code, programme ID and country.
type ResolvedProgrammes struct {
	Versions map[string]ProgrammeListVersion
	Hits     []CountryProgrammeHit
}

// ProgrammeQueries is the read side of country programme reference data.
type ProgrammeQueries interface {
	// ResolveCountryProgrammes answers the embargo lookups of downstream contexts (CNX's trade
	// compliance check, UL CNX-EmbargoCheck): the versions asked for and every programme of them
	// that names one of the countries, read in one snapshot. It needs no permission beyond an
	// active grant. Ask before opening your own transaction; inside it, hold each list you used
	// with CountryProgrammeListGuardSQL and compare its current version with the one you used.
	ResolveCountryProgrammes(ctx context.Context, q ResolveProgrammes) (ResolvedProgrammes, error)
	// GetProgrammeList returns a programme list and its versions, audited as
	// country_programme_list.get.
	GetProgrammeList(ctx context.Context, listCode string) (ProgrammeListView, error)
	// GetProgrammeListVersion returns one version and its programmes, audited as
	// country_programme_list.get.
	GetProgrammeListVersion(ctx context.Context, listCode, version string) (ProgrammeListVersionView, error)
}

// Validate checks the size and form of a lookup.
func (q ResolveProgrammes) Validate() error {
	if len(q.Lists) > MaxProgrammeLists {
		return schema("/lists")
	}
	if len(q.Countries) > MaxProgrammeQueryCountries {
		return schema("/countries")
	}
	for i, l := range q.Lists {
		if !ValidListCode(l.ListCode) || (l.Version != "" && !ValidVersion(l.Version)) ||
			slices.IndexFunc(q.Lists, func(o ListVersionRef) bool { return o.ListCode == l.ListCode }) != i {
			return schema("/lists/" + strconv.Itoa(i))
		}
	}
	for i, c := range q.Countries {
		if !kernel.ValidCountryCode(c) {
			return schema("/countries/" + strconv.Itoa(i))
		}
	}
	return nil
}
