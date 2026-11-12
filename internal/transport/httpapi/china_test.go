package httpapi

import (
	"context"
	"net/http"
	"testing"

	"go.idp.xyz/idp-bento-go/application"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// fakeCNX records the commands it receives; methods the tests do not use are nil and would panic.
type fakeCNX struct {
	CNX
	evaluated []cnxapi.Evaluate
	decided   []cnxapi.Decide
	reopened  []cnxapi.Reopen
}

func (f *fakeCNX) Evaluate(_ context.Context, cmd cnxapi.Evaluate) (cnxapi.CheckReceipt, error) {
	f.evaluated = append(f.evaluated, cmd)
	return cnxapi.CheckReceipt{Receipt: cnxapi.Receipt{ActionID: "act-1", ObjectID: "tcc-1", RowVersion: 1, Status: "pending"}}, nil
}

func (f *fakeCNX) Decide(_ context.Context, cmd cnxapi.Decide) (cnxapi.CheckReceipt, error) {
	f.decided = append(f.decided, cmd)
	return cnxapi.CheckReceipt{}, cnxapi.Reject(cnxapi.CodeTradeComplianceNotClear, "/args/check_id", "")
}

func (f *fakeCNX) Reopen(_ context.Context, cmd cnxapi.Reopen) (cnxapi.CheckReceipt, error) {
	f.reopened = append(f.reopened, cmd)
	return cnxapi.CheckReceipt{Receipt: cnxapi.Receipt{ActionID: "act-2", ObjectID: "tcc-2", RowVersion: 1, Status: "pending"}}, nil
}

func (f *fakeCNX) AuditRejected(context.Context, string, string, string) error { return nil }

func (f *fakeCNX) OrderChecks(_ context.Context, id string) ([]cnxapi.CheckSummary, error) {
	if id != "o1" {
		return nil, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	return nil, nil
}

// CNX's commands decode their members by the reflective mapping (S3-D149): nested destination,
// address countries with evidence_ids, end use, a license_required present only when sent.
func TestCNXCommandsDecodeTheirMembers(t *testing.T) {
	cnx := &fakeCNX{}
	h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: &fakeINQ{}, POL: struct{ POL }{}, REF: struct{ REF }{},
		SUP: struct{ SUP }{}, CST: struct{ CST }{}, TRD: struct{ TRD }{}, PUR: struct{ PUR }{}, CNX: cnx, Verifier: tokens{"Bearer good": alice},
		Clock: application.Clock(fixedClock{})})
	if err != nil {
		t.Fatal(err)
	}
	envelope := func(command, args string) string {
		return `{"contract":"` + cnxapi.Contract + `","command":"` + command + `","idempotency_key":"k1","args":` + args + `}`
	}
	a := call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/trade_compliance.evaluate", "Bearer good", envelope("trade_compliance.evaluate",
		`{"order_id":"o1","order_revision":2,"expected_version":0,"regulation_id":"screening","regulation_revision":1,"programme_lists":["cn.embargo"],"code_table_version":"CN-2026",`+
			`"destination":{"country":"DE","reason":"B/L","evidence_ids":["s1"]},`+
			`"address_countries":[{"party_id":"p1","address_id":"hq","country":"NL","evidence_ids":["s2"]}],`+
			`"end_use":{"statement":"零售","evidence_ids":["s3"]}}`))
	if a.status != http.StatusOK || a.body["object_id"] != "tcc-1" || len(cnx.evaluated) != 1 {
		t.Fatalf("%d %v", a.status, a.body)
	}
	e := cnx.evaluated[0]
	if e.OrderRevision != 2 || e.Destination == nil || e.Destination.EvidenceIDs[0] != "s1" || e.AddressCountries[0].Country != "NL" ||
		e.EndUse.Statement != "零售" || e.IdempotencyKey != "k1" || e.ProgrammeLists[0] != "cn.embargo" || e.CodeTableVersion != "CN-2026" {
		t.Fatalf("%+v", e)
	}

	a = call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/trade_compliance.decide", "Bearer good", envelope("trade_compliance.decide",
		`{"check_id":"tcc-1","expected_version":1,"evaluation_id":"tce-1","conclusion":"clear","reason":"ok","license_required":false}`))
	if a.status != http.StatusConflict || a.body["code"] != cnxapi.CodeTradeComplianceNotClear {
		t.Fatalf("%d %v", a.status, a.body)
	}
	if d := cnx.decided[0]; d.LicenseRequired == nil || *d.LicenseRequired || d.Conclusion != "clear" {
		t.Fatalf("%+v", d)
	}

	a = call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/trade_compliance.reopen", "Bearer good", envelope("trade_compliance.reopen",
		`{"cause":"order_amended","order_id":"o1","order_revision":2,"expected_version":0,"reason":"OrderAmended"}`))
	if a.status != http.StatusOK || cnx.reopened[0].Cause != cnxapi.CauseOrderAmended || cnx.reopened[0].CheckID != "" {
		t.Fatalf("%d %v %+v", a.status, a.body, cnx.reopened)
	}

	a = call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/trade_compliance.decide", "Bearer good", envelope("trade_compliance.decide",
		`{"check_id":"tcc-1","evaluation_id":"tce-1","conclusion":"blocked","reason":"x"}`))
	if a.status != http.StatusUnprocessableEntity || a.body["code"] != kernel.CodeSchemaInvalid {
		t.Fatalf("expected_version is required: %d %v", a.status, a.body)
	}

	a = call(t, h, http.MethodGet, "/api/brant/v1/cnx/orders/o1/trade_compliance", "Bearer good", "")
	if checks, ok := a.body["checks"].([]any); a.status != http.StatusOK || !ok || len(checks) != 0 || a.body["order_id"] != "o1" {
		t.Fatalf("%d %v", a.status, a.body)
	}
}
