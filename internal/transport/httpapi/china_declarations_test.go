package httpapi

import (
	"context"
	"net/http"
	"testing"

	"go.idp.xyz/idp-bento-go/application"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
)

// fakeDeclarations records the declaration and bundle commands it receives on top of fakeCNX.
type fakeDeclarations struct {
	fakeCNX
	prepared   []cnxapi.PrepareDeclaration
	confirmed  []cnxapi.ConfirmDeclaration
	submitted  []cnxapi.RecordSubmission
	receipts   []cnxapi.RecordReceipt
	checked    []cnxapi.CheckBundle
	reconciled []cnxapi.ResolveReconciliation
}

func (f *fakeDeclarations) PrepareDeclaration(_ context.Context, cmd cnxapi.PrepareDeclaration) (cnxapi.DeclarationReceipt, error) {
	f.prepared = append(f.prepared, cmd)
	return cnxapi.DeclarationReceipt{Receipt: cnxapi.Receipt{ActionID: "act-1", ObjectID: "dcl-1", RowVersion: 1, Status: "draft"}}, nil
}

func (f *fakeDeclarations) ConfirmDeclaration(_ context.Context, cmd cnxapi.ConfirmDeclaration) (cnxapi.DeclarationReceipt, error) {
	f.confirmed = append(f.confirmed, cmd)
	return cnxapi.DeclarationReceipt{}, cnxapi.Reject(cnxapi.CodeDeclarationMappingUnverified, "/args/version", "")
}

func (f *fakeDeclarations) RecordSubmission(_ context.Context, cmd cnxapi.RecordSubmission) (cnxapi.DeclarationReceipt, error) {
	f.submitted = append(f.submitted, cmd)
	return cnxapi.DeclarationReceipt{}, cnxapi.Reject(cnxapi.CodeExternalResultUnknown, "/args/declaration_id", "")
}

func (f *fakeDeclarations) RecordReceipt(_ context.Context, cmd cnxapi.RecordReceipt) (cnxapi.DeclarationReceipt, error) {
	f.receipts = append(f.receipts, cmd)
	return cnxapi.DeclarationReceipt{}, cnxapi.Reject(cnxapi.CodeDeclarationReceiptStale, "/args/external/sequence", "")
}

func (f *fakeDeclarations) CheckBundle(_ context.Context, cmd cnxapi.CheckBundle) (cnxapi.BundleReceipt, error) {
	f.checked = append(f.checked, cmd)
	return cnxapi.BundleReceipt{}, cnxapi.Reject(cnxapi.CodeDocumentRequirementUnverified, "/args/requirements/0/status", "")
}

func (f *fakeDeclarations) ResolveReconciliation(_ context.Context, cmd cnxapi.ResolveReconciliation) (cnxapi.BundleReceipt, error) {
	f.reconciled = append(f.reconciled, cmd)
	return cnxapi.BundleReceipt{Receipt: cnxapi.Receipt{ActionID: "act-2", ObjectID: "dbn-1", RowVersion: 3, Status: "complete"}}, nil
}

func (f *fakeDeclarations) Declarations(context.Context) ([]cnxapi.DeclarationSummary, error) {
	return nil, nil
}
func (f *fakeDeclarations) Bundles(context.Context) ([]cnxapi.BundleSummary, error) { return nil, nil }

