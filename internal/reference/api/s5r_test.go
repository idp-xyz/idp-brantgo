package api

import (
	"slices"
	"strings"
	"testing"
)

// S5R-D15(a) (UL PLATFORM-CountryProgrammeVersion): a programme may list ISO 3166-2 regions of its
// countries; it then affects only those regions of those countries, which CNX leaves to a human as a
// region_review finding.
func TestS5RProgrammeRegionsContract(t *testing.T) {
	for code, want := range map[string]bool{"UA-43": true, "GB-ENG": true, "US-NY": true, "CN-31": true, "UA-4": true,
		"UK-ENG": false, "UA": false, "UA-": false, "UA-ABCD": false, "ua-43": false, "UA-4x": false, "XK-01": false, "UA43": false} {
		if ValidRegionCode(code) != want {
			t.Errorf("%q: %v", code, !want)
		}
	}
	for _, c := range []struct {
		edit func(*ImportCountryProgrammes)
		want string
	}{
		{func(c *ImportCountryProgrammes) { c.Programmes[1].Regions = []string{"HM-01"} }, "OK"},
		{func(c *ImportCountryProgrammes) { c.Programmes[1].Regions = []string{"AQ-02", "HM-01"} }, "OK"},
		{func(c *ImportCountryProgrammes) { c.Programmes[1].Regions = []string{} }, "OK"},
		{func(c *ImportCountryProgrammes) { c.Programmes[1].Regions = []string{"HM-01", "HM-01"} }, "SCHEMA_INVALID /args/programmes/1/regions/1"},
		{func(c *ImportCountryProgrammes) { c.Programmes[1].Regions = []string{"HM"} }, "SCHEMA_INVALID /args/programmes/1/regions/0"},
		{func(c *ImportCountryProgrammes) { c.Programmes[1].Regions = []string{"hm-01"} }, "SCHEMA_INVALID /args/programmes/1/regions/0"},
		{func(c *ImportCountryProgrammes) { c.Programmes[1].Regions = []string{"BV-01"} }, "INVALID_KNOWN_VALUE /args/programmes/1/regions/0"},
		{func(c *ImportCountryProgrammes) {
			c.Programmes[1].Regions = strings.Split(strings.Repeat("HM-1,", MaxProgrammeRegions+1), ",")[:MaxProgrammeRegions+1]
		}, "SCHEMA_INVALID /args/programmes/1/regions"},
	} {
		cmd := programmeImport()
		c.edit(&cmd)
		if got := code(cmd.Validate()); got != c.want {
			t.Errorf("%v: %s, want %s", cmd.Programmes[1].Regions, got, c.want)
		}
	}
	p := Programme{Countries: []string{"AQ", "HM", "UA"}, Regions: []string{"AQ-02", "UA-43", "UA-9"}}
	if got := p.RegionsOf("UA"); !slices.Equal(got, []string{"UA-43", "UA-9"}) {
		t.Fatalf("regions of UA: %v", got)
	}
	if p.RegionsOf("HM") != nil || p.RegionsOf("U") != nil {
		t.Fatal("a country without listed regions is affected whole")
	}
}
