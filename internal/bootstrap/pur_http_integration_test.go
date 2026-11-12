//go:build integration

package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S3-10 and S3-16 over HTTP with OIDC access tokens: from the active 960.00 order of the TRD world
// the editor prepares BRC-001's 600.00 purchase draft in its wire form, reads it and the order's
// demand allocation, reconciles the command, and voids the draft, which releases the allocation.
func TestPurchaseDraftOverHTTP(t *testing.T) {
	q := newOrderWorld(t)
	order := q.activeOrder("quote.1")
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ed := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "editor", "human")}

	prepare := map[string]any{"draft_id": "purchase.http", "order_id": order.ObjectID, "selection_reason": purReason,
		"lines": []any{map[string]any{"order_line_id": "L1", "quantity": "120",
			"source": map[string]any{"kind": purapi.KindSupplierOffer, "id": "offer.1", "revision": 1, "condition_line_id": "OL1"}}}}
	envelope := map[string]any{"contract": purapi.Contract, "command": purapi.CommandPrepareDraft, "idempotency_key": "pur.http.1", "args": prepare}
	status, r := ed.do("POST", "/api/brant/v1/pur/commands/purchase.draft.prepare", envelope)
	if status != http.StatusOK || r["object_id"] != "purchase.http" || r["row_version"] != float64(1) || r["status"] != "prepared" {
		t.Fatalf("prepare %d %v", status, r)
	}
	status, draft := ed.do("GET", "/api/brant/v1/pur/purchase_drafts/purchase.http", nil)
	if status != http.StatusOK || draft["ok"] != true || draft["state"] != "prepared" || draft["net_minor"] != "60000" ||
		draft["order_id"] != order.ObjectID || len(draft["lines"].([]any)) != 1 {
		t.Fatalf("draft %d %v", status, draft)
	}
	status, alloc := ed.do("GET", "/api/brant/v1/pur/orders/"+order.ObjectID+"/allocations", nil)
	lines, _ := alloc["allocations"].([]any)
	if status != http.StatusOK || len(lines) != 1 || lines[0].(map[string]any)["allocated"] != "120" || lines[0].(map[string]any)["remaining"] != "0" {
		t.Fatalf("allocations %d %v", status, alloc)
	}
	status, rec := ed.do("POST", "/api/brant/v1/pur/commands/purchase.draft.prepare/reconcile", envelope)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != r["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}

	ed.n = 500
	r = ed.command("pur", purapi.Contract, purapi.CommandVoidDraft, map[string]any{"draft_id": "purchase.http", "expected_version": 1,
		"reason": purVoidReason})
	if r["row_version"] != float64(2) || r["status"] != "voided" {
		t.Fatalf("void %v", r)
	}
	if _, alloc = ed.do("GET", "/api/brant/v1/pur/orders/"+order.ObjectID+"/allocations", nil); alloc["allocations"].([]any)[0].(map[string]any)["allocated"] != "0" {
		t.Fatalf("allocations after the void %v", alloc)
	}
	if status, out := ed.try("pur", "brant.other/0.1.0", purapi.CommandVoidDraft, map[string]any{"draft_id": "purchase.http"}); status != http.StatusBadRequest ||
		out["code"] != kernel.CodeUnsupportedContract {
		t.Fatalf("another contract: %d %v", status, out)
	}
}
