//go:build integration

package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S1-09 and S1-11 over HTTP (TD-AT15): compliance imports a list, an editor saves the
// screening_regulation through the reflective POL mapping and a reviewer publishes it, compliance
// screens a customer, reads the suspected match, decides it a false positive and finds the party due
// once the conclusion has expired. A sales clerk cannot decide.
func TestPartyScreeningOverHTTP(t *testing.T) {
	s := newScreeningWorld(t)
	is := newIssuer(t)
	handler, err := s.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	s.must(err)
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	as := func(actor string) *client {
		return &client{t: t, base: api.URL, token: is.token(s.tenant, "company.A", actor, "human")}
	}
	compliance, editor, reviewer, sales := as("compliance"), as("editor"), as("reviewer"), as("sales")
	editor.n, reviewer.n, sales.n = 100, 200, 300 // distinct idempotency keys per client

	r := compliance.command("ref", "brant.ref/0.1.0", "screening_list.import", map[string]any{
		"list_code": "un.sc", "jurisdiction": "UN", "version": "2026-11-01", "source_name": "Synthetic UN list",
		"source_url": "https://lists.example.org/un.sc", "published_on": "2026-11-01",
		"file_sha256": "4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e", "entry_count": 1,
		"entries": []any{map[string]any{"entry_id": "QDe.001", "primary_name": "Acme Trading Co.", "aliases": []string{"ACME TRADING"},
			"entity_type": "entity"}}})
	if r["object_id"] != "un.sc:2026-11-01" || r["status"] != "imported" {
		t.Fatalf("import %v", r)
	}

	editor.command("pol", "brant.pol/0.1.0", "config.save_draft", map[string]any{"kind": "screening_regulation", "id": "screening.cn",
		"expected_version": 0, "valid_from": kernel.FormatTimestamp(newWindow().from),
		"data": map[string]any{"lists": []any{map[string]any{"list_code": "un.sc", "jurisdiction": "UN"}}, "rescreen_interval_days": 30}})
	status, cfg := editor.do("GET", "/api/brant/v1/pol/configs/screening_regulation/screening.cn", nil)
	if status != http.StatusOK || cfg["working_revision"] != float64(1) {
		t.Fatalf("config %d %v", status, cfg)
	}
	reviewer.command("pol", "brant.pol/0.1.0", "config.publish", map[string]any{"kind": "screening_regulation", "id": "screening.cn",
		"expected_version": 1, "revision": 1, "evidence_ids": []string{"evidence"}, "reason": "合规负责人确认名单来源"})

	s.customer("party.acme", "ACME Trading Co")
	r = compliance.command("pty", "brant.pty/0.1.0", "party_screening.record", map[string]any{"id": "party.acme", "expected_version": 0,
		"regulation_id": "screening.cn", "regulation_revision": 1})
	if r["status"] != "match" || r["row_version"] != float64(1) {
		t.Fatalf("record %v", r)
	}
	status, v := compliance.do("GET", "/api/brant/v1/pty/parties/party.acme/screening", nil)
	if status != http.StatusOK || v["ok"] != true || v["status"] != "match" || v["reason"] != "suspected_match" {
		t.Fatalf("screening %d %v", status, v)
	}
	conclusion := v["screenings"].([]any)[0].(map[string]any)
	hit := conclusion["hits"].([]any)[0].(map[string]any)
	by := conclusion["screened_by"].(map[string]any)
	if hit["entry_id"] != "QDe.001" || hit["disposition"] != "suspected" || by["kind"] != "human" || by["id"] != "compliance" ||
		conclusion["normalization"] != "name-key-v1" {
		t.Fatalf("conclusion %v", conclusion)
	}
	if _, err := kernel.ParseTimestamp(conclusion["screened_at"].(string)); err != nil {
		t.Fatalf("screened_at %v: %v", conclusion["screened_at"], err)
	}
	if _, present := conclusion["decision"]; present {
		t.Fatal("no decision yet: the member is absent, never null")
	}

	decide := map[string]any{"id": "party.acme", "expected_version": 1, "screening_id": v["current_screening_id"],
		"decision": "false_positive", "reason": "注册号与名单条目不同", "register_positive": true}
	if status, out := sales.try("pty", "brant.pty/0.1.0", "party_screening.decide", decide); status != http.StatusForbidden ||
		out["code"] != "PERMISSION_DENIED" {
		t.Fatalf("sales decided: %d %v", status, out)
	}
	if r = compliance.command("pty", "brant.pty/0.1.0", "party_screening.decide", decide); r["status"] != "clear" {
		t.Fatalf("decide %v", r)
	}

	status, due := compliance.do("GET", "/api/brant/v1/pty/screenings/due?at="+kernel.FormatTimestamp(time.Now().Add(40*24*time.Hour)), nil)
	rows, _ := due["due"].([]any)
	if status != http.StatusOK || len(rows) != 1 || rows[0].(map[string]any)["party_id"] != "party.acme" ||
		rows[0].(map[string]any)["reason"] != "expired" {
		t.Fatalf("due %d %v", status, due)
	}
	if status, out := compliance.do("GET", "/api/brant/v1/pty/screenings/due?at=yesterday", nil); status != http.StatusUnprocessableEntity ||
		out["code"] != "SCHEMA_INVALID" {
		t.Fatalf("bad at: %d %v", status, out)
	}

	status, list := compliance.do("GET", "/api/brant/v1/ref/screening_lists/un.sc", nil)
	if status != http.StatusOK || list["current_version"] != "2026-11-01" || len(list["versions"].([]any)) != 1 {
		t.Fatalf("list %d %v", status, list)
	}
	status, version := compliance.do("GET", "/api/brant/v1/ref/screening_lists/un.sc/versions/2026-11-01", nil)
	entries, _ := version["entries"].([]any)
	if status != http.StatusOK || version["list_code"] != "un.sc" || version["current"] != true || len(entries) != 1 ||
		entries[0].(map[string]any)["primary_name"] != "Acme Trading Co." {
		t.Fatalf("version %d %v", status, version)
	}
	status, audit := compliance.do("GET", "/api/brant/v1/ref/audit", nil)
	if status != http.StatusOK || audit["entries"].([]any)[0].(map[string]any)["command"] != "screening_list.import" {
		t.Fatalf("ref audit %d %v", status, audit)
	}
}
