package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"go.idp.xyz/idp-bento-go/application"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// openINQ accepts any principal, so a system token can be shown to reach the command.
type openINQ struct{ fakeINQ }

func (f *openINQ) SaveRfq(_ context.Context, cmd inqapi.SaveRfq) (inqapi.Receipt, error) {
	f.saved = append(f.saved, cmd)
	return inqapi.Receipt{ActionID: "act-1", ObjectID: cmd.RfqID, RowVersion: 1, Status: "draft"}, nil
}

func principalServer(t *testing.T, inq INQ, replay DeadLetterReplayer) http.Handler {
	t.Helper()
	system := identity.Principal{Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "system.1"}, Scope: alice.Scope}
	h, err := New(Config{
		EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: inq,
		POL: struct{ POL }{}, REF: struct{ REF }{}, SUP: struct{ SUP }{}, CST: struct{ CST }{},
		TRD: struct{ TRD }{}, PUR: struct{ PUR }{},
		Verifier: tokens{"Bearer human": alice, "Bearer ai": agentAI, "Bearer system": system},
		Clock:    application.Clock(fixedClock{}), Replayer: replay,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func assertAIRefused(t *testing.T, a answer) {
	t.Helper()
	fields, _ := a.body["field_errors"].([]any)
	if a.status != http.StatusForbidden || a.body["code"] != kernel.CodePermissionDenied || a.body["ok"] != false || len(fields) != 1 {
		t.Fatalf("%d %v", a.status, a.body)
	}
	got := fields[0].(map[string]any)
	if got["path"] != "/" || got["reason"] != aiWritingRouteReason {
		t.Fatalf("field %v", got)
	}
}

func assertAIRefusedOn(t *testing.T, method, path string, a answer) {
	t.Helper()
	fields, _ := a.body["field_errors"].([]any)
	if a.status != http.StatusForbidden || a.body["code"] != kernel.CodePermissionDenied || a.body["ok"] != false || len(fields) != 1 {
		t.Fatalf("%s %s: %d %v", method, path, a.status, a.body)
	}
	got := fields[0].(map[string]any)
	if got["path"] != "/" || got["reason"] != aiWritingRouteReason {
		t.Fatalf("%s %s: field %v", method, path, got)
	}
}

const aiBody = `{"contract":"brant.inq/0.1.0","command":"rfq.save_draft","idempotency_key":"k1","args":{"id":"rfq.1","expected_version":0,"lines":[{"line_id":"L1","quantity":"120"}],"source_ids":[]}}`

// An AI bearer is refused on the command route before the body is read: a known command, an unknown
// command, an unknown context, a malformed body and an oversized body are all PERMISSION_DENIED, and
// the command function is not called. A known context audits the command as the route names it, with
// an empty object id; an unknown context has no auditor.
func TestAICommandRouteRefusesBeforeTheBody(t *testing.T) {
	huge := request(`{"id":"r","expected_version":0,"unresolved_fields":["` + strings.Repeat("x", MaxBody) + `"]}`)
	for _, c := range []struct {
		name, path, body, audited string
	}{
		{"known command", saveRoute, aiBody, "rfq.save_draft " + kernel.CodePermissionDenied},
		{"unknown command", "/api/brant/v1/inq/commands/no.such", aiBody, "invalid " + kernel.CodePermissionDenied},
		{"unknown context", "/api/brant/v1/xyz/commands/rfq.save_draft", aiBody, ""},
		{"malformed body", saveRoute, `{`, "rfq.save_draft " + kernel.CodePermissionDenied},
		{"oversized body", saveRoute, huge, "rfq.save_draft " + kernel.CodePermissionDenied},
	} {
		t.Run(c.name, func(t *testing.T) {
			inq := &fakeINQ{}
			a := call(t, principalServer(t, inq, nil), "POST", c.path, "Bearer ai", c.body)
			assertAIRefused(t, a)
			if len(inq.saved) != 0 {
				t.Fatalf("the command ran: %+v", inq.saved)
			}
			switch {
			case c.audited == "" && len(inq.audited) != 0:
				t.Fatalf("an unknown context was audited: %v", inq.audited)
			case c.audited != "" && (len(inq.audited) != 1 || inq.audited[0] != c.audited):
				t.Fatalf("audit %v, want %s", inq.audited, c.audited)
			}
		})
	}
	inq := &fakeINQ{}
	a := call(t, principalServer(t, inq, nil), "POST", saveRoute, "", aiBody)
	if a.status != http.StatusUnauthorized || a.body["code"] != kernel.CodeUnauthenticated || len(inq.audited) != 0 || len(inq.saved) != 0 {
		t.Fatalf("unauthenticated %d %v audited %v", a.status, a.body, inq.audited)
	}
}

// The reconcile route refuses an AI actor before the body is read and does not audit the refusal,
// which is how that route treats its other transport refusals. A human still gets the route's own
// answer when the command is not reconcilable.
func TestAIReconcileRouteRefusesBeforeTheBody(t *testing.T) {
	inq := &fakeINQ{}
	h := principalServer(t, inq, nil)
	for _, c := range []struct{ name, path, body string }{
		{"known command", saveRoute + "/reconcile", `{`},
		{"unknown command", "/api/brant/v1/inq/commands/no.such/reconcile", `{}`},
		{"unknown context", "/api/brant/v1/xyz/commands/rfq.save_draft/reconcile", `{}`},
		{"oversized body", saveRoute + "/reconcile", strings.Repeat("x", MaxBody+1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := call(t, h, "POST", c.path, "Bearer ai", c.body)
			assertAIRefused(t, a)
		})
	}
	if len(inq.saved) != 0 || len(inq.audited) != 0 {
		t.Fatalf("reconcile wrote or audited: saved %v audited %v", inq.saved, inq.audited)
	}
	a := call(t, h, "POST", "/api/brant/v1/inq/commands/no.such/reconcile", "Bearer human", `{}`)
	if a.status != http.StatusNotFound || a.body["code"] != kernel.CodeNotFound {
		t.Fatalf("human unknown reconcile %d %v", a.status, a.body)
	}
	a = call(t, h, "POST", "/api/brant/v1/xyz/commands/rfq.save_draft", "Bearer human", aiBody)
	if a.status != http.StatusBadRequest || a.body["code"] != kernel.CodeUnsupportedContract {
		t.Fatalf("human unknown context %d %v", a.status, a.body)
	}
}

// A human and a system bearer still reach the command. A read still answers an AI token.
func TestHumanAndSystemBearersAndAIReadsAreUnchanged(t *testing.T) {
	inq := &openINQ{}
	h := principalServer(t, inq, nil)
	body := request(`{"id":"rfq.1","expected_version":0,"lines":[{"line_id":"L1","quantity":"120"}],"source_ids":[]}`)
	for _, auth := range []string{"Bearer human", "Bearer system"} {
		a := call(t, h, "POST", saveRoute, auth, body)
		if a.status != http.StatusOK || a.body["object_id"] != "rfq.1" {
			t.Fatalf("%s: %d %v", auth, a.status, a.body)
		}
	}
	if len(inq.saved) != 2 {
		t.Fatalf("saved %d", len(inq.saved))
	}
	a := call(t, h, "GET", "/api/brant/v1/inq/rfqs/rfq.1", "Bearer ai", "")
	if a.status != http.StatusOK || a.body["ok"] != true || a.body["rfq_id"] != "rfq.1" {
		t.Fatalf("AI read %d %v", a.status, a.body)
	}
	if len(inq.audited) != 0 {
		t.Fatalf("a read or a successful command was refused: %v", inq.audited)
	}
}

type countingReplay struct{ n int }

func (c *countingReplay) Replay(context.Context, string, string) (messaging.ReplayResult, error) {
	c.n++
	return messaging.ReplayResult{}, errors.New("replayer ran")
}

// The dead-letter route refuses an AI actor before the replayer runs, and does not audit it.
func TestAIReplayRouteRefusesBeforeTheReplayer(t *testing.T) {
	inq := &fakeINQ{}
	replay := &countingReplay{}
	a := call(t, principalServer(t, inq, replay), "POST", "/api/brant/v1/msg/dead_letters/idp-brant.inq/evt-1/replay", "Bearer ai", "")
	assertAIRefused(t, a)
	if replay.n != 0 || len(inq.audited) != 0 {
		t.Fatalf("replay calls %d, audit %v", replay.n, inq.audited)
	}
}

const taskStepReason = "task steps here are people's; an AI agent's go through the AI gateway"

// On the task-step route an AI actor is refused before the body, audited when the route exists.
// A system actor is still refused after the body, with the route's own words.
func TestAITaskStepIsRefusedBeforeTheBody(t *testing.T) {
	trd := &stepTRD{}
	ai := &fakeAI{}
	system := identity.Principal{Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "system.1"}, Scope: alice.Scope}
	h, err := New(Config{
		EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: &fakeINQ{},
		POL: struct{ POL }{}, REF: struct{ REF }{}, SUP: struct{ SUP }{}, CST: struct{ CST }{},
		TRD: trd, PUR: struct{ PUR }{}, TSK: &fakeTSK{}, AI: ai,
		Verifier: tokens{"Bearer ai": agentAI, "Bearer system": system},
		Clock:    application.Clock(fixedClock{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	route := "/api/brant/v1/tsk/tasks/task.1/steps/trd/quote.freeze"
	body := `{"contract":"` + trdapi.Contract + `","command":"quote.freeze","business_intent_id":"i.1","args":{"quote_id":"quote.1","expected_version":4,"working_revision":1}}`
	a := call(t, h, "POST", route, "Bearer ai", `{`)
	assertAIRefused(t, a)
	if len(trd.audited) != 1 || trd.audited[0] != "quote.freeze "+kernel.CodePermissionDenied || len(ai.steps) != 0 {
		t.Fatalf("AI audit %v steps %d", trd.audited, len(ai.steps))
	}
	a = call(t, h, "POST", "/api/brant/v1/tsk/tasks/task.1/steps/trd/no.such", "Bearer ai", body)
	assertAIRefused(t, a)
	if len(trd.audited) != 2 || trd.audited[1] != "invalid "+kernel.CodePermissionDenied {
		t.Fatalf("unknown command audit %v", trd.audited)
	}
	a = call(t, h, "POST", "/api/brant/v1/tsk/tasks/task.1/steps/pur/purchase.draft", "Bearer ai", body)
	assertAIRefused(t, a)
	if len(trd.audited) != 2 {
		t.Fatalf("a missing route was audited: %v", trd.audited)
	}
	a = call(t, h, "POST", route, "Bearer system", body)
	fields, _ := a.body["field_errors"].([]any)
	if a.status != http.StatusForbidden || a.body["code"] != kernel.CodePermissionDenied || len(fields) != 1 ||
		fields[0].(map[string]any)["reason"] != taskStepReason || len(ai.steps) != 0 ||
		len(trd.audited) != 3 || trd.audited[2] != "quote.freeze "+kernel.CodePermissionDenied {
		t.Fatalf("system %d %v audit %v", a.status, a.body, trd.audited)
	}
}

type auditCount struct{ n int }

func (a *auditCount) note() error {
	a.n++
	return nil
}

type (
	quietEVD struct {
		EVD
		*auditCount
	}
	quietPTY struct {
		PTY
		*auditCount
	}
	quietCAT struct {
		CAT
		*auditCount
	}
	quietINQ struct {
		INQ
		*auditCount
	}
	quietPOL struct {
		POL
		*auditCount
	}
	quietREF struct {
		REF
		*auditCount
	}
	quietSUP struct {
		SUP
		*auditCount
	}
	quietCST struct {
		CST
		*auditCount
	}
	quietTRD struct {
		TRD
		*auditCount
	}
	quietPUR struct {
		PUR
		*auditCount
	}
	quietQLT struct {
		QLT
		*auditCount
	}
	quietSTL struct {
		STL
		*auditCount
	}
	quietCNX struct {
		CNX
		*auditCount
	}
	quietFUL struct {
		FUL
		*auditCount
	}
	quietTSK struct {
		TSK
		*auditCount
	}
	quietAI struct {
		AI
		*auditCount
	}
)

func (q *quietEVD) AuditRejected(context.Context, string, string, string) error { return q.note() }
func (q *quietPTY) AuditRejected(context.Context, string, string, string) error { return q.note() }
func (q *quietCAT) AuditRejected(context.Context, string, string, string) error { return q.note() }
func (q *quietINQ) AuditRejected(context.Context, string, string, string) error { return q.note() }
func (q *quietPOL) AuditRejected(context.Context, string, string, string) error { return q.note() }
func (q *quietREF) AuditRejected(context.Context, string, string, string) error { return q.note() }
func (q *quietSUP) AuditRejected(context.Context, string, string, string) error { return q.note() }
func (q *quietCST) AuditRejected(context.Context, string, string, string) error { return q.note() }
func (q *quietTRD) AuditRejected(context.Context, string, string, string) error { return q.note() }
func (q *quietPUR) AuditRejected(context.Context, string, string, string) error { return q.note() }
func (q *quietQLT) AuditRejected(context.Context, string, string, string) error { return q.note() }
func (q *quietSTL) AuditRejected(context.Context, string, string, string) error { return q.note() }
func (q *quietCNX) AuditRejected(context.Context, string, string, string) error { return q.note() }
func (q *quietFUL) AuditRejected(context.Context, string, string, string) error { return q.note() }
func (q *quietTSK) AuditRejected(context.Context, string, string, string) error { return q.note() }
func (q *quietAI) AuditRejected(context.Context, string, string, string) error  { return q.note() }

func fillPath(path string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(path, '{')
		if i < 0 {
			b.WriteString(path)
			return b.String()
		}
		j := strings.IndexByte(path[i:], '}')
		if j < 0 {
			b.WriteString(path)
			return b.String()
		}
		b.WriteString(path[:i])
		switch path[i+1 : i+j] {
		case "bc":
			b.WriteString("inq")
		case "command":
			b.WriteString("rfq.save_draft")
		case "source":
			b.WriteString("idp-brant.inq")
		case "event_id":
			b.WriteString("evt-1")
		case "sha256":
			b.WriteString(strings.Repeat("ab", 32))
		default:
			b.WriteString("x")
		}
		path = path[i+j+1:]
	}
}

// Every writing route the server registers, and every writing operation the OpenAPI document
// describes, refuses an AI bearer. A writing route is any method other than GET. The one pattern
// with no method is the fallback `/`, and it answers NOT_FOUND. HEAD and OPTIONS are not registered;
// seeing one fails the test so the author decides, rather than the walk skipping it. A route that
// answered anything else — including a missing route — fails the test.
func TestEveryWritingRouteRefusesAnAIActor(t *testing.T) {
	n := &auditCount{}
	replay := &countingReplay{}
	h, err := New(Config{
		EVD: &quietEVD{auditCount: n}, PTY: &quietPTY{auditCount: n}, CAT: &quietCAT{auditCount: n},
		INQ: &quietINQ{auditCount: n}, POL: &quietPOL{auditCount: n}, REF: &quietREF{auditCount: n},
		SUP: &quietSUP{auditCount: n}, CST: &quietCST{auditCount: n}, TRD: &quietTRD{auditCount: n},
		PUR: &quietPUR{auditCount: n}, QLT: &quietQLT{auditCount: n}, STL: &quietSTL{auditCount: n},
		CNX: &quietCNX{auditCount: n}, FUL: &quietFUL{auditCount: n}, TSK: &quietTSK{auditCount: n},
		AI:       &quietAI{auditCount: n},
		Verifier: tokens{"Bearer ai": agentAI}, Clock: application.Clock(fixedClock{}), Replayer: replay,
	})
	if err != nil {
		t.Fatal(err)
	}
	var fallback, writing int
	for _, pattern := range RegisteredPatterns(h) {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			if pattern != "/" {
				t.Fatalf("pattern %q has no method", pattern)
			}
			fallback++
			a := call(t, h, http.MethodGet, "/", "Bearer ai", "")
			if a.status != http.StatusNotFound || a.body["code"] != kernel.CodeNotFound {
				t.Fatalf("fallback /: %d %v", a.status, a.body)
			}
			continue
		}
		switch method {
		case http.MethodGet:
			continue
		case http.MethodHead, http.MethodOptions:
			t.Fatalf("pattern %q uses %s; decide whether it is a writing route (the walk does not skip it)", pattern, method)
		}
		writing++
		a := call(t, h, method, fillPath(path), "Bearer ai", `{"not":"read"}`)
		assertAIRefusedOn(t, method, path, a)
	}
	if fallback != 1 {
		t.Fatalf("%d method-less patterns, want the one fallback /", fallback)
	}
	if writing != 4 {
		t.Fatalf("%d writing patterns, want the command, reconcile, replay and task-step routes", writing)
	}
	doc, err := OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err = json.Unmarshal(doc, &spec); err != nil {
		t.Fatal(err)
	}
	var documented int
	for path, item := range spec.Paths {
		for key := range item {
			switch key {
			case "get", "parameters", "summary", "description", "servers", "$ref":
				continue
			case "head", "options":
				t.Fatalf("OpenAPI %s %s: decide whether it is a writing route (the walk does not skip it)", strings.ToUpper(key), path)
			case "post", "put", "patch", "delete", "trace":
			default:
				t.Fatalf("OpenAPI path %s has key %q; decide whether it is an operation", path, key)
			}
			documented++
			method := strings.ToUpper(key)
			a := call(t, h, method, fillPath(path), "Bearer ai", `{"not":"read"}`)
			assertAIRefusedOn(t, method, path, a)
		}
	}
	if documented < writing {
		t.Fatalf("OpenAPI describes %d writing operations, the server registers %d writing patterns", documented, writing)
	}
	if replay.n != 0 {
		t.Fatalf("the replayer ran %d times", replay.n)
	}
	if n.n == 0 {
		t.Fatal("no refusal was audited")
	}
}
