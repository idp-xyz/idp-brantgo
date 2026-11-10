//go:build integration

package bootstrap_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// issuer is a local OpenID Connect issuer: discovery, a JWKS with one RSA key, and a signer.
type issuer struct {
	t      *testing.T
	srv    *httptest.Server
	signer jose.Signer
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("at+jwt").WithHeader("kid", "test-key"))
	if err != nil {
		t.Fatal(err)
	}
	is := &issuer{t: t, signer: signer}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": is.srv.URL, "jwks_uri": is.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}}})
	})
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	return is
}

// token returns an access token for actor of kind in tenant and company.
func (is *issuer) token(tenant, company, actor, kind string) string {
	is.t.Helper()
	now := time.Now()
	raw, err := jwt.Signed(is.signer).Claims(jwt.Claims{Issuer: is.srv.URL, Subject: actor, Audience: jwt.Audience{"brant-api"},
		Expiry: jwt.NewNumericDate(now.Add(5 * time.Minute)), IssuedAt: jwt.NewNumericDate(now)}).
		Claims(map[string]any{"brant_tenant": tenant, "brant_company": company, "brant_actor_kind": kind}).Serialize()
	if err != nil {
		is.t.Fatal(err)
	}
	return raw
}

type client struct {
	t     *testing.T
	base  string
	token string
	n     int
}

// do sends one request and decodes the JSON answer.
func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err = json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp.StatusCode, out
}

// command posts a command of bc and requires success.
func (c *client) command(bc, contract, command string, args map[string]any) map[string]any {
	c.t.Helper()
	status, out := c.try(bc, contract, command, args)
	if status != http.StatusOK || out["ok"] != true {
		c.t.Fatalf("%s: %d %v", command, status, out)
	}
	return out
}

func (c *client) try(bc, contract, command string, args map[string]any) (int, map[string]any) {
	c.n++
	return c.do("POST", "/api/brant/v1/"+bc+"/commands/"+command, map[string]any{
		"contract": contract, "command": command, "idempotency_key": "http." + string(rune('a'+c.n/26%26)) + string(rune('a'+c.n%26)), "args": args})
}

