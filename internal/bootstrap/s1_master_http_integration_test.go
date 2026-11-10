//go:build integration

package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S1-02 and S1-03 over HTTP with OIDC access tokens: a supplier with address roles, a verified
// service role, a trading profile published by an independent reviewer, a revised contact and a
// published item-code mapping, read back through their routes.
func TestS1MasterDataOverHTTP(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	as := func(actor string, n int) *client {
		return &client{t: t, base: api.URL, token: is.token(w.tenant, "company.A", actor, "human"), n: n}
	}
	ed, rv := as("editor", 0), as("reviewer", 300)
	const pty, cat = "brant.pty/0.1.0", "brant.cat/0.1.0"

	ed.command("pty", pty, "party.save_draft", map[string]any{"id": "supplier", "expected_version": 0, "name": "合成供应商",
		"roles": []string{"supplier"}, "addresses": []any{map[string]any{"address_id": "factory", "role": "seller", "address": "测试地址"}}})
	ed.command("pty", pty, "party.confirm", map[string]any{"id": "supplier", "expected_version": 1, "evidence_ids": []string{"evidence"}})
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	r := ed.command("pty", pty, "service_role.verify", map[string]any{"id": "supplier", "expected_version": 2, "service_role_id": "sea",
		"role": "forwarder", "service_code": "sea.fcl", "origin": "CNSHA", "destination": "DEHAM", "valid_from": kernel.FormatTimestamp(from),
		"valid_until": kernel.FormatTimestamp(from.Add(7 * 24 * time.Hour)), "evidence_ids": []string{"evidence"}})
	if r["status"] != "verified" || r["row_version"] != float64(3) {
		t.Fatalf("verify %v", r)
	}
	status, party := ed.do("GET", "/api/brant/v1/pty/parties/supplier", nil)
	addresses, _ := party["addresses"].([]any)
	roles, _ := party["service_roles"].([]any)
	revision := party["revisions"].([]any)[0].(map[string]any)
	if status != http.StatusOK || party["revision"] != float64(1) || len(addresses) != 1 || len(roles) != 1 ||
		addresses[0].(map[string]any)["role"] != "seller" || len(revision["addresses"].([]any)) != 1 ||
		roles[0].(map[string]any)["origin"] != "CNSHA" || roles[0].(map[string]any)["verified_by"].(map[string]any)["id"] != "editor" {
		t.Fatalf("party %d %v", status, party)
	}

	payment, trade := w.settlement(editor, reviewer)
	ref := func(t ptyapi.TermsRef) map[string]any {
		return map[string]any{"kind": t.Kind, "id": t.ConfigID, "revision": t.Revision}
	}
	ed.command("pty", pty, "trading_profile.save_draft", map[string]any{"id": "supplier", "expected_version": 0, "party_revision": 1,
		"payment_terms": ref(payment), "trade_terms": ref(trade), "owner": map[string]any{"actor_id": "sales.owner", "team_id": "trade"}})
	publish := map[string]any{"id": "supplier", "expected_version": 1, "revision": 1, "evidence_ids": []string{"evidence"}, "reason": "核实合成交易档案"}
	if status, out := ed.try("pty", pty, "trading_profile.publish", publish); status != http.StatusUnprocessableEntity ||
		out["code"] != "INDEPENDENT_REVIEW_REQUIRED" {
		t.Fatalf("editor published its own revision: %d %v", status, out)
	}
	if r = rv.command("pty", pty, "trading_profile.publish", publish); r["status"] != "active" {
		t.Fatalf("publish %v", r)
	}
	status, profile := ed.do("GET", "/api/brant/v1/pty/parties/supplier/trading_profile", nil)
	if status != http.StatusOK || profile["ok"] != true || profile["active_revision"] != float64(1) ||
		profile["revisions"].([]any)[0].(map[string]any)["payment_terms"].(map[string]any)["id"] != payment.ConfigID {
		t.Fatalf("profile %d %v", status, profile)
	}

	w.staffParty(editor, "customer", []string{ptyapi.RoleCustomer})
	_, err = w.c.PTY.SaveContact(editor, ptyapi.SaveContact{IdempotencyKey: w.key(), ContactID: "buyer", Name: "合成联系人", PartyID: "customer"})
	w.must(err)
	_, err = w.c.PTY.ConfirmContact(editor, ptyapi.ConfirmContact{IdempotencyKey: w.key(), ContactID: "buyer", ExpectedVersion: 1,
		EvidenceIDs: []string{"evidence"}})
	w.must(err)
	if r = ed.command("pty", pty, "contact.revise", map[string]any{"id": "buyer", "expected_version": 2, "reason": "电话变更"}); r["status"] != "draft" {
		t.Fatalf("contact.revise %v", r)
	}

	w.sku(editor, "sku", true, "evidence")
	ed.command("cat", cat, "item_code_mapping.save_draft", map[string]any{"id": "mapping1", "expected_version": 0, "party_id": "supplier",
		"party_role": "supplier", "external_code": "SUP-01", "sku_id": "sku", "spec_revision": 1})
	if r = rv.command("cat", cat, "item_code_mapping.publish", map[string]any{"id": "mapping1", "expected_version": 1, "revision": 1,
		"evidence_ids": []string{"evidence"}, "reason": "供应商货号核实"}); r["status"] != "active" {
		t.Fatalf("mapping publish %v", r)
	}
	status, mapping := ed.do("GET", "/api/brant/v1/cat/item_code_mappings/mapping1", nil)
	rev := mapping["revisions"].([]any)[0].(map[string]any)
	if status != http.StatusOK || mapping["active_revision"] != float64(1) || mapping["code"].(map[string]any)["external_code"] != "SUP-01" ||
		rev["sku"].(map[string]any)["spec_revision"] != float64(1) || rev["published_by"].(map[string]any)["id"] != "reviewer" {
		t.Fatalf("mapping %d %v", status, mapping)
	}
}
