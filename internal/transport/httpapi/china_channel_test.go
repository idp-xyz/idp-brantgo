package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.idp.xyz/idp-bento-go/application"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// fakeChannel records the five channel commands. Settle is not a method it expects to be called.
type fakeChannel struct {
	fakeCNX
	submitted     []cnxapi.SubmitDeclarationChannel
	reconciled    []cnxapi.ReconcileDeclarationChannel
	ingested      []cnxapi.IngestDeclarationReceipts
	taxSubmitted  []cnxapi.SubmitTaxChannel
	taxReconciled []cnxapi.ReconcileTaxChannel
	audited       []string
}

func (f *fakeChannel) AuditRejected(_ context.Context, command, objectID, code string) error {
	f.audited = append(f.audited, command+"|"+objectID+"|"+code)
	return nil
}

func (f *fakeChannel) SubmitDeclarationChannel(_ context.Context, cmd cnxapi.SubmitDeclarationChannel) (cnxapi.ChannelAnswer, error) {
	f.submitted = append(f.submitted, cmd)
	return cnxapi.ChannelAnswer{Receipt: cnxapi.Receipt{ActionID: "act-1", ObjectID: cmd.DeclarationID, RowVersion: cmd.ExpectedVersion, Status: cnxapi.DeclarationUnknown},
		OperationID: "op-secret", ConnectorState: cnxapi.ChannelUnknown, AttemptNo: 1}, nil
}

func (f *fakeChannel) ReconcileDeclarationChannel(_ context.Context, cmd cnxapi.ReconcileDeclarationChannel) (cnxapi.ChannelAnswer, error) {
	f.reconciled = append(f.reconciled, cmd)
	return cnxapi.ChannelAnswer{Receipt: cnxapi.Receipt{ActionID: "act-2", ObjectID: cmd.DeclarationID, RowVersion: cmd.ExpectedVersion, Status: cnxapi.DeclarationAccepted},
		OperationID: "op-secret", ConnectorState: cnxapi.ChannelSucceeded, AttemptNo: 1, Outcome: cnxapi.DeclarationAccepted}, nil
}

func (f *fakeChannel) IngestDeclarationReceipts(_ context.Context, cmd cnxapi.IngestDeclarationReceipts) (cnxapi.ChannelIngest, error) {
	f.ingested = append(f.ingested, cmd)
	return cnxapi.ChannelIngest{Receipt: cnxapi.Receipt{ActionID: "act-3", ObjectID: cmd.DeclarationID, RowVersion: 4, Status: cnxapi.DeclarationReleased},
		DeclarationID: cmd.DeclarationID, LastSequence: 2,
		Chain: []cnxapi.ChannelChainEntry{{ReceiptID: "cr-1", Authoritative: false, RefusalCode: cnxapi.CodeDeclarationReceiptStale}}}, nil
}

func (f *fakeChannel) SubmitTaxChannel(_ context.Context, cmd cnxapi.SubmitTaxChannel) (cnxapi.ChannelAnswer, error) {
	f.taxSubmitted = append(f.taxSubmitted, cmd)
	return cnxapi.ChannelAnswer{Receipt: cnxapi.Receipt{ActionID: "act-4", ObjectID: cmd.CaseID, RowVersion: cmd.ExpectedVersion, Status: cnxapi.TaxUnknown},
		OperationID: "op-tax", ConnectorState: cnxapi.ChannelUnknown, AttemptNo: 1}, nil
}

func (f *fakeChannel) ReconcileTaxChannel(_ context.Context, cmd cnxapi.ReconcileTaxChannel) (cnxapi.ChannelAnswer, error) {
	f.taxReconciled = append(f.taxReconciled, cmd)
	return cnxapi.ChannelAnswer{Receipt: cnxapi.Receipt{ActionID: "act-5", ObjectID: cmd.CaseID, RowVersion: cmd.ExpectedVersion, Status: cnxapi.TaxRejected},
		OperationID: "op-tax", ConnectorState: cnxapi.ChannelFailed, AttemptNo: 1, Outcome: cnxapi.TaxRejected}, nil
}

