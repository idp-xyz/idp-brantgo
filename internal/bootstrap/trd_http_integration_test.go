//go:build integration

package bootstrap_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S3 over HTTP with OIDC access tokens, on the TRD world: the editor drafts the fixture quote in its
// wire form, freezes it and asks for review; the reviewer approves; the editor activates, reads the
// formal document and records the customer's full acceptance; the reviewer verifies it. Every step
// goes through the command routes and every result is read back through the read routes.
func TestQuoteToAcceptanceOverHTTP(t *testing.T) {
	q := newTrdWorld(t)
	q.mail()
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	as := func(actor string, n int) *client {
		return &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", actor, "human"), n: n}
	}
	ed, rv := as("editor", 0), as("reviewer", 300)
	const trd = trdapi.Contract
	ref := func(r polapi.ConfigRef) map[string]any {
		return map[string]any{"kind": r.Kind, "id": r.ConfigID, "revision": r.Revision}
	}
	proof := []any{map[string]any{"source_id": "evidence", "locator": "合成核实依据"}}
	place := map[string]any{"kind": trdapi.PlaceKind, "id": "DEHAM", "revision": 1}
	data := map[string]any{
		"named_place_ref": place,
		"delivery": map[string]any{"kind": trdapi.DeliveryFixed, "date": "2027-01-31", "milestone": trdapi.MilestoneHandover, "place_ref": place,
			"time_zone": "Asia/Shanghai"},
		"issuer_ref":            ref(q.company),
		"customer_ref":          map[string]any{"party_id": "customer.1", "revision": 2},
		"contact_ref":           map[string]any{"contact_id": "contact.1", "row_version": 2},
		"trading_profile_ref":   map[string]any{"party_id": "customer.1", "revision": 1},
		"customer_address_refs": []any{map[string]any{"party_id": "customer.1", "party_revision": 2, "address_id": "hq"}},
		"rfq_ref":               map[string]any{"rfq_id": "rfq.1", "row_version": q.rfqVersion},
		"cost_snapshot_ref":     map[string]any{"snapshot_id": q.snap.SnapshotID, "result_hash": q.snap.ResultHash},
		"lines": []any{map[string]any{"line_id": "L1", "rfq_line_id": "L1", "sku_ref": map[string]any{"sku_id": "sku.1", "spec_revision": q.spec},
			"quantity": "120.000000", "unit_ref": ref(q.pcs), "unit_price": "8.0000", "price_basis_quantity": "1", "discount_ratio": "0.00",
			"tax_application_ref": ref(q.sale), "evidence": proof}},
		"currency_ref": ref(q.usd), "pricing_policy_ref": ref(q.policy), "trade_terms_ref": ref(q.trade), "payment_terms_ref": ref(q.payment),
		"document_template_ref": ref(q.template), "sequence_ref": ref(q.seq), "expires_at": kernel.FormatTimestamp(time.Now().Add(72 * time.Hour)),
		"customer_notes": "客户可见说明：合成样例\nKeep this line break.", "evidence": proof,
	}

	r := ed.command("trd", trd, trdapi.CommandSaveDraft, map[string]any{"quote_id": "quote.http", "expected_version": 0, "data": data})
	if r["object_id"] != "quote.http" || r["row_version"] != float64(1) || r["status"] != trdapi.StatusWorking {
		t.Fatalf("save_draft %v", r)
	}
	// The wire form is the fixture: the same draft through trd/api freezes to the same commercial content.
	q.frozenQuote("quote.api")
	r = ed.command("trd", trd, trdapi.CommandFreeze, map[string]any{"quote_id": "quote.http", "expected_version": 1, "working_revision": 1})
	if r["row_version"] != float64(2) || r["status"] != trdapi.StatusFrozen {
		t.Fatalf("freeze %v", r)
	}
	status, quote := ed.do("GET", "/api/brant/v1/trd/quotes/quote.http", nil)
	revision := quote["revisions"].([]any)[0].(map[string]any)
	hash, _ := revision["business_hash"].(string)
	if status != http.StatusOK || quote["ok"] != true || len(hash) != 64 ||
		revision["commercial"] == "" || revision["data"].(map[string]any)["lines"].([]any)[0].(map[string]any)["sku_ref"].(map[string]any)["sku_id"] != "sku.1" {
		t.Fatalf("quote %d %v", status, quote)
	}
	// Apart from the quote id and the instants, the commercial content is the same.
	sealed := func(commercial string) map[string]any {
		var c map[string]any
		q.must(json.Unmarshal([]byte(commercial), &c))
		delete(c, "quote_id")
		delete(c, "issued_at")
		delete(c["data"].(map[string]any), "expires_at")
		return c
	}
	api := q.getQuote("quote.api").Revisions[0]
	if http, direct := sealed(revision["commercial"].(string)), sealed(api.Commercial); !reflect.DeepEqual(http, direct) {
		t.Fatalf("the HTTP draft froze to other commercial content:\n%v\n--- trd/api ---\n%v", http, direct)
	}

	ed.command("trd", trd, trdapi.CommandSubmitReview, map[string]any{"quote_id": "quote.http", "expected_version": 2, "business_revision": 1,
		"review_request_id": "review.http", "target_command": trdapi.TargetActivate})
	decide := map[string]any{"quote_id": "quote.http", "expected_version": 3, "review_request_id": "review.http", "business_hash": hash,
		"decision": trdapi.DecisionApprove, "approval_id": "approval.http", "expires_at": kernel.FormatTimestamp(time.Now().Add(24 * time.Hour)),
		"reason": "内容核对无误", "evidence": proof}
	if status, out := ed.try("trd", trd, trdapi.CommandDecideReview, decide); status != http.StatusUnprocessableEntity || out["ok"] != false {
		t.Fatalf("the requester decided its own review: %d %v", status, out)
	}
	rv.command("trd", trd, trdapi.CommandDecideReview, decide)
	status, check := ed.do("GET", "/api/brant/v1/trd/quotes/quote.http/approvals/approval.http/check", nil)
	if status != http.StatusOK || check["releasable"] != true || check["business_hash"] != hash || check["target_command"] != trdapi.TargetActivate {
		t.Fatalf("approval check %d %v", status, check)
	}

	r = ed.command("trd", trd, trdapi.CommandActivate, map[string]any{"quote_id": "quote.http", "expected_version": 4, "business_revision": 1,
		"approval_id": "approval.http", "expected_approval_effectivity_version": 1})
	if r["row_version"] != float64(5) || r["status"] != trdapi.StatusActive {
		t.Fatalf("activate %v", r)
	}
	status, active := ed.do("GET", "/api/brant/v1/trd/quotes/quote.http/active", nil)
	if status != http.StatusOK || active["ref"].(map[string]any)["business_revision"] != float64(1) || active["business_hash"] != hash ||
		active["active"] != true || active["number"] != number(1) {
		t.Fatalf("active %d %v", status, active)
	}
	status, doc := ed.do("GET", "/api/brant/v1/trd/quotes/quote.http/revisions/1/document", nil)
	content, err := base64.StdEncoding.DecodeString(doc["content"].(string))
	q.must(err)
	sum := sha256.Sum256(content)
	formal := doc["formal"].(map[string]any)
	if status != http.StatusOK || doc["status"] != trdapi.StatusActive || formal["document_sha256"] != hex.EncodeToString(sum[:]) ||
		formal["number"] != number(1) || string(content) != q.rendered("quote.http", 1, number(1)) {
		t.Fatalf("document %d %v\n%s", status, formal, content)
	}
	for _, path := range []string{"/api/brant/v1/trd/quotes/quote.http/revisions/0/document", "/api/brant/v1/trd/quotes/quote.http/revisions/x/document"} {
		if status, out := ed.do("GET", path, nil); status != http.StatusUnprocessableEntity || out["code"] != kernel.CodeSchemaInvalid {
			t.Fatalf("%s: %d %v", path, status, out)
		}
	}

	r = ed.command("trd", trd, trdapi.CommandRecordResponse, map[string]any{"response_id": "response.http", "quote_id": "quote.http",
		"business_revision": 1, "business_hash": hash, "classification": trdapi.ClassificationFullUnconditional, "summary": "客户回复：全部接受",
		"source_ref": map[string]any{"source_id": "mail", "locator": "response.http"}, "received_at": kernel.FormatTimestamp(time.Now()),
		"received_at_basis": trdapi.ArrivalChannelServer})
	if r["object_id"] != "response.http" {
		t.Fatalf("response.record %v", r)
	}
	r = rv.command("trd", trd, trdapi.CommandVerifyResponse, map[string]any{"response_id": "response.http", "acceptance_id": "acceptance.http",
		"business_revision": 1, "business_hash": hash, "customer_ref": map[string]any{"party_id": "customer.1", "revision": 2},
		"contact_ref": map[string]any{"contact_id": "contact.1", "row_version": 2}, "line_ids": []string{"L1"}})
	if r["object_id"] != "acceptance.http" {
		t.Fatalf("response.verify %v", r)
	}
	status, response := ed.do("GET", "/api/brant/v1/trd/customer_responses/response.http", nil)
	if status != http.StatusOK || response["ok"] != true || response["classification"] != trdapi.ClassificationFullUnconditional {
		t.Fatalf("response %d %v", status, response)
	}
	status, acceptance := ed.do("GET", "/api/brant/v1/trd/acceptances/acceptance.http", nil)
	if status != http.StatusOK || acceptance["formed"] != true || acceptance["effectivity"] != trdapi.AcceptanceValid ||
		acceptance["number"] != number(1) || acceptance["verified_by"].(map[string]any)["id"] != "reviewer" {
		t.Fatalf("acceptance %d %v", status, acceptance)
	}
	if status, out := ed.do("GET", "/api/brant/v1/trd/acceptances/acceptance.none", nil); status != http.StatusNotFound || out["ok"] != false {
		t.Fatalf("unknown acceptance: %d %v", status, out)
	}
}

