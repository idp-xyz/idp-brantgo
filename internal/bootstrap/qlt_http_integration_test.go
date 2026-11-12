//go:build integration

package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/identity/oidc"
	"go.idp.xyz/idp-brant/internal/kernel"
	"go.idp.xyz/idp-brant/internal/transport/httpapi"
)

// WBS S5-02 over HTTP with OIDC access tokens (SIM-01 step 11, F-06): an external agency's report
// is recorded as an inspection in its wire form, our quality inspector releases 120 PCS, reads the
// inspection, the release and the subject, reconciles the grant, revokes the release and revokes it
// again. The transport is the composed one with QLT on the in-memory PUR port.
func TestQualityOverHTTP(t *testing.T) {
	x := newQltWorld(t)
	x.subjects.put(commitmentLine("commitment.1", "120"))
	is := newIssuer(t)
	verifier, err := oidc.New(oidc.Config{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	x.must(err)
	c := x.c
	handler, err := httpapi.New(httpapi.Config{EVD: c.EVD, PTY: c.PTY, CAT: c.CAT, INQ: c.INQ, POL: c.POL, REF: c.REF, SUP: c.SUP, CST: c.CST,
		TRD: c.TRD, PUR: c.PUR, QLT: x.q, Verifier: oidc.HeaderVerifier{Tokens: verifier}, Clock: bento.SystemClock{}})
	x.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	qc := &client{t: t, base: srv.URL, token: is.token(x.tenant, "company.A", "qc.1", "human")}

	subject := map[string]any{"kind": qltapi.SubjectCommitmentLine, "id": "commitment.1", "line_id": "L1"}
	r := qc.command("qlt", qltapi.Contract, qltapi.CommandRecordInspection, map[string]any{"inspection_id": "insp.http", "subject": subject,
		"inspected_quantity": "120", "conforming_quantity": "120", "nonconforming_quantity": "0",
		"sampling":       map[string]any{"method": qltapi.SamplingSample, "sample_size": "32", "plan": "AQL 2.5 GII"},
		"standard":       map[string]any{"source_id": "standard"},
		"inspector":      map[string]any{"kind": qltapi.InspectorAgency, "agency": map[string]any{"party_id": "agency", "service_role_id": "final", "revision": 1}},
		"report_file_id": "report", "inspected_at": kernel.FormatTimestamp(time.Now().Add(-time.Hour))})
	if r["object_id"] != "insp.http" || r["status"] != qltapi.StatusRecorded || r["row_version"] != float64(1) {
		t.Fatalf("record %v", r)
	}
	grant := map[string]any{"contract": qltapi.Contract, "command": qltapi.CommandGrantRelease, "idempotency_key": "qlt.http.grant",
		"args": map[string]any{"release_id": "rel.http", "subject": subject, "quantity": "120",
			"valid_until": kernel.FormatTimestamp(time.Now().Add(30 * 24 * time.Hour))}}
	status, g := qc.do("POST", "/api/brant/v1/qlt/commands/quality_release.grant", grant)
	if status != http.StatusOK || g["object_id"] != "rel.http" || g["status"] != qltapi.StatusGranted {
		t.Fatalf("grant %d %v", status, g)
	}
	status, rec := qc.do("POST", "/api/brant/v1/qlt/commands/quality_release.grant/reconcile", grant)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != g["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	status, insp := qc.do("GET", "/api/brant/v1/qlt/inspections/insp.http", nil)
	inspector, _ := insp["inspector"].(map[string]any)
	if status != http.StatusOK || insp["ok"] != true || insp["conforming_quantity"] != "120" || inspector["kind"] != qltapi.InspectorAgency ||
		insp["report"].(map[string]any)["file_id"] != "report" || insp["sampling"].(map[string]any)["sample_size"] != "32" {
		t.Fatalf("inspection %d %v", status, insp)
	}
	status, rel := qc.do("GET", "/api/brant/v1/qlt/quality_releases/rel.http", nil)
	if status != http.StatusOK || rel["state"] != qltapi.StatusGranted || rel["quantity"] != "120" || rel["conforming"] != "120" {
		t.Fatalf("release %d %v", status, rel)
	}
	status, sub := qc.do("GET", "/api/brant/v1/qlt/subjects/purchase_commitment_line/commitment.1/lines/L1", nil)
	if status != http.StatusOK || sub["released"] != "120" || sub["releasable"] != "0" || len(sub["inspections"].([]any)) != 1 {
		t.Fatalf("subject %d %v", status, sub)
	}
	if status, out := qc.do("GET", "/api/brant/v1/qlt/subjects/purchase_commitment_line/commitment.9/lines/L1", nil); status != http.StatusNotFound ||
		out["code"] != kernel.CodeNotFound {
		t.Fatalf("an unknown subject %d %v", status, out)
	}

	agent := &client{t: t, base: srv.URL, token: is.token(x.tenant, "company.A", "agent.1", "ai")}
	if status, out := agent.try("qlt", qltapi.Contract, qltapi.CommandRevokeRelease, map[string]any{"release_id": "rel.http", "reason": "AI 撤回"}); status != http.StatusForbidden ||
		out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("an AI revocation %d %v", status, out)
	}
	if status, out := qc.try("qlt", qltapi.Contract, qltapi.CommandGrantRelease, map[string]any{"release_id": "rel.more", "subject": subject,
		"quantity": "1", "valid_until": kernel.FormatTimestamp(time.Now().Add(time.Hour))}); status != http.StatusUnprocessableEntity ||
		out["code"] != qltapi.CodeQualityReleaseExceedsConforming {
		t.Fatalf("above conforming %d %v", status, out)
	}
	r = qc.command("qlt", qltapi.Contract, qltapi.CommandRevokeRelease, map[string]any{"release_id": "rel.http", "reason": "复检发现色差"})
	if r["status"] != qltapi.StatusRevoked || r["row_version"] != float64(2) || r["replayed"] != false {
		t.Fatalf("revoke %v", r)
	}
	r = qc.command("qlt", qltapi.Contract, qltapi.CommandRevokeRelease, map[string]any{"release_id": "rel.http", "reason": "重复撤回"})
	if r["status"] != qltapi.StatusRevoked || r["row_version"] != float64(2) || r["replayed"] != true {
		t.Fatalf("second revoke %v", r)
	}
	if status, out := qc.try("qlt", qltapi.Contract, qltapi.CommandRecordInspection, map[string]any{"inspection_id": "insp.bad"}); status != http.StatusUnprocessableEntity ||
		out["code"] != kernel.CodeSchemaInvalid {
		t.Fatalf("an incomplete record %d %v", status, out)
	}
}
