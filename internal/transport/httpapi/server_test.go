package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	"go.idp.xyz/idp-bento-go/eventing"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC) }

// tokens maps bearer header values to principals.
type tokens map[string]identity.Principal

func (t tokens) Verify(_ context.Context, header string) (identity.Principal, error) {
	p, ok := t[header]
	if !ok {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	return p, nil
}

var alice = identity.Principal{Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "alice"}, Scope: kernel.Scope{Tenant: "t", Company: "c"}}

// fakeINQ records the commands it receives; methods the tests do not use are nil and would panic.
type fakeINQ struct {
	INQ
	saved   []inqapi.SaveRfq
	audited []string
	err     error
}

func (f *fakeINQ) SaveRfq(ctx context.Context, cmd inqapi.SaveRfq) (inqapi.Receipt, error) {
	if p, err := identity.FromContext(ctx); err != nil || p != alice {
		return inqapi.Receipt{}, errors.New("principal did not reach the context")
	}
	f.saved = append(f.saved, cmd)
	if f.err != nil {
		return inqapi.Receipt{}, f.err
	}
	return inqapi.Receipt{ActionID: "act-1", ObjectID: cmd.RfqID, RowVersion: 1, Status: "draft"}, nil
}

func (f *fakeINQ) AuditRejected(_ context.Context, command, _, code string) error {
	f.audited = append(f.audited, command+" "+code)
	return nil
}

func (f *fakeINQ) GetRfq(_ context.Context, id string) (inqapi.RfqView, error) {
	if id != "rfq.1" {
		return inqapi.RfqView{}, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	return inqapi.RfqView{RfqID: id, Status: "draft", RowVersion: 1, Lines: []inqapi.RfqLineView{{RfqLine: inqapi.RfqLine{LineID: "L1", Quantity: "120"}}},
		SourceIDs: []string{}, UnresolvedFields: []string{}}, nil
}

func newServer(t *testing.T, inq *fakeINQ) http.Handler {
	t.Helper()
	h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: inq, POL: struct{ POL }{}, REF: struct{ REF }{}, SUP: struct{ SUP }{}, CST: struct{ CST }{}, TRD: struct{ TRD }{}, PUR: struct{ PUR }{}, QLT: struct{ QLT }{},
		Verifier: tokens{"Bearer good": alice}, Clock: application.Clock(fixedClock{})})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type answer struct {
	status int
	body   map[string]any
}

func call(t *testing.T, h http.Handler, method, path, auth, body string) answer {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: body %q: %v", method, path, rec.Body.String(), err)
	}
	if rec.Header().Get("X-Correlation-ID") == "" {
		t.Fatal("every response carries a correlation ID")
	}
	return answer{rec.Code, out}
}

const saveRoute = "/api/brant/v1/inq/commands/rfq.save_draft"

func request(args string) string {
	return `{"contract":"brant.inq/0.1.0","command":"rfq.save_draft","idempotency_key":"k1","args":` + args + `}`
}

func TestCommandReachesTheContextWithTheTokenPrincipal(t *testing.T) {
	inq := &fakeINQ{}
	a := call(t, newServer(t, inq), "POST", saveRoute, "Bearer good",
		request(`{"id":"rfq.1","expected_version":0,"lines":[{"line_id":"L1","quantity":"120"}],"source_ids":[]}`))
	if a.status != 200 || a.body["ok"] != true || a.body["object_id"] != "rfq.1" || a.body["replayed"] != false {
		t.Fatalf("%d %v", a.status, a.body)
	}
	// Python test_contracts.py test_closed_response_disallows_forged_fields: the success branch is closed.
	if len(a.body) != 6 {
		t.Fatalf("success keys %v", a.body)
	}
	got := inq.saved[0]
	if got.IdempotencyKey != "k1" || got.Lines[0].Quantity != "120" || got.SourceIDs == nil || got.UnresolvedFields != nil {
		t.Fatalf("command %+v", got)
	}
}

