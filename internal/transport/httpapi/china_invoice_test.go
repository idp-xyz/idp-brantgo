package httpapi

import (
	"context"
	"net/http"
	"testing"

	"go.idp.xyz/idp-bento-go/application"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
)

// fakeInvoices records the invoice and tax refund commands it receives on top of fakeCNX.
type fakeInvoices struct {
	fakeCNX
	imported  []cnxapi.ImportInvoice
	verified  []cnxapi.VerifyInvoice
	allocated []cnxapi.AllocateTaxEvidence
	recorded  []cnxapi.RecordTaxResult
}

func (f *fakeInvoices) ImportInvoice(_ context.Context, cmd cnxapi.ImportInvoice) (cnxapi.InvoiceReceipt, error) {
	f.imported = append(f.imported, cmd)
	return cnxapi.InvoiceReceipt{Receipt: cnxapi.Receipt{ActionID: "act-1", ObjectID: "pin-1", RowVersion: 1, Status: "imported"}}, nil
}

func (f *fakeInvoices) VerifyInvoice(_ context.Context, cmd cnxapi.VerifyInvoice) (cnxapi.InvoiceReceipt, error) {
	f.verified = append(f.verified, cmd)
	return cnxapi.InvoiceReceipt{}, cnxapi.Reject(cnxapi.CodePartyEntityMismatch, "/args/facts/buyer/tax_id", "")
}

func (f *fakeInvoices) AllocateTaxEvidence(_ context.Context, cmd cnxapi.AllocateTaxEvidence) (cnxapi.TaxCaseReceipt, error) {
	f.allocated = append(f.allocated, cmd)
	return cnxapi.TaxCaseReceipt{}, cnxapi.Reject(cnxapi.CodeTaxEvidenceExceeded, "/args/allocations/0/quantity", "")
}

func (f *fakeInvoices) RecordTaxResult(_ context.Context, cmd cnxapi.RecordTaxResult) (cnxapi.TaxCaseReceipt, error) {
	f.recorded = append(f.recorded, cmd)
	return cnxapi.TaxCaseReceipt{Receipt: cnxapi.Receipt{ActionID: "act-2", ObjectID: "trc-1", RowVersion: 3, Status: "approved"}}, nil
}

func (f *fakeInvoices) Invoices(context.Context) ([]cnxapi.InvoiceSummary, error) { return nil, nil }
func (f *fakeInvoices) TaxCases(context.Context) ([]cnxapi.TaxCaseSummary, error) { return nil, nil }

