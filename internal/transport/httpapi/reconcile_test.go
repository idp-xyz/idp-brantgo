package httpapi

import (
	"context"
	"strings"
	"testing"

	"go.idp.xyz/idp-bento-go/application"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// hashINQ answers reconciliations: by command, and by request hash (ActionReconciliation v1).
type hashINQ struct {
	fakeINQ
	byCommand []inqapi.Command
	byHash    []kernel.ActionRequest
}

func (f *hashINQ) Reconcile(_ context.Context, cmd inqapi.Command) (inqapi.Reconciliation, error) {
	f.byCommand = append(f.byCommand, cmd)
	return inqapi.Reconciliation{State: kernel.ReconcileNotCommitted}, nil
}

func (f *hashINQ) ReconcileHash(_ context.Context, command, key, hash string) (inqapi.Reconciliation, error) {
	f.byHash = append(f.byHash, kernel.ActionRequest{Command: command, IdempotencyKey: key, RequestHash: hash})
	return inqapi.Reconciliation{State: kernel.ReconcileCommitted, ActionID: "act-1", ObjectID: "rfq.1",
		Receipt: &inqapi.Receipt{ActionID: "act-1", ObjectID: "rfq.1", RowVersion: 1, Status: "draft"}}, nil
}

const reconcileRoute = saveRoute + "/reconcile"

func hashBody(members string) string {
	return `{"contract":"brant.inq/0.1.0","command":"rfq.save_draft","idempotency_key":"k1"` + members + `}`
}

// The reconcile route takes the envelope with the request hash in place of the arguments
// (ActionReconciliation v1, Context Map R67) and hands the context the command, key and hash
// unchanged; the full form still reaches Reconcile. The hash form is closed and checked: another
// contract or command, a key that is not an identifier, a hash that is not 64 lower-case hex digits,
// a missing member, an unknown one (args beside request_hash, a case variant) are refused before the
// context sees them.
func TestReconcileByRequestHash(t *testing.T) {
	inq := &hashINQ{}
	h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: inq, POL: struct{ POL }{}, REF: struct{ REF }{},
		SUP: struct{ SUP }{}, CST: struct{ CST }{}, TRD: struct{ TRD }{}, PUR: struct{ PUR }{}, QLT: struct{ QLT }{}, CNX: struct{ CNX }{},
		Verifier: tokens{"Bearer good": alice}, Clock: application.Clock(fixedClock{})})
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("ab", 32)
	a := call(t, h, "POST", reconcileRoute, "Bearer good", hashBody(`,"request_hash":"`+hash+`"`))
	receipt, _ := a.body["receipt"].(map[string]any)
	if a.status != 200 || a.body["state"] != "committed" || a.body["action_id"] != "act-1" || receipt["row_version"] != float64(1) ||
		len(inq.byHash) != 1 || inq.byHash[0] != (kernel.ActionRequest{Command: "rfq.save_draft", IdempotencyKey: "k1", RequestHash: hash}) {
		t.Fatalf("by hash: %d %v %+v", a.status, a.body, inq.byHash)
	}
	a = call(t, h, "POST", reconcileRoute, "Bearer good", request(`{"id":"rfq.1","expected_version":0}`))
	if a.status != 200 || a.body["state"] != "not_committed" || len(inq.byCommand) != 1 || len(inq.byHash) != 1 {
		t.Fatalf("the full form: %d %v", a.status, a.body)
	}
	for _, c := range []struct {
		name, body string
		status     int
		code, path string
	}{
		{"another contract", `{"contract":"brant.trd/0.1.0","command":"rfq.save_draft","idempotency_key":"k1","request_hash":"` + hash + `"}`,
			400, kernel.CodeUnsupportedContract, "/contract"},
		{"another command", `{"contract":"brant.inq/0.1.0","command":"rfq.cancel","idempotency_key":"k1","request_hash":"` + hash + `"}`,
			400, kernel.CodeUnsupportedContract, "/command"},
		{"upper-case hash", hashBody(`,"request_hash":"` + strings.ToUpper(hash) + `"`), 422, kernel.CodeSchemaInvalid, "/request_hash"},
		{"short hash", hashBody(`,"request_hash":"abc"`), 422, kernel.CodeSchemaInvalid, "/request_hash"},
		{"a key that is not an identifier", `{"contract":"brant.inq/0.1.0","command":"rfq.save_draft","idempotency_key":"-k","request_hash":"` +
			hash + `"}`, 422, kernel.CodeSchemaInvalid, "/idempotency_key"},
		{"no key", `{"contract":"brant.inq/0.1.0","command":"rfq.save_draft","request_hash":"` + hash + `"}`, 422, kernel.CodeSchemaInvalid,
			"/idempotency_key"},
		{"args beside the hash", hashBody(`,"request_hash":"` + hash + `","args":{}`), 422, kernel.CodeSchemaInvalid, ""},
		{"a case variant", hashBody(`,"Request_Hash":"` + hash + `"`), 422, kernel.CodeSchemaInvalid, ""},
		{"a hash that is not a string", hashBody(`,"request_hash":1`), 422, kernel.CodeSchemaInvalid, ""},
	} {
		a := call(t, h, "POST", reconcileRoute, "Bearer good", c.body)
		if a.status != c.status || a.body["code"] != c.code {
			t.Errorf("%s: %d %v", c.name, a.status, a.body)
			continue
		}
		if fields, _ := a.body["field_errors"].([]any); c.path != "" && (len(fields) == 0 || fields[0].(map[string]any)["path"] != c.path) {
			t.Errorf("%s: path %v", c.name, a.body)
		}
	}
	if len(inq.byHash) != 1 || len(inq.byCommand) != 1 {
		t.Fatalf("a refused request reached the context: %+v %+v", inq.byHash, inq.byCommand)
	}
}