// Python test_null_extra_scope_and_float_are_schema_invalid, test_cli.py
// test_local_execute_rejects_duplicate_keys_and_size_before_parsing and
// test_limits_unknown_contract_and_sensitive_audit (unknown contract).
func TestEnvelopeRejections(t *testing.T) {
	for _, c := range []struct {
		name, body string
		status     int
		code, path string
	}{
		{"null", request(`{"id":"r","expected_version":0,"customer_id":null}`), 422, "SCHEMA_INVALID", ""},
		{"extra field", request(`{"id":"r","expected_version":0,"status":"ready"}`), 422, "SCHEMA_INVALID", ""},
		{"scope in body", request(`{"id":"r","expected_version":0,"tenant_id":"other"}`), 422, "SCHEMA_INVALID", ""},
		{"float", request(`{"id":"r","expected_version":0,"lines":[{"line_id":"L1","quantity":120.0}]}`), 422, "SCHEMA_INVALID", ""},
		{"duplicate key", `{"contract":"brant.inq/0.1.0","command":"x","command":"rfq.save_draft","idempotency_key":"k","args":{}}`, 422, "SCHEMA_INVALID", ""},
		{"unknown contract", `{"contract":"unknown","command":"rfq.save_draft","idempotency_key":"k","args":{}}`, 400, "UNSUPPORTED_CONTRACT", "/contract"},
		{"command differs from route", `{"contract":"brant.inq/0.1.0","command":"rfq.close","idempotency_key":"k","args":{}}`, 400, "UNSUPPORTED_CONTRACT", "/command"},
		{"member name case", request(`{"ID":"r","expected_version":0}`), 422, "SCHEMA_INVALID", ""},
		{"nested float", request(`{"id":"r","expected_version":0.0}`), 422, "SCHEMA_INVALID", ""},
		{"empty string", request(`{"id":"r","expected_version":0,"customer_id":""}`), 422, "SCHEMA_INVALID", "/args/customer_id"},
		{"missing version", request(`{"id":"r"}`), 422, "SCHEMA_INVALID", "/args/expected_version"},
		{"wrong type", request(`{"id":"r","expected_version":"0"}`), 422, "SCHEMA_INVALID", "/args/expected_version"},
		{"missing args", `{"contract":"brant.inq/0.1.0","command":"rfq.save_draft","idempotency_key":"k"}`, 422, "SCHEMA_INVALID", "/args"},
		{"too large", request(`{"id":"r","expected_version":0,"unresolved_fields":["` + strings.Repeat("x", MaxBody) + `"]}`), 413, "REQUEST_TOO_LARGE", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			inq := &fakeINQ{}
			a := call(t, newServer(t, inq), "POST", saveRoute, "Bearer good", c.body)
			fields, _ := a.body["field_errors"].([]any)
			if a.status != c.status || a.body["code"] != c.code || a.body["ok"] != false || a.body["retryable"] != false || len(fields) != 1 {
				t.Fatalf("%d %v", a.status, a.body)
			}
			if p := fields[0].(map[string]any)["path"]; c.path != "" && p != c.path {
				t.Fatalf("path %v, want %s", p, c.path)
			}
			if len(inq.saved) != 0 || len(inq.audited) != 1 || inq.audited[0] != "rfq.save_draft "+c.code {
				t.Fatalf("the context must not see the command, only audit the refusal: %v %v", inq.saved, inq.audited)
			}
		})
	}
}

func TestUnauthenticatedRequestsAreRefusedBeforeAnything(t *testing.T) {
	inq := &fakeINQ{}
	h := newServer(t, inq)
	for _, auth := range []string{"", "Bearer forged"} {
		a := call(t, h, "POST", saveRoute, auth, request(`{"id":"r","expected_version":0}`))
		if a.status != 401 || a.body["code"] != "UNAUTHENTICATED" {
			t.Fatalf("%q: %d %v", auth, a.status, a.body)
		}
	}
	if len(inq.saved)+len(inq.audited) != 0 {
		t.Fatal("an unauthenticated request must not reach the context")
	}
	if a := call(t, h, "GET", "/healthz", "", ""); a.status != 200 {
		t.Fatalf("health %d", a.status)
	}
}

// The status follows the rejection's category; a context's own rejection is not audited twice;
// an infrastructure failure reveals nothing.
func TestContextOutcomesMapByCategory(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", ""), 409, "VERSION_CONFLICT"},
		{kernel.Reject(kernel.CodePermissionDenied, "/", ""), 403, "PERMISSION_DENIED"},
		{kernel.Reject(kernel.CodeNotFound, "/args/id", ""), 404, "NOT_FOUND"},
		{kernel.Reject(kernel.CodeDraftNotReady, "/args/lines", ""), 422, "DRAFT_NOT_READY"},
		{errors.New("connection refused to 10.0.0.1"), 503, ""},
		{errors.Join(application.ErrCommitUncertain, errors.New("eof")), 503, ""},
	} {
		inq := &fakeINQ{err: c.err}
		a := call(t, newServer(t, inq), "POST", saveRoute, "Bearer good", request(`{"id":"r","expected_version":0}`))
		if a.status != c.status || (c.code != "" && a.body["code"] != c.code) {
			t.Fatalf("%v: %d %v", c.err, a.status, a.body)
		}
		if c.code == "" && (a.body["transport_error"] == nil || strings.Contains(strings.Join([]string{a.body["transport_error"].(string)}, ""), "10.0.0.1")) {
			t.Fatalf("transport error %v", a.body)
		}
		if len(inq.audited) != 0 {
			t.Fatalf("the context audits its own rejections: %v", inq.audited)
		}
	}
}

func TestReadsAnswerViewsAndRejections(t *testing.T) {
	h := newServer(t, &fakeINQ{})
	a := call(t, h, "GET", "/api/brant/v1/inq/rfqs/rfq.1", "Bearer good", "")
	if a.status != 200 || a.body["rfq_id"] != "rfq.1" || a.body["customer_id"] != nil {
		t.Fatalf("%d %v", a.status, a.body)
	}
	if a = call(t, h, "GET", "/api/brant/v1/inq/rfqs/other", "Bearer good", ""); a.status != 404 || a.body["code"] != "NOT_FOUND" {
		t.Fatalf("%d %v", a.status, a.body)
	}
	if a = call(t, h, "POST", "/api/brant/v1/xyz/commands/rfq.save_draft", "Bearer good", request(`{}`)); a.status != 400 {
		t.Fatalf("unknown context %d %v", a.status, a.body)
	}
}

