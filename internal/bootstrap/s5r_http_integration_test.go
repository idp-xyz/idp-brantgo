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

// The PTY and POL additions of S5R over HTTP (unit R2): a service role without a route (S5R-D36),
// a notify address (S5R-D40), the permitted payers of a trading profile (S5R-D21) and the contract
// insurance percentage of trade terms (S5R-D06) are written and read with the snake_case members of
// the contract; a member that is not stated stays absent from the view.
func TestS5RPartyAndConfigAdditionsOverHTTP(t *testing.T) {
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
	const pty, pol = "brant.pty/0.1.0", "brant.pol/0.1.0"

	ed.command("pty", pty, "party.save_draft", map[string]any{"id": "agency", "expected_version": 0, "name": "合成检验机构", "roles": []string{"supplier"}})
	ed.command("pty", pty, "party.confirm", map[string]any{"id": "agency", "expected_version": 1, "evidence_ids": []string{"evidence"}})
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	r := ed.command("pty", pty, "service_role.verify", map[string]any{"id": "agency", "expected_version": 2, "service_role_id": "qc",
		"role": "inspection_agency", "service_code": "final.inspection", "valid_from": kernel.FormatTimestamp(from),
		"valid_until": kernel.FormatTimestamp(from.Add(7 * 24 * time.Hour)), "evidence_ids": []string{"evidence"}})
	if r["status"] != "verified" {
		t.Fatalf("verify %v", r)
	}
	status, party := ed.do("GET", "/api/brant/v1/pty/parties/agency", nil)
	roles, _ := party["service_roles"].([]any)
	if status != http.StatusOK || len(roles) != 1 {
		t.Fatalf("party %d %v", status, party)
	}
	if role := roles[0].(map[string]any); role["role"] != "inspection_agency" || role["origin"] != nil || role["destination"] != nil {
		t.Fatalf("a role without a route %v", role)
	}

	ed.command("pty", pty, "party.save_draft", map[string]any{"id": "customer", "expected_version": 0, "name": "合成客户", "roles": []string{"customer"},
		"addresses": []any{map[string]any{"address_id": "notify", "role": "notify", "address": "合成通知方地址", "country": "NL"}}})
	ed.command("pty", pty, "party.confirm", map[string]any{"id": "customer", "expected_version": 1, "evidence_ids": []string{"evidence"}})
	w.staffParty(editor, "payer", []string{ptyapi.RoleCustomer})
	payment, trade := w.settlement(editor, reviewer)
	ref := func(t ptyapi.TermsRef) map[string]any {
		return map[string]any{"kind": t.Kind, "id": t.ConfigID, "revision": t.Revision}
	}
	ed.command("pty", pty, "trading_profile.save_draft", map[string]any{"id": "customer", "expected_version": 0, "party_revision": 1,
		"payment_terms": ref(payment), "trade_terms": ref(trade), "owner": map[string]any{"actor_id": "sales.owner"}, "permitted_payers": []string{"payer"}})
	rv.command("pty", pty, "trading_profile.publish", map[string]any{"id": "customer", "expected_version": 1, "revision": 1,
		"evidence_ids": []string{"evidence"}, "reason": "付款委托书核实（合成）"})
	status, profile := ed.do("GET", "/api/brant/v1/pty/parties/customer/trading_profile", nil)
	rev := profile["revisions"].([]any)[0].(map[string]any)
	if payers, _ := rev["permitted_payers"].([]any); status != http.StatusOK || len(payers) != 1 || payers[0] != "payer" {
		t.Fatalf("profile %d %v", status, profile)
	}
	status, customer := ed.do("GET", "/api/brant/v1/pty/parties/customer", nil)
	if a := customer["addresses"].([]any)[0].(map[string]any); status != http.StatusOK || a["role"] != "notify" || a["country"] != "NL" {
		t.Fatalf("customer %d %v", status, customer)
	}

	ed.command("pol", pol, "config.save_draft", map[string]any{"kind": "trade_terms", "id": "cif", "expected_version": 0,
		"valid_from": kernel.FormatTimestamp(from), "data": map[string]any{"incoterm": "CIF", "edition": "2020", "named_place": "DEHAM",
			"responsibility_note": "synthetic", "delivery": map[string]any{"kind": "fixed_date", "date": "2027-01-31"}, "insurance_percent": "120"}})
	if status, out := ed.try("pol", pol, "config.save_draft", map[string]any{"kind": "trade_terms", "id": "cif.low", "expected_version": 0,
		"valid_from": kernel.FormatTimestamp(from), "data": map[string]any{"incoterm": "CIF", "edition": "2020", "named_place": "DEHAM",
			"responsibility_note": "synthetic", "delivery": map[string]any{"kind": "fixed_date", "date": "2027-01-31"}, "insurance_percent": "100"}}); status != http.StatusUnprocessableEntity || out["code"] != "INVALID_KNOWN_VALUE" {
		t.Fatalf("100 %%: %d %v", status, out)
	}
	status, cfg := ed.do("GET", "/api/brant/v1/pol/configs/trade_terms/cif", nil)
	data := cfg["revisions"].([]any)[0].(map[string]any)["data"].(map[string]any)
	if status != http.StatusOK || data["insurance_percent"] != "120" {
		t.Fatalf("trade terms %d %v", status, cfg)
	}
	status, fca := ed.do("GET", "/api/brant/v1/pol/configs/trade_terms/FCA", nil)
	if _, stated := fca["revisions"].([]any)[0].(map[string]any)["data"].(map[string]any)["insurance_percent"]; status != http.StatusOK || stated {
		t.Fatalf("terms without an insurance percentage %d %v", status, fca)
	}
}