// Contract 06 动作对账 over HTTP: the caller posts the request it sent to the command's reconcile
// route and learns what became of it without executing anything (Python t03_reconciliation; T03
// A22).
func TestReconcileOverHTTP(t *testing.T) {
	q := newTrdWorld(t)
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ed := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "editor", "human")}
	envelope := func(key string, args map[string]any) map[string]any {
		return map[string]any{"contract": trdapi.Contract, "command": trdapi.CommandUpdateMetadata, "idempotency_key": key, "args": args}
	}
	q.drafted("quote.r")
	labels := map[string]any{"quote_id": "quote.r", "expected_version": 1, "labels": []string{"vip"}, "internal_note": "合成备注"}
	const command, reconcile = "/api/brant/v1/trd/commands/quote.metadata.update", "/api/brant/v1/trd/commands/quote.metadata.update/reconcile"

	status, out := ed.do("POST", reconcile, envelope("rec.1", labels))
	if status != http.StatusOK || out["ok"] != true || out["state"] != "not_committed" || out["action_id"] != nil || out["receipt"] != nil {
		t.Fatalf("before the command: %d %v", status, out)
	}
	status, sent := ed.do("POST", command, envelope("rec.1", labels))
	if status != http.StatusOK || sent["ok"] != true {
		t.Fatalf("metadata.update %d %v", status, sent)
	}
	status, out = ed.do("POST", reconcile, envelope("rec.1", labels))
	stored, _ := out["receipt"].(map[string]any)
	if status != http.StatusOK || out["state"] != "committed" || out["action_id"] != sent["action_id"] || out["object_id"] != "quote.r" ||
		stored["action_id"] != sent["action_id"] || stored["row_version"] != sent["row_version"] || stored["status"] != sent["status"] {
		t.Fatalf("after the command: %d %v (sent %v)", status, out, sent)
	}
	if v := q.getQuote("quote.r"); v.RootVersion != 2 {
		t.Fatalf("reconciling executed the command: root version %d", v.RootVersion)
	}
	changed := map[string]any{"quote_id": "quote.r", "expected_version": 1, "labels": []string{"other"}}
	if status, out = ed.do("POST", reconcile, envelope("rec.1", changed)); status != http.StatusOK || out["state"] != "payload_conflict" ||
		out["action_id"] != sent["action_id"] || out["receipt"] != nil {
		t.Fatalf("another request under the key: %d %v", status, out)
	}

	// The request is checked as the command checks it; contexts without reconciliation have no route.
	wrong := envelope("rec.2", labels)
	wrong["contract"] = "brant.other/0.1.0"
	if status, out = ed.do("POST", reconcile, wrong); status != http.StatusBadRequest || out["code"] != kernel.CodeUnsupportedContract {
		t.Fatalf("wrong contract: %d %v", status, out)
	}
	if status, out = ed.do("POST", reconcile, envelope("rec.2", map[string]any{"quote_id": "quote.r", "expected_version": 1, "colour": "red"})); status != http.StatusUnprocessableEntity ||
		out["code"] != kernel.CodeSchemaInvalid {
		t.Fatalf("unknown member: %d %v", status, out)
	}
	if status, out = ed.do("POST", "/api/brant/v1/pty/commands/party.save_draft/reconcile", map[string]any{}); status != http.StatusNotFound {
		t.Fatalf("pty reconcile: %d %v", status, out)
	}

	// Reconciling needs trd.read now (X-INV-003).
	q.regrant("editor", "company", without(allPermissions, "trd.read"))
	if status, out = ed.do("POST", reconcile, envelope("rec.1", labels)); status != http.StatusForbidden || out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("without trd.read: %d %v", status, out)
	}
}
