package api

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func programmeImport() ImportCountryProgrammes {
	return ImportCountryProgrammes{IdempotencyKey: "k1", ListCode: "cn.country_programmes", Jurisdiction: JurisdictionCN,
		Version: "2026-11-01", SourceName: "Synthetic CN country programmes", SourceURL: "https://lists.example.org/cn.country_programmes",
		PublishedOn: "2026-11-01", FileSHA256: strings.Repeat("cd", 32), ProgrammeCount: 2, CountryCount: 3,
		Programmes: []CountryProgramme{
			{ProgrammeID: "SYN-CN-EMB-1", Name: "Synthetic embargo programme", Measure: MeasureEmbargo, Countries: []string{"BV", "AQ"}},
			{ProgrammeID: "SYN-CN-EXP-1", Name: "Synthetic export control programme", Measure: MeasureExportControl, Countries: []string{"HM", "AQ"},
				Scope: "Synthetic controlled goods"},
		}}
}

// S5 E-3 (role template country_programme.import: source, publication date, file SHA-256, country
// and programme counts): a programme list version is imported as a structured list whose declared
// counts match its content; countries are assigned ISO 3166-1 alpha-2 codes.
func TestProgrammeImportContract(t *testing.T) {
	for _, c := range []struct {
		edit func(*ImportCountryProgrammes)
		want string
	}{
		{func(*ImportCountryProgrammes) {}, "OK"},
		{func(c *ImportCountryProgrammes) { c.Jurisdiction = JurisdictionForeign }, "OK"}, // importing enables nothing (ADR-BR-017)
		{func(c *ImportCountryProgrammes) { c.IdempotencyKey = "" }, "SCHEMA_INVALID /idempotency_key"},
		{func(c *ImportCountryProgrammes) { c.ListCode = strings.Repeat("x", MaxListCodeLength+1) }, "SCHEMA_INVALID /args/list_code"},
		{func(c *ImportCountryProgrammes) { c.Jurisdiction = "EU" }, "SCHEMA_INVALID /args/jurisdiction"},
		{func(c *ImportCountryProgrammes) { c.Version = "v 1" }, "SCHEMA_INVALID /args/version"},
		{func(c *ImportCountryProgrammes) { c.SourceName = "" }, "SCHEMA_INVALID /args/source_name"},
		{func(c *ImportCountryProgrammes) { c.SourceURL = "file:///tmp/list" }, "SCHEMA_INVALID /args/source_url"},
		{func(c *ImportCountryProgrammes) { c.PublishedOn = "2026-02-30" }, "SCHEMA_INVALID /args/published_on"},
		{func(c *ImportCountryProgrammes) { c.FileSHA256 = "cd" }, "SCHEMA_INVALID /args/file_sha256"},
		{func(c *ImportCountryProgrammes) { c.ProgrammeCount, c.Programmes = 0, nil }, "SCHEMA_INVALID /args/programme_count"},
		{func(c *ImportCountryProgrammes) { c.ProgrammeCount = MaxProgrammes + 1 }, "SCHEMA_INVALID /args/programme_count"},
		{func(c *ImportCountryProgrammes) { c.CountryCount = 0 }, "SCHEMA_INVALID /args/country_count"},
		{func(c *ImportCountryProgrammes) { c.ProgrammeCount = 3 }, "INVALID_KNOWN_VALUE /args/programme_count"},
		{func(c *ImportCountryProgrammes) { c.CountryCount = 4 }, "INVALID_KNOWN_VALUE /args/country_count"},
		{func(c *ImportCountryProgrammes) { c.Programmes[1].ProgrammeID = "SYN-CN-EMB-1" }, "SCHEMA_INVALID /args/programmes/1/programme_id"},
		{func(c *ImportCountryProgrammes) { c.Programmes[0].ProgrammeID = "-x" }, "SCHEMA_INVALID /args/programmes/0/programme_id"},
		{func(c *ImportCountryProgrammes) { c.Programmes[0].Name = "--" }, "SCHEMA_INVALID /args/programmes/0/name"},
		{func(c *ImportCountryProgrammes) { c.Programmes[0].Measure = "boycott" }, "SCHEMA_INVALID /args/programmes/0/measure"},
		{func(c *ImportCountryProgrammes) { c.Programmes[0].Countries = nil }, "SCHEMA_INVALID /args/programmes/0/countries"},
		{func(c *ImportCountryProgrammes) { c.Programmes[0].Countries = []string{"AQ", "AQ"} }, "SCHEMA_INVALID /args/programmes/0/countries/1"},
		{func(c *ImportCountryProgrammes) { c.Programmes[0].Countries = []string{"aq"} }, "SCHEMA_INVALID /args/programmes/0/countries/0"},
		{func(c *ImportCountryProgrammes) { c.Programmes[1].Countries = []string{"HM", "XK"} }, "SCHEMA_INVALID /args/programmes/1/countries/1"},
		{func(c *ImportCountryProgrammes) { c.Programmes[1].Scope = " " }, "SCHEMA_INVALID /args/programmes/1/scope"},
		{func(c *ImportCountryProgrammes) { c.Programmes[1].Scope = strings.Repeat("s", MaxProgrammeScope+1) }, "SCHEMA_INVALID /args/programmes/1/scope"},
	} {
		cmd := programmeImport()
		cmd.Programmes = append([]CountryProgramme(nil), cmd.Programmes...)
		c.edit(&cmd)
		if got := code(cmd.Validate()); got != c.want {
			t.Errorf("%s: want %s", got, c.want)
		}
	}
	all := make([]string, 0, MaxProgrammeCountries)
	for a := 'A'; a <= 'Z'; a++ {
		for b := 'A'; b <= 'Z'; b++ {
			if cc := string([]rune{a, b}); kernel.ValidCountryCode(cc) {
				all = append(all, cc)
			}
		}
	}
	cmd := programmeImport()
	cmd.ProgrammeCount, cmd.CountryCount = 1, len(all)
	cmd.Programmes = []CountryProgramme{{ProgrammeID: "ALL", Name: "Every country", Measure: MeasureSanctions, Countries: all}}
	if len(all) != MaxProgrammeCountries || cmd.Validate() != nil {
		t.Fatalf("a programme may name every assigned country: %d %v", len(all), cmd.Validate())
	}
}