// The five channel commands decode on the CNX command route by the reflective mapping (S6-05).
// The HTTP answer is the closed receipt: connector state, operation id and the ingest chain stay
// off it. Settle is not a command of the route. An AI actor is refused before the body is read.
func TestCNXChannelCommandsDecodeOnTheCommandRoute(t *testing.T) {
	cnx := &fakeChannel{}
	h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: &fakeINQ{}, POL: struct{ POL }{}, REF: struct{ REF }{},
		SUP: struct{ SUP }{}, CST: struct{ CST }{}, TRD: struct{ TRD }{}, PUR: struct{ PUR }{}, CNX: cnx,
		Verifier: tokens{"Bearer good": alice, "Bearer ai": agentAI}, Clock: application.Clock(fixedClock{})})
	if err != nil {
		t.Fatal(err)
	}
	envelope := func(command, args string) string {
		return `{"contract":"` + cnxapi.Contract + `","command":"` + command + `","idempotency_key":"k1","args":` + args + `}`
	}
	post := func(auth, command, args string) answer {
		t.Helper()
		return call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/"+command, auth, envelope(command, args))
	}
	closed := func(a answer, object, status string, version float64) {
		t.Helper()
		if a.status != http.StatusOK || a.body["ok"] != true || a.body["action_id"] == "" || a.body["object_id"] != object ||
			a.body["row_version"] != version || a.body["status"] != status || a.body["replayed"] != false ||
			a.body["operation_id"] != nil || a.body["connector_state"] != nil || a.body["chain"] != nil || a.body["last_sequence"] != nil {
			t.Fatalf("closed receipt: %d %v", a.status, a.body)
		}
	}

	a := post("Bearer good", cnxapi.CommandSubmitDeclarationChannel, `{"declaration_id":"dcl-1","expected_version":2,"submitter_name":"合成报关员（测试）","evidence_ids":["e1"]}`)
	closed(a, "dcl-1", cnxapi.DeclarationUnknown, 2)
	if len(cnx.submitted) != 1 || cnx.submitted[0].IdempotencyKey != "k1" || cnx.submitted[0].DeclarationID != "dcl-1" ||
		cnx.submitted[0].ExpectedVersion != 2 || cnx.submitted[0].SubmitterName != "合成报关员（测试）" || len(cnx.submitted[0].EvidenceIDs) != 1 ||
		cnx.submitted[0].EvidenceIDs[0] != "e1" {
		t.Fatalf("submit %+v", cnx.submitted)
	}

	a = post("Bearer good", cnxapi.CommandReconcileDeclarationChannel, `{"declaration_id":"dcl-1","expected_version":3,"evidence_ids":["e1","e2"],"reason":"海关查询无此申报（测试）"}`)
	closed(a, "dcl-1", cnxapi.DeclarationAccepted, 3)
	if r := cnx.reconciled; len(r) != 1 || r[0].ExpectedVersion != 3 || r[0].Reason != "海关查询无此申报（测试）" || len(r[0].EvidenceIDs) != 2 {
		t.Fatalf("reconcile %+v", r)
	}

	a = post("Bearer good", cnxapi.CommandIngestDeclarationReceipts, `{"declaration_id":"dcl-1","expected_version":4,"receipts":[`+
		`{"outcome":"released","sequence":2,"status_code":"SYN-released","original_text":"合成海关回执：released（测试数据）","mapping_version":"syn-customs-status.v1","declaration_number":"SYN-DECL-1","received_at":"2026-11-12T08:00:00Z","exported_at":"2026-11-11T16:00:00Z"},`+
		`{"outcome":"accepted","sequence":1,"status_code":"SYN-accepted","original_text":"合成海关回执：accepted（测试数据）","mapping_version":"syn-customs-status.v1","declaration_number":"SYN-DECL-1","received_at":"2026-11-12T07:00:00Z"}]}`)
	closed(a, "dcl-1", cnxapi.DeclarationReleased, 4)
	if g := cnx.ingested; len(g) != 1 || len(g[0].Receipts) != 2 || g[0].Receipts[0].Outcome != cnxapi.DeclarationReleased ||
		g[0].Receipts[0].Sequence != 2 || g[0].Receipts[0].ExportedAt != "2026-11-11T16:00:00Z" || g[0].Receipts[1].Outcome != cnxapi.DeclarationAccepted ||
		g[0].Receipts[1].ExportedAt != "" || g[0].Receipts[1].Sequence != 1 {
		t.Fatalf("ingest %+v", g)
	}

	a = post("Bearer good", cnxapi.CommandSubmitTaxChannel, `{"case_id":"trc-1","expected_version":5,"evidence_ids":["e1"],"currency":{"id":"CNY","revision":1}}`)
	closed(a, "trc-1", cnxapi.TaxUnknown, 5)
	if g := cnx.taxSubmitted; len(g) != 1 || g[0].CaseID != "trc-1" || g[0].Currency == nil || g[0].Currency.ID != "CNY" || g[0].Currency.Revision != 1 ||
		len(g[0].EvidenceIDs) != 1 {
		t.Fatalf("tax submit %+v", g)
	}

	a = post("Bearer good", cnxapi.CommandReconcileTaxChannel, `{"case_id":"trc-1","expected_version":6,"evidence_ids":["e1"],"reason":"核对退回（测试）"}`)
	closed(a, "trc-1", cnxapi.TaxRejected, 6)
	if g := cnx.taxReconciled; len(g) != 1 || g[0].Reason != "核对退回（测试）" || g[0].Currency != nil || g[0].ExpectedVersion != 6 {
		t.Fatalf("tax reconcile %+v", g)
	}

	a = post("Bearer good", cnxapi.CommandSubmitDeclarationChannel, `{"declaration_id":"dcl-1","submitter_name":"合成报关员（测试）"}`)
	if a.status != http.StatusUnprocessableEntity || a.body["code"] != kernel.CodeSchemaInvalid || errField(a, "/args/expected_version") == "" {
		t.Fatalf("expected_version: %d %v", a.status, a.body)
	}

	for _, command := range []string{cnxapi.CommandSettleDeclarationChannel, cnxapi.CommandSettleTaxChannel} {
		a = post("Bearer good", command, `{"channel":"customs_declaration","subject_id":"dcl-1","state":"unknown"}`)
		if a.status != http.StatusBadRequest || a.body["code"] != kernel.CodeUnsupportedContract {
			t.Fatalf("settle %s: %d %v", command, a.status, a.body)
		}
	}

	before := len(cnx.submitted) + len(cnx.reconciled) + len(cnx.ingested) + len(cnx.taxSubmitted) + len(cnx.taxReconciled)
	for _, command := range []string{cnxapi.CommandSubmitDeclarationChannel, cnxapi.CommandReconcileDeclarationChannel,
		cnxapi.CommandIngestDeclarationReceipts, cnxapi.CommandSubmitTaxChannel, cnxapi.CommandReconcileTaxChannel} {
		body := `{`
		if command == cnxapi.CommandSubmitDeclarationChannel {
			body = strings.Repeat("x", MaxBody+1)
		}
		a = call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/"+command, "Bearer ai", body)
		assertAIRefused(t, a)
		want := command + "||" + kernel.CodePermissionDenied
		if len(cnx.audited) == 0 || cnx.audited[len(cnx.audited)-1] != want {
			t.Fatalf("audit %v, want %s", cnx.audited, want)
		}
	}
	if got := len(cnx.submitted) + len(cnx.reconciled) + len(cnx.ingested) + len(cnx.taxSubmitted) + len(cnx.taxReconciled); got != before {
		t.Fatalf("an AI actor reached a channel command: %d calls, before %d", got, before)
	}
}

func errField(a answer, path string) string {
	fields, _ := a.body["field_errors"].([]any)
	for _, f := range fields {
		if got, _ := f.(map[string]any)["path"].(string); got == path {
			return got
		}
	}
	return ""
}
