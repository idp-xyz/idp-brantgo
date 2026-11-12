//go:build integration

package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5-07 over HTTP with OIDC access tokens, on the composed process (bootstrap's FUL on PUR's real
// supplier deliveries and CNX's and STL's guards; the FUL world on PUR of newFulPurWorld): the
// world's order, whose 960.00 before shipment the treasurer unapplies and applies again as 958.00,
// is 2.00 short. The documentation clerk's evaluation answers status blocked and the release reads
// back with its code, the stage and the bindings; the sales clerk's exception is 403
// PERMISSION_DENIED; the credit manager's is granted and reconciles; the merchandiser confirms;
// the documents scope, short the same 2.00, is evaluated blocked and excepted on its own; the
// documentation clerk records the original bill of lading's release; the shipment's releases read
// back.
func TestCommercialReleaseOverHTTP(t *testing.T) {
	w := newFulPurWorld(t)
	w.assign("sales.1", "sales_clerk")
	d := w.verified("delivery.http", "120", "120")
	_, err := w.ship(w.c.FUL, w.merch, "shipment.sc08", "120", d)
	w.must(err)
	stage := w.snapshots(w.author, w.order.ObjectID)[w.order.ObjectID].Stages[0].StageID
	_, err = w.c.STL.Unapply(w.treasury, stlapi.Unapply{IdempotencyKey: w.key(), ApplicationID: "app." + w.order.ObjectID + "." + stage,
		ExpectedVersion: 1, Reason: "金额有误，撤销重核"})
	w.must(err)
	w.stlApply("app.short", "fund."+w.order.ObjectID+"."+stage, w.order.ObjectID, stage, "958.00")
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	doc := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "doc.1", "human")}
	manager := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "manager.1", "human"), n: 200}
	sales := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "sales.1", "human"), n: 300}
	merch := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "merch.1", "human"), n: 400}

	e := doc.command("ful", fulapi.Contract, fulapi.CommandEvaluateRelease, map[string]any{"shipment_id": "shipment.sc08", "scope": "shipment"})
	id, _ := e["object_id"].(string)
	if e["status"] != fulapi.ReleaseBlocked || e["row_version"] != float64(1) || id == "" {
		t.Fatalf("evaluate %v", e)
	}
	status, v := doc.do("GET", "/api/brant/v1/ful/commercial_releases/"+id, nil)
	evaluations, _ := v["evaluations"].([]any)
	if status != http.StatusOK || v["status"] != fulapi.ReleaseBlocked || len(evaluations) != 1 {
		t.Fatalf("release %d %v", status, v)
	}
	first := evaluations[0].(map[string]any)
	payment, _ := first["bindings"].(map[string]any)["payment_condition"].(map[string]any)
	if first["code"] != fulapi.CodeCommercialReleaseBlocked || first["guard"] != fulapi.GuardPaymentCondition || payment["stage_id"] != stage ||
		payment["shortfall_minor"] != "200" || len(first["guards_sha256"].(string)) != 64 {
		t.Fatalf("evaluation %v", first)
	}
	exception := map[string]any{"shipment_id": "shipment.sc08", "scope": "shipment", "expected_version": 1, "stage_id": stage, "currency": "USD",
		"shortfall_minor": "200", "valid_until": kernel.FormatTimestamp(time.Now().Add(48 * time.Hour)), "reason": "客户书面承诺差额随下批支付",
		"evidence_ids": []any{"evidence"}}
	if status, out := sales.try("ful", fulapi.Contract, fulapi.CommandGrantReleaseException, exception); status != http.StatusForbidden ||
		out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("sales clerk %d %v", status, out)
	}
	req := map[string]any{"contract": fulapi.Contract, "command": fulapi.CommandGrantReleaseException, "idempotency_key": "ful.http.exception",
		"args": exception}
	status, g := manager.do("POST", "/api/brant/v1/ful/commands/"+fulapi.CommandGrantReleaseException, req)
	if status != http.StatusOK || g["status"] != fulapi.ReleaseGranted || g["row_version"] != float64(2) || g["object_id"] != id {
		t.Fatalf("exception %d %v", status, g)
	}
	status, rec := manager.do("POST", "/api/brant/v1/ful/commands/"+fulapi.CommandGrantReleaseException+"/reconcile", req)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != g["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	c := merch.command("ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": "shipment.sc08", "expected_version": 1,
		"evidence_ids": []any{"evidence"}})
	if c["status"] != fulapi.StatusConfirmed {
		t.Fatalf("confirm %v", c)
	}

	docs := doc.command("ful", fulapi.Contract, fulapi.CommandEvaluateRelease, map[string]any{"shipment_id": "shipment.sc08", "scope": "documents"})
	if docs["status"] != fulapi.ReleaseBlocked {
		t.Fatalf("documents %v", docs)
	}
	exception["scope"], exception["valid_until"] = "documents", kernel.FormatTimestamp(time.Now().Add(24*time.Hour))
	if x := manager.command("ful", fulapi.Contract, fulapi.CommandGrantReleaseException, exception); x["status"] != fulapi.ReleaseGranted {
		t.Fatalf("documents exception %v", x)
	}
	r := doc.command("ful", fulapi.Contract, fulapi.CommandRecordDocumentRelease, map[string]any{"shipment_id": "shipment.sc08", "expected_version": 2,
		"release_kind": fulapi.ReleaseOriginalBL, "evidence_ids": []any{"evidence"}})
	if r["status"] != fulapi.ReleaseConsumed || r["row_version"] != float64(3) {
		t.Fatalf("document release %v", r)
	}
	status, list := merch.do("GET", "/api/brant/v1/ful/shipments/shipment.sc08/commercial_releases", nil)
	releases, _ := list["releases"].([]any)
	if status != http.StatusOK || len(releases) != 2 || releases[0].(map[string]any)["status"] != fulapi.ReleaseConsumed ||
		releases[1].(map[string]any)["consumption"].(map[string]any)["release_kind"] != fulapi.ReleaseOriginalBL {
		t.Fatalf("releases %d %v", status, list)
	}
	if status, out := doc.do("GET", "/api/brant/v1/ful/commercial_releases/rls-none", nil); status != http.StatusNotFound {
		t.Fatalf("unknown release %d %v", status, out)
	}
}
