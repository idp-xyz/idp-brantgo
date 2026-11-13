//go:build integration

package bootstrap_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
)

// BC-AT27 over HTTP (WBS S4-04b): a file uploaded, its extract registered (file_id; no text), the
// located form read with the file's access re-checked, an RFQ saved with field locators and a
// pending field, read back with its proposer, and confirmed with line verifications whose
// corrections the RFQ shows. A text with a file_extract and an unknown member are refused.
func TestFileExtractAndVerifiedLinesOverHTTP(t *testing.T) {
	w := newWorld(t)
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	w.actor("human.b4", allPermissions...)
	w.actor("outsider", allPermissions...)
	human := &client{t: t, base: api.URL, token: is.token(w.tenant, "company.A", "human.b4", "human")}
	outsider := &client{t: t, base: api.URL, token: is.token(w.tenant, "company.A", "outsider", "human")}

	human.command("evd", "brant.evd/0.1.0", "source.register", map[string]any{"id": "src.1", "source_kind": "manual_text", "content_text": "合成主数据"})
	human.command("evd", "brant.evd/0.1.0", "file.register", map[string]any{"id": "file.inq", "file_name": "inquiry.xlsx",
		"mime_type": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "content_base64": base64.StdEncoding.EncodeToString(inquiryWorkbook())})
	if status, out := human.try("evd", "brant.evd/0.1.0", "source.register", map[string]any{"id": "src.inq", "source_kind": "file_extract",
		"file_id": "file.inq", "content_text": "ST01 9999"}); status != http.StatusUnprocessableEntity || out["code"] != "SCHEMA_INVALID" {
		t.Fatalf("text with a file extract: %d %v", status, out)
	}
	human.command("evd", "brant.evd/0.1.0", "source.register", map[string]any{"id": "src.inq", "source_kind": "file_extract", "file_id": "file.inq"})
	status, x := human.do("GET", "/api/brant/v1/evd/sources/src.inq/extract", nil)
	extract, _ := x["extract"].(map[string]any)
	if status != http.StatusOK || extract["file_id"] != "file.inq" || extract["extractor"] != "brant.ingest.xlsx/1" || extract["kind"] != "xlsx" ||
		!strings.Contains(x["located_form"].(string), "segment\t询价单!C5\t48\n") {
		t.Fatalf("extract %d %v", status, x)
	}
	if status, out := outsider.do("GET", "/api/brant/v1/evd/sources/src.inq/extract", nil); status != http.StatusNotFound {
		t.Fatalf("outsider %d %v", status, out)
	}
	if status, s := human.do("GET", "/api/brant/v1/evd/sources/src.inq", nil); status != http.StatusOK || s["source_kind"] != "file_extract" ||
		s["extract"].(map[string]any)["extract_sha256"] != extract["extract_sha256"] {
		t.Fatalf("source %d %v", status, s)
	}

	human.command("pty", "brant.pty/0.1.0", "party.save_draft", map[string]any{"id": "customer.1", "expected_version": 0, "name": "customer.1", "roles": []string{"customer"}})
	human.command("pty", "brant.pty/0.1.0", "party.confirm", map[string]any{"id": "customer.1", "expected_version": 1, "evidence_ids": []string{"src.1"}})
	human.command("pty", "brant.pty/0.1.0", "contact.save_draft", map[string]any{"id": "contact.1", "expected_version": 0, "name": "contact.1", "party_id": "customer.1"})
	human.command("pty", "brant.pty/0.1.0", "contact.confirm", map[string]any{"id": "contact.1", "expected_version": 1, "evidence_ids": []string{"src.1"}})
	human.command("cat", "brant.cat/0.1.0", "sku.save_draft", map[string]any{"id": "ST01.blue.S", "expected_version": 0, "name": "ST01.blue.S", "base_uom": "PCS"})
	human.command("cat", "brant.cat/0.1.0", "sku.confirm", map[string]any{"id": "ST01.blue.S", "expected_version": 1, "evidence_ids": []string{"src.1"}})
	line := map[string]any{"line_id": "L4", "sku_id": "ST01.blue.S", "quantity": "48", "uom": "PCS", "requested_item": "针织T恤 蓝 S",
		"source_id": "src.inq", "locator": "询价单 第5行",
		"locators": []map[string]any{{"field": "quantity", "locator": "询价单!C5", "quote": "48"}},
		"pending":  []map[string]any{{"field": "quantity", "reason": "conflict", "candidates": []string{"48", "46"}}}}
	if status, out := human.try("inq", "brant.inq/0.1.0", "rfq.save_draft", map[string]any{"id": "rfq.http", "expected_version": 0,
		"lines": []map[string]any{{"line_id": "L1", "locators": []map[string]any{{"field": "quantity", "locator": "1:1", "quote": "x", "verified": true}}}}}); status != http.StatusUnprocessableEntity || out["code"] != "SCHEMA_INVALID" {
		t.Fatalf("unknown member: %d %v", status, out)
	}
	human.command("inq", "brant.inq/0.1.0", "rfq.save_draft", map[string]any{"id": "rfq.http", "expected_version": 0, "customer_id": "customer.1",
		"contact_id": "contact.1", "source_ids": []string{"src.inq"}, "lines": []map[string]any{line}})
	if status, out := human.try("inq", "brant.inq/0.1.0", "rfq.confirm_scope", map[string]any{"id": "rfq.http", "expected_version": 1,
		"evidence_ids": []string{"src.inq"}}); status != http.StatusUnprocessableEntity || out["code"] != "DRAFT_NOT_READY" {
		t.Fatalf("pending conflict: %d %v", status, out)
	}
	human.command("inq", "brant.inq/0.1.0", "rfq.confirm_scope", map[string]any{"id": "rfq.http", "expected_version": 1,
		"evidence_ids": []string{"src.inq"}, "verified_lines": []map[string]any{{"line_id": "L4",
			"fields": []map[string]any{{"field": "quantity", "value": "48", "locator": "询价单!C5"}}}}})
	status, rfq := human.do("GET", "/api/brant/v1/inq/rfqs/rfq.http", nil)
	lines, _ := rfq["lines"].([]any)
	corrections, _ := rfq["corrections"].([]any)
	if status != http.StatusOK || rfq["status"] != "ready" || len(lines) != 1 || len(corrections) != 1 {
		t.Fatalf("rfq %d %v", status, rfq)
	}
	l := lines[0].(map[string]any)
	c := corrections[0].(map[string]any)
	if fmt.Sprint(l["locators"]) != "[map[field:quantity locator:询价单!C5 quote:48]]" || l["proposed_by"].(map[string]any)["id"] != "human.b4" ||
		c["kind"] != "verified" || c["reason"] != "conflict" || c["locator"] != "询价单!C5" || c["command"] != "rfq.confirm_scope" ||
		c["corrected_by"].(map[string]any)["id"] != "human.b4" {
		t.Fatalf("line %v correction %v", l, c)
	}
}
