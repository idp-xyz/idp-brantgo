package application

import (
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
	"go.idp.xyz/idp-brant/internal/reference/api"
)

// Both imports are human only (S1-11; S5 E-3, ADR-BR-017: the compliance lead confirms list and
// programme sources); reads are open to every actor kind with ref.read. The role templates give
// both import permissions to compliance alone (exclusivePermissions).
func TestImportsAreHumanOnlyAndHeldByCompliance(t *testing.T) {
	humanOnly := map[string]bool{api.CommandImportScreeningList: true, api.CommandImportCountryProgrammes: true}
	seen := map[string]bool{}
	for _, r := range Rules() {
		seen[r.Command] = true
		human := len(r.ActorKinds) == 1 && r.ActorKinds[0] == kernel.ActorHuman
		if human != humanOnly[r.Command] {
			t.Errorf("%s: human only %v", r.Command, human)
		}
		if humanOnly[r.Command] && r.Permission != r.Command {
			t.Errorf("%s needs permission %s", r.Command, r.Permission)
		}
	}
	for _, c := range []string{api.CommandImportCountryProgrammes, CommandGetProgrammeList, CommandGetList, CommandAudit} {
		if !seen[c] {
			t.Errorf("no rule for %s", c)
		}
	}
	if _, err := authorization.NewPolicy(Rules()...); err != nil {
		t.Fatal(err)
	}
	for _, e := range authorization.ExclusivePermissions() {
		if humanOnly[e.Permission] && e.Template != "compliance" {
			t.Errorf("%s is exclusive to %s", e.Permission, e.Template)
		}
	}
	var held []string
	for _, tpl := range authorization.Templates() {
		if slices.Contains(tpl.Permissions, api.CommandImportCountryProgrammes) {
			held = append(held, tpl.Code)
		}
	}
	if !slices.Equal(held, []string{"compliance"}) {
		t.Fatalf("country_programme.import is held by %v", held)
	}
}

// The request hash of country_programme.import is over the canonical content: the same programmes
// in another order, with their countries in another order, are the same request.
func TestProgrammesAreCanonicalForTheRequest(t *testing.T) {
	a := canonicalProgrammes([]api.CountryProgramme{
		{ProgrammeID: "P2", Name: "Two", Measure: api.MeasureSanctions, Countries: []string{"HM", "AQ"}},
		{ProgrammeID: "P1", Name: "One", Measure: api.MeasureEmbargo, Countries: []string{"BV"}},
	})
	b := canonicalProgrammes([]api.CountryProgramme{
		{ProgrammeID: "P1", Name: "One", Measure: api.MeasureEmbargo, Countries: []string{"BV"}},
		{ProgrammeID: "P2", Name: "Two", Measure: api.MeasureSanctions, Countries: []string{"AQ", "HM"}},
	})
	if len(a) != 2 || a[0].ID != "P1" || !slices.Equal(a[1].Countries, []string{"AQ", "HM"}) || a[0].Hash != b[0].Hash || a[1].Hash != b[1].Hash {
		t.Fatalf("%+v %+v", a, b)
	}
}
