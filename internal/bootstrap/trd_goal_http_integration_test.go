//go:build integration

package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// GoalReadiness v1 and ActionReconciliation v1 over HTTP (WBS S4-01c; Context Map R18, R66): the
// goal read answers the assessment in snake case — the S3 gap "no HTTP read for TRD's ActiveOrderRef"
// is closed by its order_active form, which names the root's full_v1 order —, and the reconcile
// route of TRD and EVD takes the request hash in place of the arguments, answering as it does for
// the command itself; contexts without ActionReconciliation v1 refuse the hash form (an unknown
// member), and the read and the reconciliation need trd.read / evd.read now.
func TestGoalReadinessAndReconcileByHashOverHTTP(t *testing.T) {
	q := newOrderWorld(t)
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ed := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "editor", "human")}
	q.activeOrder("quote.1")
	const goal = "/api/brant/v1/trd/quotes/quote.1/goal_readiness"

	status, out := ed.do("GET", goal+"?goal=order_active&customer_id=customer.1", nil)
	assertions, _ := out["assertions"].([]any)
	if status != http.StatusOK || out["ok"] != true || out["outcome"] != trdapi.AnswerSatisfied || out["order_id"] != "order.quote.1" ||
		out["order_revision"] != float64(1) || out["goal_version"] != float64(1) || len(assertions) != 8 {
		t.Fatalf("order_active: %d %v", status, out)
	}
	first := assertions[0].(map[string]any)
	checks, _ := first["checks"].([]any)
	evidence, _ := first["evidence"].([]any)
	if first["assertion"] != trdapi.AssertionScope || first["answer"] != trdapi.AnswerSatisfied || len(checks) == 0 || len(evidence) == 0 ||
		evidence[0].(map[string]any)["kind"] != trdapi.EvidenceQuoteRoot {
		t.Fatalf("Q01.scope on the wire: %v", first)
	}
	status, out = ed.do("GET", goal+"?goal=quote_ready", nil)
	if status != http.StatusOK || out["outcome"] != trdapi.AnswerUnsatisfied || out["code"] != trdapi.CodeRequiredCheckUnsatisfied {
		t.Fatalf("quote_ready after the conversion: %d %v", status, out)
	}
	if status, out = ed.do("GET", goal+"?goal=quote_ready&goal_version=2", nil); status != http.StatusUnprocessableEntity ||
		out["code"] != kernel.CodeSchemaInvalid {
		t.Fatalf("goal version 2: %d %v", status, out)
	}
	if status, out = ed.do("GET", "/api/brant/v1/trd/quotes/quote.none/goal_readiness?goal=quote_ready", nil); status != http.StatusNotFound {
		t.Fatalf("a missing quote: %d %v", status, out)
	}

	// TRD's reconcile route by hash: quote.metadata.update on quote.r.
	q.drafted("quote.r")
	meta := trdapi.UpdateMetadata{IdempotencyKey: "rec.1", QuoteID: "quote.r", ExpectedVersion: 1, Labels: []string{"vip"}}
	req, err := q.c.TRD.RequestOf(meta)
	q.must(err)
	const route = "/api/brant/v1/trd/commands/quote.metadata.update"
	byHash := func(key, hash string) map[string]any {
		return map[string]any{"contract": trdapi.Contract, "command": trdapi.CommandUpdateMetadata, "idempotency_key": key, "request_hash": hash}
	}
	if status, out = ed.do("POST", route+"/reconcile", byHash("rec.1", req.RequestHash)); status != http.StatusOK ||
		out["state"] != string(kernel.ReconcileNotCommitted) {
		t.Fatalf("by hash before the command: %d %v", status, out)
	}
	status, sent := ed.do("POST", route, map[string]any{"contract": trdapi.Contract, "command": trdapi.CommandUpdateMetadata,
		"idempotency_key": "rec.1", "args": map[string]any{"quote_id": "quote.r", "expected_version": 1, "labels": []string{"vip"}}})
	if status != http.StatusOK {
		t.Fatalf("metadata.update over HTTP: %d %v", status, sent)
	}
	status, out = ed.do("POST", route+"/reconcile", byHash("rec.1", req.RequestHash))
	stored, _ := out["receipt"].(map[string]any)
	if status != http.StatusOK || out["state"] != string(kernel.ReconcileCommitted) || out["action_id"] != sent["action_id"] ||
		stored["row_version"] != sent["row_version"] {
		t.Fatalf("by hash after the command (the HTTP command hashes as RequestOf): %d %v, sent %v", status, out, sent)
	}
	if status, out = ed.do("POST", route+"/reconcile", byHash("rec.1", strings.Repeat("0", 64))); status != http.StatusOK ||
		out["state"] != string(kernel.ReconcilePayloadConflict) || out["receipt"] != nil {
		t.Fatalf("another hash: %d %v", status, out)
	}
	if status, out = ed.do("POST", route+"/reconcile", byHash("rec.1", "XYZ")); status != http.StatusUnprocessableEntity ||
		out["code"] != kernel.CodeSchemaInvalid {
		t.Fatalf("a malformed hash: %d %v", status, out)
	}

	// EVD reconciles over HTTP for the first time, by command and by hash.
	register := map[string]any{"contract": evdapi.Contract, "command": evdapi.CommandRegisterSource, "idempotency_key": "rec.src",
		"args": map[string]any{"id": "src.http", "content_text": "合成来源", "source_kind": evdapi.KindManualText}}
	if status, sent = ed.do("POST", "/api/brant/v1/evd/commands/source.register", register); status != http.StatusOK {
		t.Fatalf("source.register: %d %v", status, sent)
	}
	src, err := q.c.EVD.RequestOf(evdapi.RegisterSource{IdempotencyKey: "rec.src", SourceID: "src.http", ContentText: "合成来源",
		SourceKind: evdapi.KindManualText})
	q.must(err)
	for name, body := range map[string]map[string]any{"the command": register, "the hash": {"contract": evdapi.Contract,
		"command": evdapi.CommandRegisterSource, "idempotency_key": "rec.src", "request_hash": src.RequestHash}} {
		if status, out = ed.do("POST", "/api/brant/v1/evd/commands/source.register/reconcile", body); status != http.StatusOK ||
			out["state"] != string(kernel.ReconcileCommitted) || out["action_id"] != sent["action_id"] {
			t.Fatalf("EVD by %s: %d %v", name, status, out)
		}
	}

	// A context without ActionReconciliation v1 refuses the hash form (PUR: an unknown member).
	if status, out = ed.do("POST", "/api/brant/v1/pur/commands/purchase.draft.prepare/reconcile", map[string]any{"contract": purapi.Contract,
		"command": purapi.CommandPrepareDraft, "idempotency_key": "rec.pur", "request_hash": strings.Repeat("0", 64)}); status !=
		http.StatusUnprocessableEntity || out["code"] != kernel.CodeSchemaInvalid {
		t.Fatalf("PUR by hash: %d %v", status, out)
	}

	// Both need the read permission now (X-INV-003).
	q.regrant("editor", "company", without(allPermissions, "trd.read"))
	if status, out = ed.do("GET", goal+"?goal=order_active", nil); status != http.StatusForbidden || out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("the goal without trd.read: %d %v", status, out)
	}
	if status, out = ed.do("POST", route+"/reconcile", byHash("rec.1", req.RequestHash)); status != http.StatusForbidden {
		t.Fatalf("by hash without trd.read: %d %v", status, out)
	}
}
