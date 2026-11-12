//go:build integration

package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
)

// S5 A3b over HTTP with OIDC access tokens: compliance imports the synthetic UN programme list,
// reads the list and the version back and finds the import in the reference audit; an actor
// without the import permission is refused. A party saved with an address country reads the
// country back, and an address without one reads back without the member (D-8).
func TestCountryProgrammesAndAddressCountryOverHTTP(t *testing.T) {
	p := newProgrammeWorld(t)
	is := newIssuer(t)
	handler, err := p.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	p.must(err)
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	as := func(actor string) *client {
		return &client{t: t, base: api.URL, token: is.token(p.tenant, "company.A", actor, "human")}
	}
	compliance, screener := as("compliance"), as("screener")
	screener.n = 100

	fixture := syntheticCountryProgrammes(unProgrammeList, "2026-11", "2026-11-01")
	programmes := []any{}
	for _, pr := range fixture.Programmes {
		programmes = append(programmes, map[string]any{"programme_id": pr.ProgrammeID, "name": pr.Name, "measure": pr.Measure,
			"countries": pr.Countries, "scope": pr.Scope})
	}
	args := map[string]any{"list_code": fixture.ListCode, "jurisdiction": fixture.Jurisdiction, "version": fixture.Version,
		"source_name": fixture.SourceName, "source_url": fixture.SourceURL, "published_on": fixture.PublishedOn,
		"file_sha256": fixture.FileSHA256, "programme_count": fixture.ProgrammeCount, "country_count": fixture.CountryCount,
		"programmes": programmes}
	if status, out := screener.try("ref", "brant.ref/0.1.0", "country_programme.import", args); status != http.StatusForbidden ||
		out["code"] != "PERMISSION_DENIED" {
		t.Fatalf("screener imported: %d %v", status, out)
	}
	r := compliance.command("ref", "brant.ref/0.1.0", "country_programme.import", args)
	if r["object_id"] != "un.country_programmes:2026-11" || r["status"] != "imported" || r["row_version"] != float64(1) {
		t.Fatalf("import %v", r)
	}

	status, list := compliance.do("GET", "/api/brant/v1/ref/country_programme_lists/"+unProgrammeList, nil)
	if status != http.StatusOK || list["current_version"] != "2026-11" || list["jurisdiction"] != "UN" || len(list["versions"].([]any)) != 1 {
		t.Fatalf("list %d %v", status, list)
	}
	status, version := compliance.do("GET", "/api/brant/v1/ref/country_programme_lists/"+unProgrammeList+"/versions/2026-11", nil)
	got, _ := version["programmes"].([]any)
	if status != http.StatusOK || version["current"] != true || version["country_count"] != float64(2) || len(got) != 1 {
		t.Fatalf("version %d %v", status, version)
	}
	pr := got[0].(map[string]any)
	if pr["programme_id"] != "SYN-UN-ARM-1" || pr["measure"] != "arms_embargo" || len(pr["countries"].([]any)) != 2 ||
		pr["countries"].([]any)[0] != "BV" || len(pr["content_hash"].(string)) != 64 {
		t.Fatalf("programme %v", pr)
	}
	if status, out := compliance.do("GET", "/api/brant/v1/ref/country_programme_lists/"+cnProgrammeList, nil); status != http.StatusNotFound ||
		out["code"] != "NOT_FOUND" {
		t.Fatalf("an unknown list: %d %v", status, out)
	}
	status, audit := compliance.do("GET", "/api/brant/v1/ref/audit", nil)
	var commands []string
	for _, e := range audit["entries"].([]any) {
		commands = append(commands, e.(map[string]any)["command"].(string)+"/"+e.(map[string]any)["outcome"].(string))
	}
	if status != http.StatusOK || len(commands) < 2 || commands[0] != "country_programme.import/rejected" || commands[1] != "country_programme.import/succeeded" {
		t.Fatalf("ref audit %d %v", status, commands)
	}

	editor := &client{t: t, base: api.URL, token: is.token(p.tenant, "company.A", "editor", "human"), n: 200}
	p.provision(bootstrap.Grant{Scope: p.scope("company.A"), Actor: actorRef("human", "editor"), Permissions: allPermissions, DataScope: "company"})
	editor.command("pty", "brant.pty/0.1.0", "party.save_draft", map[string]any{"id": "customer", "expected_version": 0, "name": "customer",
		"roles": []string{"customer"}, "addresses": []any{
			map[string]any{"address_id": "hq", "role": "buyer", "address": "合成买方地址\n汉堡"},
			map[string]any{"address_id": "dock", "role": "consignee", "address": "合成收货地址", "country": "DE"}}})
	status, party := editor.do("GET", "/api/brant/v1/pty/parties/customer", nil)
	addresses, _ := party["addresses"].([]any)
	if status != http.StatusOK || len(addresses) != 2 || addresses[0].(map[string]any)["country"] != "DE" {
		t.Fatalf("party %d %v", status, party)
	}
	if _, present := addresses[1].(map[string]any)["country"]; present {
		t.Fatalf("an unknown country is absent, never null or empty: %v", addresses[1])
	}
	if status, out := editor.try("pty", "brant.pty/0.1.0", "party.save_draft", map[string]any{"id": "other", "expected_version": 0,
		"name": "other", "roles": []string{"customer"}, "addresses": []any{map[string]any{"address_id": "dock", "role": "consignee",
			"address": "x", "country": "UK"}}}); status != http.StatusUnprocessableEntity || out["code"] != "SCHEMA_INVALID" {
		t.Fatalf("UK is not an assigned code: %d %v", status, out)
	}
}
