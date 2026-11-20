package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/reference/api"
)

// S5R-D15(a): country_programme.import hashes the canonical request; without regions it is the hash
// of the same request before regions existed (computed by the code of 90925f3, the S5R base, and
// pinned here), and regions sent in another order are the same request.
func TestS5RProgrammeImportKeepsItsRequestHash(t *testing.T) {
	cmd := api.ImportCountryProgrammes{ListCode: "cn.country_programmes", Jurisdiction: "CN", Version: "2026.1", SourceName: "SYNTHETIC",
		SourceURL: "https://example.invalid/programmes", PublishedOn: "2026-11-01", FileSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		ProgrammeCount: 2, CountryCount: 2, Programmes: []api.CountryProgramme{
			{ProgrammeID: "SYN-B", Name: "Synthetic B", Measure: "sanctions", Countries: []string{"BV", "AQ"}, Scope: "goods"},
			{ProgrammeID: "SYN-A", Name: "Synthetic A", Measure: "embargo", Countries: []string{"AQ"}}}}
	hash := func(cmd api.ImportCountryProgrammes) string {
		args := cmd
		list := canonicalProgrammes(cmd.Programmes)
		args.Programmes = make([]api.CountryProgramme, len(list))
		for i, p := range list {
			args.Programmes[i] = api.CountryProgramme{ProgrammeID: p.ID, Name: p.Name, Measure: p.Measure, Countries: p.Countries, Scope: p.Scope,
				Regions: p.Regions}
		}
		h, err := action.RequestHash(args)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if got := hash(cmd); got != "5444ee0bb7f11b836a88c31d87effba906bf7a6a49bc748f72d79795569c9549" {
		t.Fatalf("an import without regions hashes as %s", got)
	}
	cmd.Programmes[0].Regions = []string{"BV-02", "AQ-01"}
	first := hash(cmd)
	cmd.Programmes[0].Regions = []string{"AQ-01", "BV-02"}
	if first == "5444ee0bb7f11b836a88c31d87effba906bf7a6a49bc748f72d79795569c9549" || hash(cmd) != first {
		t.Fatal("regions are part of the canonical request, in any order")
	}
}