// S1-08 over HTTP: the front of BRC-001 with OIDC access tokens from a local issuer, as the Python
// demo runs it, plus the refusals the transport owns.
func TestBRC001FrontOverHTTPWithOIDCTokens(t *testing.T) {
	w := newWorld(t)
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	w.actor("human.demo", allPermissions...)
	w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("ai", "ai.demo"), Permissions: allPermissions, DataScope: "company"})
	human := &client{t: t, base: api.URL, token: is.token(w.tenant, "company.A", "human.demo", "human")}

	human.command("evd", "brant.evd/0.1.0", "source.register", map[string]any{"id": "source.1", "source_kind": "manual_text",
		"content_text": "合成样例：ST01四个SKU，24、36、12、48件，合计120 PCS。"})
	// file.register has its own body limit: a 1.1 MiB file passes the 1 MiB default.
	large := strings.Repeat("A", 1100*1024)
	human.command("evd", "brant.evd/0.1.0", "file.register", map[string]any{"id": "inquiry.file", "file_name": "inquiry.txt",
		"mime_type": "text/plain", "content_base64": base64.StdEncoding.EncodeToString([]byte(large))})
	if status, f := human.do("GET", "/api/brant/v1/evd/files/inquiry.file", nil); status != http.StatusOK ||
		f["content_sha256"] != fmt.Sprintf("%x", sha256.Sum256([]byte(large))) {
		t.Fatalf("file read %d", status)
	}
	human.command("pty", "brant.pty/0.1.0", "party.save_draft", map[string]any{"id": "customer.1", "expected_version": 0, "name": "customer.1", "roles": []string{"customer"}})
	human.command("pty", "brant.pty/0.1.0", "party.confirm", map[string]any{"id": "customer.1", "expected_version": 1, "evidence_ids": []string{"source.1"}})
	human.command("pty", "brant.pty/0.1.0", "contact.save_draft", map[string]any{"id": "contact.1", "expected_version": 0, "name": "contact.1", "party_id": "customer.1"})
	human.command("pty", "brant.pty/0.1.0", "contact.confirm", map[string]any{"id": "contact.1", "expected_version": 1, "evidence_ids": []string{"source.1"}})
	var lines []map[string]any
	for i, sku := range []string{"ST01.red.S", "ST01.red.M", "ST01.blue.S", "ST01.blue.M"} {
		human.command("cat", "brant.cat/0.1.0", "sku.save_draft", map[string]any{"id": sku, "expected_version": 0, "name": sku, "base_uom": "PCS"})
		human.command("cat", "brant.cat/0.1.0", "sku.confirm", map[string]any{"id": sku, "expected_version": 1, "evidence_ids": []string{"source.1"}})
		n := string(rune('1' + i))
		lines = append(lines, map[string]any{"line_id": "L" + n, "sku_id": sku, "quantity": []string{"24", "36", "12", "48"}[i], "uom": "PCS",
			"source_id": "source.1", "locator": "第1段，第" + n + "项"})
	}
	saved := human.command("inq", "brant.inq/0.1.0", "rfq.save_draft", map[string]any{"id": "rfq.1", "expected_version": 0,
		"customer_id": "customer.1", "contact_id": "contact.1", "source_ids": []string{"source.1"}, "lines": lines})
	if saved["status"] != "draft" || saved["row_version"] != float64(1) {
		t.Fatalf("save %v", saved)
	}

	// An AI agent holding every permission may not confirm the scope.
	ai := &client{t: t, base: api.URL, token: is.token(w.tenant, "company.A", "ai.demo", "ai")}
	if status, out := ai.try("inq", "brant.inq/0.1.0", "rfq.confirm_scope", map[string]any{"id": "rfq.1", "expected_version": 1,
		"evidence_ids": []string{"source.1"}}); status != http.StatusForbidden || out["code"] != "PERMISSION_DENIED" {
		t.Fatalf("AI confirmation: %d %v", status, out)
	}
	ready := human.command("inq", "brant.inq/0.1.0", "rfq.confirm_scope", map[string]any{"id": "rfq.1", "expected_version": 1, "evidence_ids": []string{"source.1"}})
	if ready["status"] != "ready" || ready["row_version"] != float64(2) {
		t.Fatalf("confirm %v", ready)
	}
	status, rfq := human.do("GET", "/api/brant/v1/inq/rfqs/rfq.1", nil)
	if status != http.StatusOK || rfq["status"] != "ready" || len(rfq["lines"].([]any)) != 4 {
		t.Fatalf("rfq %d %v", status, rfq)
	}
	for i, q := range []string{"24", "36", "12", "48"} {
		if got := rfq["lines"].([]any)[i].(map[string]any)["quantity"]; got != q {
			t.Fatalf("line %d quantity %v", i, got)
		}
	}

	// No token, a token of another tenant, a forged signature.
	anonymous := &client{t: t, base: api.URL}
	if status, out := anonymous.do("GET", "/api/brant/v1/inq/rfqs/rfq.1", nil); status != http.StatusUnauthorized || out["code"] != "UNAUTHENTICATED" {
		t.Fatalf("anonymous %d %v", status, out)
	}
	stranger := &client{t: t, base: api.URL, token: is.token("tenant.other", "company.A", "human.demo", "human")}
	if status, out := stranger.do("GET", "/api/brant/v1/inq/rfqs/rfq.1", nil); status != http.StatusForbidden && status != http.StatusNotFound {
		t.Fatalf("another tenant must not read the RFQ: %d %v", status, out)
	}
	forged := &client{t: t, base: api.URL, token: newIssuer(t).token(w.tenant, "company.A", "human.demo", "human")}
	if status, _ := forged.do("GET", "/api/brant/v1/inq/rfqs/rfq.1", nil); status != http.StatusUnauthorized {
		t.Fatalf("a token of another issuer key: %d", status)
	}
	// POL over HTTP: the editor cannot publish its own draft (POL-INV-001); another human can.
	w.actor("reviewer.demo", allPermissions...)
	reviewer := &client{t: t, base: api.URL, token: is.token(w.tenant, "company.A", "reviewer.demo", "human")}
	from := kernel.FormatTimestamp(time.Now().Add(-time.Hour))
	human.command("pol", "brant.pol/0.1.0", "config.save_draft", map[string]any{"kind": "currency", "id": "USD", "expected_version": 0,
		"valid_from": from, "data": map[string]any{"code": "USD", "name": "US Dollar", "minor_units": 2, "rounding": "HALF_UP"}})
	publish := map[string]any{"kind": "currency", "id": "USD", "expected_version": 1, "revision": 1, "evidence_ids": []string{"source.1"}, "reason": "核对 ISO 4217"}
	if status, out := human.try("pol", "brant.pol/0.1.0", "config.publish", publish); status != http.StatusUnprocessableEntity ||
		out["code"] != "INDEPENDENT_REVIEW_REQUIRED" {
		t.Fatalf("self-publication: %d %v", status, out)
	}
	reviewer.command("pol", "brant.pol/0.1.0", "config.publish", publish)
	if status, cfg := human.do("GET", "/api/brant/v1/pol/configs/currency/USD", nil); status != http.StatusOK || cfg["active_revision"] != float64(1) {
		t.Fatalf("config %d %v", status, cfg)
	}
	if status, out := human.try("pol", "brant.pol/0.1.0", "config.save_draft", map[string]any{"kind": "currency", "id": "EUR",
		"expected_version": 0, "valid_from": from, "data": map[string]any{"code": "EUR", "name": "Euro", "minor_units": 2.5, "rounding": "HALF_UP"}}); status != http.StatusUnprocessableEntity || out["code"] != "SCHEMA_INVALID" {
		t.Fatalf("a float in config data: %d %v", status, out)
	}

	audit, err := w.c.INQ.Audit(w.actor("human.demo", allPermissions...))
	w.must(err)
	if last := audit[len(audit)-1]; last.Command != "rfq.get" || last.Outcome != "read" {
		t.Fatalf("the HTTP read is audited: %+v", last)
	}
	_ = context.Background
}