// goalTRD answers GoalReadiness with the query it was given.
type goalTRD struct {
	TRD
	got []trdapi.GoalQuery
}

func (f *goalTRD) GoalReadiness(_ context.Context, q trdapi.GoalQuery) (trdapi.GoalAssessment, error) {
	f.got = append(f.got, q)
	if err := q.Validate(); err != nil {
		return trdapi.GoalAssessment{}, err
	}
	return trdapi.GoalAssessment{Goal: q.Goal, GoalVersion: q.GoalVersion, QuoteID: q.QuoteID, Outcome: trdapi.AnswerUnsatisfied,
		Code: trdapi.CodeRequiredCheckUnsatisfied, Assertions: []trdapi.Assertion{{Assertion: trdapi.AssertionOrder, Answer: trdapi.AnswerUnsatisfied,
			Code: trdapi.CodeOrderMissing, Checks: []trdapi.GoalCheck{{Check: trdapi.CheckConversion, Answer: trdapi.AnswerUnsatisfied,
				Code: trdapi.CodeOrderMissing, Path: "/quote_id"}}, Evidence: []trdapi.EvidenceRef{}}}}, nil
}

// GET /api/brant/v1/trd/quotes/<id>/goal_readiness passes the goal, its version (1 by default), the
// revision and the target to GoalReadiness v1 and answers its assessment in snake case; unknown or
// repeated parameters and malformed numbers are refused before TRD sees them.
func TestGoalReadinessRoute(t *testing.T) {
	trd := &goalTRD{}
	h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: &fakeINQ{}, POL: struct{ POL }{}, REF: struct{ REF }{},
		SUP: struct{ SUP }{}, CST: struct{ CST }{}, TRD: trd, PUR: struct{ PUR }{}, QLT: struct{ QLT }{}, CNX: struct{ CNX }{},
		Verifier: tokens{"Bearer good": alice}, Clock: application.Clock(fixedClock{})})
	if err != nil {
		t.Fatal(err)
	}
	const route = "/api/brant/v1/trd/quotes/quote.1/goal_readiness"
	a := call(t, h, "GET", route+"?goal=order_active&business_revision=2&customer_id=customer.1&rfq_id=rfq.1", "Bearer good", "")
	assertions, _ := a.body["assertions"].([]any)
	if a.status != 200 || a.body["outcome"] != "unsatisfied" || a.body["code"] != trdapi.CodeRequiredCheckUnsatisfied || len(assertions) != 1 ||
		assertions[0].(map[string]any)["assertion"] != "Q01.order" || assertions[0].(map[string]any)["code"] != "ORDER_MISSING" ||
		len(trd.got) != 1 || trd.got[0] != (trdapi.GoalQuery{Goal: trdapi.GoalOrderActive, GoalVersion: 1, QuoteID: "quote.1", BusinessRevision: 2,
		CustomerID: "customer.1", RfqID: "rfq.1"}) {
		t.Fatalf("%d %v %+v", a.status, a.body, trd.got)
	}
	for _, c := range []struct{ query, path string }{
		{"?goal=order_active&colour=red", "/colour"},
		{"?goal=order_active&goal=quote_ready", "/goal"},
		{"?goal=order_active&business_revision=-1", "/business_revision"},
		{"?goal=order_active&goal_version=2", "/goal_version"},
		{"?goal=quote_sent", "/goal"},
		{"", "/goal"},
	} {
		a := call(t, h, "GET", route+c.query, "Bearer good", "")
		fields, _ := a.body["field_errors"].([]any)
		if a.status != 422 || a.body["code"] != kernel.CodeSchemaInvalid || len(fields) == 0 || fields[0].(map[string]any)["path"] != c.path {
			t.Errorf("%s: %d %v", c.query, a.status, a.body)
		}
	}
}