// The declaration and bundle commands decode by the reflective mapping — items with lines and unit
// conversions, the submitter, the external receipt with amended items, the requirements with owner,
// declaration and fund records, the reconciliation's subject, values, responsibility and adjustment —
// and the refusals keep the catalog's categories (422 DECLARATION_MAPPING_UNVERIFIED, 409
// EXTERNAL_RESULT_UNKNOWN, DECLARATION_RECEIPT_STALE and DOCUMENT_REQUIREMENT_UNVERIFIED); both lists
// answer empty.
func TestCNXDeclarationAndBundleCommandsDecodeTheirMembers(t *testing.T) {
	cnx := &fakeDeclarations{}
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
	a := post("declaration_case.prepare", `{"export_lot_id":"lot.1","expected_version":0,"code_table_version":"CN-2026","items":[{"item_no":1,"lines":[{"line_id":"E1"},`+
		`{"line_id":"E2","unit_conversion":{"id":"dozen.pcs","revision":2}}]}]}`)
	if a.status != http.StatusOK || a.body["object_id"] != "dcl-1" || len(cnx.prepared) != 1 || cnx.prepared[0].Items[0].Lines[1].UnitConversion == nil ||
		cnx.prepared[0].Items[0].Lines[1].UnitConversion.Revision != 2 || cnx.prepared[0].CodeTableVersion != "CN-2026" {
		t.Fatalf("%d %v %+v", a.status, a.body, cnx.prepared)
	}
	a = post("declaration_case.confirm", `{"declaration_id":"dcl-1","expected_version":1,"version":1,"evidence_ids":["e1"],"reason":"核对"}`)
	if a.status != http.StatusUnprocessableEntity || a.body["code"] != cnxapi.CodeDeclarationMappingUnverified || cnx.confirmed[0].Version != 1 {
		t.Fatalf("%d %v", a.status, a.body)
	}
	a = post("declaration_case.record_submission", `{"declaration_id":"dcl-1","expected_version":2,"operation_reference":"op.1",`+
		`"submitter":{"kind":"broker","party_id":"broker.1","name":"合成报关行"},"submitted_at":"2026-11-12T08:00:00Z","evidence_ids":["e1"]}`)
	if a.status != http.StatusConflict || a.body["code"] != cnxapi.CodeExternalResultUnknown || cnx.submitted[0].Submitter.PartyID != "broker.1" {
		t.Fatalf("%d %v", a.status, a.body)
	}
	a = post("declaration_case.record_receipt", `{"declaration_id":"dcl-1","expected_version":5,"receipt_id":"r1","outcome":"amended",`+
		`"external":{"sequence":4,"status_code":"M","original_text":"改单","received_at":"2026-11-12T08:00:00Z"},"mapping_version":"syn.v1",`+
		`"declaration_number":"SYN-1","exported_at":"2026-11-11T16:00:00Z","amended_items":[{"item_no":1,"quantity":"45"}],"evidence_ids":["e1"]}`)
	if r := cnx.receipts; a.status != http.StatusConflict || a.body["code"] != cnxapi.CodeDeclarationReceiptStale || r[0].External == nil ||
		r[0].External.Sequence != 4 || r[0].AmendedItems[0].Quantity != "45" || r[0].MappingVersion != "syn.v1" || r[0].ExportedAt == "" {
		t.Fatalf("%d %v %+v", a.status, a.body, r)
	}
	a = post("document_bundle.check", `{"export_lot_id":"lot.1","expected_version":0,"expect_complete":true,"requirements":[{"requirement_id":"fx",`+
		`"kind":"fund_record","version":"1","status":"present","owner":{"kind":"role","id":"treasury"},"due_on":"2026-12-31","evidence_ids":["b1"],`+
		`"fund_record_ids":["fr-1"]}]}`)
	if c := cnx.checked; a.status != http.StatusConflict || a.body["code"] != cnxapi.CodeDocumentRequirementUnverified || !c[0].ExpectComplete ||
		c[0].Requirements[0].Owner.Kind != "role" || c[0].Requirements[0].FundRecordIDs[0] != "fr-1" {
		t.Fatalf("%d %v %+v", a.status, a.body, c)
	}
	a = post("external_reconciliation.resolve", `{"bundle_id":"dbn-1","expected_version":2,"reconciliation_id":"rec.1",`+
		`"subject":{"kind":"declaration","id":"dcl-1"},"measure":"amount","currency":{"id":"USD","revision":1},`+
		`"external":{"value":"960.00","source":"报关单","evidence_ids":["e1"]},"internal":{"value":"958.50","source":"商业发票"},`+
		`"responsibility":{"kind":"internal","note":"口径"},"adjustment":{"kind":"approved","note":"桥接"},"reason":"差异"}`)
	if r := cnx.reconciled; a.status != http.StatusOK || a.body["status"] != "complete" || r[0].Currency == nil || r[0].Internal.Value != "958.50" ||
		r[0].Adjustment.Kind != "approved" || r[0].Subject.ID != "dcl-1" {
		t.Fatalf("%d %v %+v", a.status, a.body, r)
	}
	for _, route := range []struct{ path, member string }{{"/api/brant/v1/cnx/declaration_cases", "declaration_cases"},
		{"/api/brant/v1/cnx/document_bundles", "document_bundles"}} {
		a = call(t, h, http.MethodGet, route.path, "Bearer good", "")
		if list, ok := a.body[route.member].([]any); a.status != http.StatusOK || !ok || len(list) != 0 {
			t.Fatalf("%s: %d %v", route.path, a.status, a.body)
		}
	}
}