func TestResolveProgrammesContract(t *testing.T) {
	ok := ResolveProgrammes{Lists: []ListVersionRef{{ListCode: "cn.country_programmes"}, {ListCode: "un.country_programmes", Version: "v1"}},
		Countries: []string{"AQ", "DE", "AQ"}}
	for _, c := range []struct {
		q    ResolveProgrammes
		want string
	}{
		{ok, "OK"},
		{ResolveProgrammes{}, "OK"},
		{ResolveProgrammes{Lists: make([]ListVersionRef, MaxProgrammeLists+1)}, "SCHEMA_INVALID /lists"},
		{ResolveProgrammes{Countries: make([]string, MaxProgrammeQueryCountries+1)}, "SCHEMA_INVALID /countries"},
		{ResolveProgrammes{Lists: []ListVersionRef{{ListCode: ""}}}, "SCHEMA_INVALID /lists/0"},
		{ResolveProgrammes{Lists: []ListVersionRef{{ListCode: "a", Version: "v 1"}}}, "SCHEMA_INVALID /lists/0"},
		{ResolveProgrammes{Lists: []ListVersionRef{{ListCode: "a"}, {ListCode: "a", Version: "v1"}}}, "SCHEMA_INVALID /lists/1"},
		{ResolveProgrammes{Countries: []string{"DE", "UK"}}, "SCHEMA_INVALID /countries/1"},
	} {
		if got := code(c.q.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", c.q, got, c.want)
		}
	}
}