type fakeReplayer struct{ err error }

func (f fakeReplayer) Replay(_ context.Context, source, eventID string) (messaging.ReplayResult, error) {
	return messaging.ReplayResult{Source: source, EventID: eventing.EventID(eventID), AuthorityEpoch: 3, Attempts: 1}, f.err
}

// ADR-BR-015: the replay route maps each outcome to a status and never reveals the cause.
func TestDeadLetterReplayRoute(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{nil, 200, ""},
		{authorization.ErrActorKindDenied, 403, "PERMISSION_DENIED"},
		{messaging.ErrDeadLetterNotFound, 404, "NOT_FOUND"},
		{messaging.ErrAlreadyReplayed, 422, "INVALID_STATE"},
		{messaging.ErrReplayFailed, 502, ""},
	} {
		h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: &fakeINQ{}, POL: struct{ POL }{}, REF: struct{ REF }{}, SUP: struct{ SUP }{}, CST: struct{ CST }{}, TRD: struct{ TRD }{}, PUR: struct{ PUR }{}, QLT: struct{ QLT }{},
			Verifier: tokens{"Bearer good": alice}, Clock: application.Clock(fixedClock{}), Replayer: fakeReplayer{c.err}})
		if err != nil {
			t.Fatal(err)
		}
		a := call(t, h, "POST", "/api/brant/v1/msg/dead_letters/idp-brant.inq/evt-1/replay", "Bearer good", "")
		if a.status != c.status || (c.code != "" && a.body["code"] != c.code) || (c.err == nil && a.body["event_id"] != "evt-1") {
			t.Fatalf("%v: %d %v", c.err, a.status, a.body)
		}
	}
	if a := call(t, newServer(t, &fakeINQ{}), "POST", "/api/brant/v1/msg/dead_letters/x/y/replay", "Bearer good", ""); a.status != 404 || a.body["code"] != "NOT_FOUND" {
		t.Fatalf("no replayer configured: %d", a.status)
	}
}

// fakeREF records the list imports it receives.
type fakeREF struct {
	REF
	entries int
}

func (f *fakeREF) ImportScreeningList(_ context.Context, cmd refapi.ImportScreeningList) (refapi.Receipt, error) {
	f.entries = len(cmd.Entries)
	return refapi.Receipt{ActionID: "act-1", ObjectID: refapi.ObjectID(cmd.ListCode, cmd.Version), RowVersion: 1, Status: refapi.StatusImported}, nil
}

func (f *fakeREF) AuditRejected(context.Context, string, string, string) error { return nil }

// screening_list.import has its own ingress limits: a list beyond 1 MiB and 100,000 JSON values is
// admitted there and nowhere else.
func TestListImportHasItsOwnIngressLimits(t *testing.T) {
	ref := &fakeREF{}
	h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: &fakeINQ{}, POL: struct{ POL }{}, REF: ref, SUP: struct{ SUP }{}, CST: struct{ CST }{}, TRD: struct{ TRD }{}, PUR: struct{ PUR }{}, QLT: struct{ QLT }{},
		Verifier: tokens{"Bearer good": alice}, Clock: application.Clock(fixedClock{})})
	if err != nil {
		t.Fatal(err)
	}
	var entries strings.Builder
	const n = 30000 // 7 values each: 210,000 values, about 3 MiB
	for i := range n {
		if i > 0 {
			entries.WriteByte(',')
		}
		fmt.Fprintf(&entries, `{"entry_id":"E%05d","primary_name":"Synthetic entity %05d with a long registered name","aliases":["Alias %05d"],"entity_type":"entity","country":"CN"}`, i, i, i)
	}
	args := `{"list_code":"un.sc","jurisdiction":"UN","version":"v1","source_name":"s","source_url":"https://example.org/l","published_on":"2026-11-01",` +
		`"file_sha256":"` + strings.Repeat("a", 64) + `","entry_count":30000,"entries":[` + entries.String() + `]}`
	if len(args) <= MaxBody {
		t.Fatalf("args are %d bytes", len(args))
	}
	body := func(bc, contract, command string) string {
		return `{"contract":"` + contract + `","command":"` + command + `","idempotency_key":"k1","args":` + args + `}`
	}
	a := call(t, h, "POST", "/api/brant/v1/ref/commands/screening_list.import", "Bearer good", body("ref", refapi.Contract, refapi.CommandImportScreeningList))
	if a.status != 200 || a.body["object_id"] != "un.sc:v1" || ref.entries != n {
		t.Fatalf("%d %v (%d entries)", a.status, a.body, ref.entries)
	}
	a = call(t, h, "POST", saveRoute, "Bearer good", body("inq", inqapi.Contract, inqapi.CommandSaveRfq))
	if a.status != 413 || a.body["code"] != "REQUEST_TOO_LARGE" {
		t.Fatalf("%d %v", a.status, a.body)
	}
}