// The invoice and tax refund commands decode by the reflective mapping: nested facts with parties,
// currency, lines and red_of, matches, allocations, review; the refusals keep their codes and
// categories (409 for PARTY_ENTITY_MISMATCH and TAX_EVIDENCE_EXCEEDED); both lists answer empty.
func TestCNXInvoiceAndTaxCommandsDecodeTheirMembers(t *testing.T) {
	cnx := &fakeInvoices{}
	h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: &fakeINQ{}, POL: struct{ POL }{}, REF: struct{ REF }{},
		SUP: struct{ SUP }{}, CST: struct{ CST }{}, TRD: struct{ TRD }{}, PUR: struct{ PUR }{}, CNX: cnx, Verifier: tokens{"Bearer good": alice},
		Clock: application.Clock(fixedClock{})})
	if err != nil {
		t.Fatal(err)
	}
	envelope := func(command, args string) string {
		return `{"contract":"` + cnxapi.Contract + `","command":"` + command + `","idempotency_key":"k1","args":` + args + `}`
	}
	facts := `{"kind":"special_vat","colour":"red","number":"SYN0002","issued_on":"2026-11-10",` +
		`"supplier":{"party_id":"supplier.1","name":"合成供应商","tax_id":"SUPPLIER1"},"buyer":{"name":"合成企业","tax_id":"SYNTHETIC"},` +
		`"currency":{"id":"CNY","revision":1},"lines":[{"line_no":1,"description":"合成针织衫","quantity":"120","unit":"PCS",` +
		`"net_amount":"600.00","tax_rate":"0.13","tax_amount":"78.00"}],"net_total":"600.00","tax_total":"78.00","gross_total":"678.00",` +
		`"red_of":{"kind":"special_vat","number":"SYN0001"}}`
	a := call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/purchase_invoice.import", "Bearer good",
		envelope("purchase_invoice.import", `{"file_id":"f1","candidate":`+facts+`}`))
	if a.status != http.StatusOK || a.body["object_id"] != "pin-1" || len(cnx.imported) != 1 {
		t.Fatalf("%d %v", a.status, a.body)
	}
	c := cnx.imported[0].Candidate
	if c.RedOf == nil || c.RedOf.Number != "SYN0001" || c.Supplier.TaxID != "SUPPLIER1" || c.Currency.Revision != 1 || len(c.Lines) != 1 ||
		c.Lines[0].TaxRate != "0.13" || c.Lines[0].LineNo != 1 || cnx.imported[0].FileID != "f1" {
		t.Fatalf("%+v", cnx.imported[0])
	}
	a = call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/purchase_invoice.verify", "Bearer good", envelope("purchase_invoice.verify",
		`{"purchase_invoice_id":"pin-1","expected_version":1,"decision":"verify","facts":`+facts+
			`,"matches":[{"line_no":1,"commitment_id":"commitment.1","order_line_id":"L1","quantity":"120"}]}`))
	if a.status != http.StatusConflict || a.body["code"] != cnxapi.CodePartyEntityMismatch || cnx.verified[0].Facts == nil ||
		len(cnx.verified[0].Matches) != 1 || cnx.verified[0].Matches[0].OrderLineID != "L1" || cnx.verified[0].ExpectedVersion != 1 {
		t.Fatalf("%d %v %+v", a.status, a.body, cnx.verified)
	}
	a = call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/tax_refund_case.allocate", "Bearer good", envelope("tax_refund_case.allocate",
		`{"export_lot_id":"lot.1","policy_id":"export.tax","allocations":[{"allocation_id":"t1","export_line_id":"x1","purchase_invoice_id":"pin-1",`+
			`"line_no":1,"purpose":"export_refund","quantity":"600","replaces":"t0"}]}`))
	if a.status != http.StatusConflict || a.body["code"] != cnxapi.CodeTaxEvidenceExceeded || cnx.allocated[0].Allocations[0].Replaces != "t0" ||
		cnx.allocated[0].PolicyID != "export.tax" {
		t.Fatalf("%d %v %+v", a.status, a.body, cnx.allocated)
	}
	a = call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/tax_refund_case.record_result", "Bearer good", envelope("tax_refund_case.record_result",
		`{"case_id":"trc-1","expected_version":2,"stage":"planned","currency":{"id":"CNY","revision":1},"amount":"1000.00","tax_period":"2026-11",`+
			`"review":{"reviewer":"合成税务师","evidence_ids":["e1"]}}`))
	if a.status != http.StatusOK || a.body["status"] != "approved" || cnx.recorded[0].Review == nil || cnx.recorded[0].Review.EvidenceIDs[0] != "e1" ||
		cnx.recorded[0].Currency == nil || cnx.recorded[0].TaxPeriod != "2026-11" {
		t.Fatalf("%d %v %+v", a.status, a.body, cnx.recorded)
	}
	for _, route := range []struct{ path, member string }{{"/api/brant/v1/cnx/purchase_invoices", "purchase_invoices"},
		{"/api/brant/v1/cnx/tax_refund_cases", "tax_refund_cases"}} {
		a = call(t, h, http.MethodGet, route.path, "Bearer good", "")
		if list, ok := a.body[route.member].([]any); a.status != http.StatusOK || !ok || len(list) != 0 {
			t.Fatalf("%s: %d %v", route.path, a.status, a.body)
		}
	}
}
