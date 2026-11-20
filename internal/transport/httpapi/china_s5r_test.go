package httpapi

import (
	"context"
	"net/http"
	"testing"

	"go.idp.xyz/idp-bento-go/application"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
)

// fakeS5RCNX records the S5R members R10 adds to CNX's commands on top of fakeCNX.
type fakeS5RCNX struct {
	fakeCNX
	rebound     []cnxapi.RebindPolicy
	recorded    []cnxapi.RecordTaxResult
	submissions []cnxapi.RecordSubmission
	receipts    []cnxapi.RecordReceipt
	registered  []cnxapi.RegisterCertificate
}

func (f *fakeS5RCNX) RebindPolicy(_ context.Context, cmd cnxapi.RebindPolicy) (cnxapi.TaxCaseReceipt, error) {
	f.rebound = append(f.rebound, cmd)
	return cnxapi.TaxCaseReceipt{Receipt: cnxapi.Receipt{ActionID: "act-1", ObjectID: "trc-1", RowVersion: 4, Status: "expected"}}, nil
}

func (f *fakeS5RCNX) RecordTaxResult(_ context.Context, cmd cnxapi.RecordTaxResult) (cnxapi.TaxCaseReceipt, error) {
	f.recorded = append(f.recorded, cmd)
	return cnxapi.TaxCaseReceipt{Receipt: cnxapi.Receipt{ActionID: "act-2", ObjectID: "trc-1", RowVersion: 5, Status: "submitted"}}, nil
}

func (f *fakeS5RCNX) RecordSubmission(_ context.Context, cmd cnxapi.RecordSubmission) (cnxapi.DeclarationReceipt, error) {
	f.submissions = append(f.submissions, cmd)
	return cnxapi.DeclarationReceipt{Receipt: cnxapi.Receipt{ActionID: "act-3", ObjectID: "dcl-1", RowVersion: 3, Status: "submitted"}}, nil
}

func (f *fakeS5RCNX) RecordReceipt(_ context.Context, cmd cnxapi.RecordReceipt) (cnxapi.DeclarationReceipt, error) {
	f.receipts = append(f.receipts, cmd)
	return cnxapi.DeclarationReceipt{Receipt: cnxapi.Receipt{ActionID: "act-4", ObjectID: "dcl-1", RowVersion: 5, Status: "not_received"}}, nil
}

func (f *fakeS5RCNX) RegisterCertificate(_ context.Context, cmd cnxapi.RegisterCertificate) (cnxapi.CertificateReceipt, error) {
	f.registered = append(f.registered, cmd)
	return cnxapi.CertificateReceipt{Receipt: cnxapi.Receipt{ActionID: "act-5", ObjectID: "crt-1", RowVersion: 1, Status: "registered"}}, nil
}

// S5R R10 over HTTP: tax_refund_case.rebind_policy (S5R-D23), record_result's purchase_invoice_id (evidence_rolled_back,
// S5R-D20), a broker submission's service_role (S5R-D36), a not_received receipt (S5R-D13) and a preferential
// certificate's agreement_code, origin_form and issuing_body (S5R-D12) decode by the reflective mapping.
func TestS5RCNXCommandsDecodeTheirMembers(t *testing.T) {
	cnx := &fakeS5RCNX{}
	h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: &fakeINQ{}, POL: struct{ POL }{}, REF: struct{ REF }{},
		SUP: struct{ SUP }{}, CST: struct{ CST }{}, TRD: struct{ TRD }{}, PUR: struct{ PUR }{}, CNX: cnx, Verifier: tokens{"Bearer good": alice},
		Clock: application.Clock(fixedClock{})})
	if err != nil {
		t.Fatal(err)
	}
	envelope := func(command, args string) string {
		return `{"contract":"` + cnxapi.Contract + `","command":"` + command + `","idempotency_key":"k1","args":` + args + `}`
	}
	post := func(command, args string) answer {
		return call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/"+command, "Bearer good", envelope(command, args))
	}
	if a := post("tax_refund_case.rebind_policy", `{"case_id":"trc-1","expected_version":3,"revision":2,"reason":"出口后发布的修订覆盖出口日期"}`); a.status != http.StatusOK ||
		len(cnx.rebound) != 1 || cnx.rebound[0].Revision != 2 || cnx.rebound[0].ExpectedVersion != 3 || cnx.rebound[0].Reason == "" {
		t.Fatalf("%d %v %+v", a.status, a.body, cnx.rebound)
	}
	if a := post("tax_refund_case.record_result", `{"case_id":"trc-1","expected_version":4,"stage":"evidence_rolled_back","purchase_invoice_id":"pin-1",`+
		`"result_text":"回退已确认","evidence_ids":["e1"]}`); a.status != http.StatusOK || cnx.recorded[0].PurchaseInvoiceID != "pin-1" {
		t.Fatalf("%d %v %+v", a.status, a.body, cnx.recorded)
	}
	if a := post("declaration_case.record_submission", `{"declaration_id":"dcl-1","expected_version":2,"operation_reference":"op.1",`+
		`"submitter":{"kind":"broker","party_id":"broker.1","service_role":{"id":"customs","revision":1},"name":"合成报关行"},`+
		`"submitted_at":"2026-11-12T08:00:00Z"}`); a.status != http.StatusOK || cnx.submissions[0].Submitter.ServiceRole == nil ||
		cnx.submissions[0].Submitter.ServiceRole.ID != "customs" || cnx.submissions[0].Submitter.ServiceRole.Revision != 1 {
		t.Fatalf("%d %v %+v", a.status, a.body, cnx.submissions)
	}
	if a := post("declaration_case.record_receipt", `{"declaration_id":"dcl-1","expected_version":4,"receipt_id":"r9","outcome":"not_received",`+
		`"evidence_ids":["e1"],"reason":"查询无入库记录"}`); a.status != http.StatusOK || cnx.receipts[0].Outcome != cnxapi.DeclarationNotReceived {
		t.Fatalf("%d %v %+v", a.status, a.body, cnx.receipts)
	}
	if a := post("certificate.register", `{"expected_version":0,"certificate_kind":"preferential_certificate_of_origin","agreement_code":"RCEP",`+
		`"origin_form":"certificate","issuing_body":"customs","number":"R-1","issuer":"合成海关","issued_on":"2026-11-10",`+
		`"object":{"kind":"shipment","id":"ship.1"},"market":"JP","valid_from":"2026-11-10T00:00:00Z","valid_until":"2027-11-10T00:00:00Z",`+
		`"file_id":"f1","basis":"核验"}`); a.status != http.StatusOK || cnx.registered[0].AgreementCode != "RCEP" ||
		cnx.registered[0].OriginForm != cnxapi.OriginFormCertificate || cnx.registered[0].IssuingBody != cnxapi.IssuingBodyCustoms {
		t.Fatalf("%d %v %+v", a.status, a.body, cnx.registered)
	}
}
