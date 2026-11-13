//go:build integration

package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CNX opening refund material (S5 unit C10, WBS S5-20; tactical model CNX-AGG-TaxRefundCase value object
// OpeningPosition; SM-CNX-TAXCASE; CNX-INV-004; Context Map MD-51; baseline 11.2 期初、防重、切换; CN-R04, CN-R08,
// CN-R09, CN-E01; CN-AT16 CNX half, BT17) on PostgreSQL 16, in C3b's invoice world (purchase invoices imported by
// the purchaser and verified by the tax specialist tax.1, POL's CNY and export tax policy, STL's fund records). The
// export tax policy export.tax.hist is valid from 400 days ago, published now: at the cutover the professional
// records the policy that applied to exports before it. Every quantity and amount is synthetic; no refund is
// computed.

// openingRegister is the old tax register's export at the cutoff, an EVD source.
func (w *invoiceWorld) openingRegister() string {
	return w.source(w.purchaser, "tax.register", "合成退税台账截止日导出：进项发票剩余用途余额与在途退税申报（测试数据，非税务结论）")
}

func (w *invoiceWorld) historicPolicy() {
	w.t.Helper()
	_, err := w.saveConfig(w.author, polapi.KindExportTaxPolicy, "export.tax.hist", 0, time.Now().AddDate(0, 0, -400), time.Time{},
		exportTax(polapi.RefundExemptRefund, w.review))
	w.must(err)
	_, err = w.c.POL.Publish(w.reviewer, polapi.Publish{IdempotencyKey: w.key(), Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax.hist",
		ExpectedVersion: 1, Revision: 1, EvidenceIDs: []string{w.review}, Reason: "切换时登记截止前适用的合成退税政策"})
	w.must(err)
}

func balanceOpening(id, document, invoice string, cutoff time.Time, evidence string, consumed ...string) cnxapi.RecordTaxOpening {
	c := cnxapi.RecordTaxOpening{OpeningID: id, OpeningKind: cnxapi.OpeningInvoiceBalance, CutoffAt: kernel.FormatTimestamp(cutoff),
		SourceDocument: document, EvidenceIDs: []string{evidence}, PurchaseInvoiceID: invoice}
	for i := 0; i+1 < len(consumed); i += 2 {
		c.Balances = append(c.Balances, cnxapi.OpeningBalance{LineNo: 1, Purpose: consumed[i], Consumed: consumed[i+1]})
	}
	return c
}

func caseOpening(id, document, reference string, cutoff time.Time, evidence, invoice, quantity string, p cnxapi.OpeningPosition) cnxapi.RecordTaxOpening {
	return cnxapi.RecordTaxOpening{OpeningID: id, OpeningKind: cnxapi.OpeningRefundCase, CutoffAt: kernel.FormatTimestamp(cutoff), SourceDocument: document,
		EvidenceIDs: []string{evidence}, Export: &cnxapi.OpeningExport{Reference: reference, ExportedAt: kernel.FormatTimestamp(cutoff.AddDate(0, 0, -30)),
			OrderReference: "SO-LEGACY-42", Lines: []cnxapi.OpeningExportLine{{LineID: "1", Quantity: quantity, Unit: "PCS"}}},
		PolicyID: "export.tax.hist", Allocations: []cnxapi.OpeningAllocation{{AllocationID: "oa." + id, ExportLineID: "1", PurchaseInvoiceID: invoice,
			LineNo: 1, Purpose: cnxapi.PurposeExportRefund, Quantity: quantity}}, Position: &p}
}

func (w *invoiceWorld) opening(c cnxapi.RecordTaxOpening) (cnxapi.TaxOpeningReceipt, error) {
	c.IdempotencyKey = w.key()
	return w.cnx.RecordTaxOpening(w.tax, c)
}

// CN-AT16 CNX half (未退税资料导入: 截止、来源余额及已办状态可核对；上线不重复生成用途占用; MD-51, CN-R09):
//
//   - Invoice SYNOPEN1 (1,000 PCS) had 600 consumed for export_refund by declarations finished before the cutoff:
//     its opening balance leaves 400 (the view: allocated 0, opening consumed 600, available 400; export_exemption
//     keeps 1,000). A post-cutoff allocation of 401 to a FUL lot is TAX_EVIDENCE_EXCEEDED, 400 is allocated.
//   - Recorded twice — the same register entry, or another entry for the same invoice — OPENING_BALANCE_DUPLICATE.
//   - An export declared before the cutoff (declaration 223320260001234567, 300 PCS of invoice SYNOPEN2) was
//     approved at the cutoff (CNY 390.00, approval text and evidence): its opening case opens approved at version
//     1 with its allocation standing, binds export.tax.hist (valid at the export date), and shows its opening; the
//     invoice keeps 700. After go-live record_result records the receipt on an STL fund record: received.
//   - Another export, submitted at the cutoff (200 of SYNOPEN3), is rejected after go-live (the external result):
//     its evidence returns (500 available), while SYNOPEN1's consumption stays — no local action restores it. A
//     second opening case of an export with a live case is OPENING_BALANCE_DUPLICATE; an export CNX declared itself
//     is REFERENCE_INVALID.
//   - tax_opening.get and .list reconcile each item; three tax_opening_recorded.v1 events.
func TestCNAT16OpeningRefundMaterialReconciles(t *testing.T) {
	w := newInvoiceWorld(t)
	w.historicPolicy()
	register := w.openingRegister()
	cutoff := kernel.Instant(time.Now().Add(-10 * 24 * time.Hour).Truncate(time.Second))
	a := w.invoice(w.facts("SYNOPEN1", "1000"))
	r, err := w.opening(balanceOpening("op.a", "TR-REG/0042", a.PurchaseInvoiceID, cutoff, register, cnxapi.PurposeExportRefund, "600",
		cnxapi.PurposeExportExemption, "0"))
	w.must(err)
	if r.Status != "recorded" || r.RowVersion != 1 || r.OpeningKind != cnxapi.OpeningInvoiceBalance || r.PurchaseInvoiceID != a.PurchaseInvoiceID ||
		!r.CutoffAt.Equal(cutoff) {
		t.Fatalf("invoice balance %+v", r)
	}
	v := w.view(a.PurchaseInvoiceID)
	if b := v.Balances; len(b) != 2 || b[0].Purpose != cnxapi.PurposeExportRefund || b[0].Allocated != "0" || b[0].OpeningConsumed != "600" ||
		b[0].Available != "400" || b[1].OpeningConsumed != "0" || b[1].Available != "1000" {
		t.Fatalf("balances after the opening %+v", v.Balances)
	}
	w.lot("lot.o1", time.Now().UTC(), "x1", "500")
	_, err = w.allocate(w.tax, "lot.o1", alloc("t401", "x1", a.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "401"))
	if code(err) != cnxapi.CodeTaxEvidenceExceeded {
		t.Fatalf("401 of the 400 left: %s", code(err))
	}
	_, err = w.allocate(w.tax, "lot.o1", alloc("t400", "x1", a.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "400"))
	w.must(err)
	for name, again := range map[string]struct {
		cmd  cnxapi.RecordTaxOpening
		path string
	}{
		"the same register entry":         {balanceOpening("op.a2", "TR-REG/0042", a.PurchaseInvoiceID, cutoff, register, cnxapi.PurposeExportRefund, "0"), "/args/source_document"},
		"another entry, the same invoice": {balanceOpening("op.a3", "TR-REG/0043", a.PurchaseInvoiceID, cutoff, register, cnxapi.PurposeExportRefund, "0"), "/args/purchase_invoice_id"},
	} {
		if _, err := w.opening(again.cmd); code(err) != cnxapi.CodeOpeningBalanceDuplicate || path(err) != again.path {
			t.Fatalf("%s: %s %s", name, code(err), path(err))
		}
	}

	cny := &cnxapi.CurrencyRef{ID: w.cny.ConfigID, Revision: w.cny.Revision}
	b := w.invoice(w.facts("SYNOPEN2", "1000"))
	notice := w.source(w.purchaser, "approval.notice", "合成退税核准通知（截止前，测试数据）")
	approved := cnxapi.OpeningPosition{Stage: cnxapi.TaxApproved, Currency: cny, Amount: "390.00", ResultText: "合成核准通知：核准退税 390.00 元（测试数据）",
		EvidenceIDs: []string{notice}}
	oc, err := w.opening(caseOpening("op.b", "TR-REG/0050", "223320260001234567", cutoff, register, b.PurchaseInvoiceID, "300", approved))
	w.must(err)
	if oc.Status != cnxapi.TaxApproved || oc.CaseID == "" || oc.ResultID == "" || !slices.Equal(oc.AllocationIDs, []string{"oa.op.b"}) {
		t.Fatalf("opening case %+v", oc)
	}
	tc := w.taxCase(oc.CaseID)
	if tc.Status != cnxapi.TaxApproved || tc.RowVersion != 1 || tc.ExportLotID != "223320260001234567" || tc.OrderID != "" ||
		tc.Policy.PolicyID != "export.tax.hist" || tc.Policy.Revision != 1 || tc.Opening == nil || tc.Opening.OpeningID != "op.b" ||
		tc.Opening.Position != cnxapi.TaxApproved || tc.Opening.OrderReference != "SO-LEGACY-42" || len(tc.Allocations) != 1 ||
		tc.Allocations[0].Status != cnxapi.EvidenceStanding || tc.Allocations[0].ShipmentID != "223320260001234567" || len(tc.Results) != 1 ||
		tc.Results[0].From != cnxapi.FromOpening || tc.Amounts.ApprovedMinor != "39000" || !tc.ExportedAt.Equal(cutoff.AddDate(0, 0, -30)) {
		t.Fatalf("opening case view %+v", tc)
	}
	if v := w.view(b.PurchaseInvoiceID); balance(v, 1, cnxapi.PurposeExportRefund) != "300/700" {
		t.Fatalf("invoice SYNOPEN2 %+v", v.Balances)
	}
	paid := w.fund("fund.op.b", "390.00", w.cny, w.bankCNY)
	received, err := w.record(w.tax, cnxapi.TaxCaseReceipt{Receipt: cnxapi.Receipt{RowVersion: tc.RowVersion}, CaseID: oc.CaseID},
		cnxapi.RecordTaxResult{Stage: cnxapi.TaxReceived, Currency: cny, Amount: "390.00", FundRecordID: paid})
	if err != nil || received.Status != cnxapi.TaxReceived || received.RowVersion != 2 {
		t.Fatalf("received after go-live %+v %v", received, err)
	}

	c := w.invoice(w.facts("SYNOPEN3", "500"))
	submitted := cnxapi.OpeningPosition{Stage: cnxapi.TaxSubmitted, ExternalReference: "SB-LEGACY-77", EvidenceIDs: []string{register}}
	sc, err := w.opening(caseOpening("op.c", "TR-REG/0051", "223320260007654321", cutoff, register, c.PurchaseInvoiceID, "200", submitted))
	w.must(err)
	if v := w.view(c.PurchaseInvoiceID); balance(v, 1, cnxapi.PurposeExportRefund) != "200/300" {
		t.Fatalf("invoice SYNOPEN3 while submitted %+v", v.Balances)
	}
	_, err = w.opening(caseOpening("op.c2", "TR-REG/0052", "223320260007654321", cutoff, register, c.PurchaseInvoiceID, "100", submitted))
	if code(err) != cnxapi.CodeOpeningBalanceDuplicate || path(err) != "/args/export/reference" {
		t.Fatalf("a second case of the export: %s %s", code(err), path(err))
	}
	_, err = w.opening(caseOpening("op.d", "TR-REG/0053", "SYN-DECL-lot.o1", cutoff, register, c.PurchaseInvoiceID, "100", submitted))
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/export/reference" {
		t.Fatalf("an export CNX declared itself: %s %s", code(err), path(err))
	}
	rejected, err := w.record(w.tax, cnxapi.TaxCaseReceipt{Receipt: cnxapi.Receipt{RowVersion: 1}, CaseID: sc.CaseID},
		cnxapi.RecordTaxResult{Stage: cnxapi.TaxRejected, ResultText: "合成：不予核准（测试数据）", EvidenceIDs: []string{register}})
	if err != nil || rejected.Status != cnxapi.TaxRejected {
		t.Fatalf("rejected %+v %v", rejected, err)
	}
	if v := w.view(c.PurchaseInvoiceID); balance(v, 1, cnxapi.PurposeExportRefund) != "0/500" {
		t.Fatalf("the external rejection releases the opening case's evidence %+v", v.Balances)
	}
	if v := w.view(a.PurchaseInvoiceID); v.Balances[0].Allocated != "400" || v.Balances[0].OpeningConsumed != "600" || v.Balances[0].Available != "0" {
		t.Fatalf("what finished declarations consumed never returns %+v", v.Balances)
	}

	got, err := w.cnx.GetTaxOpening(w.tax, "op.a")
	w.must(err)
	refund := slices.IndexFunc(got.Lines, func(l cnxapi.TaxOpeningLineView) bool { return l.Purpose == cnxapi.PurposeExportRefund })
	if len(got.Lines) != 2 || refund < 0 || got.Lines[refund].Consumed != "600" || got.Lines[refund].RemainingAtCut != "400" ||
		got.Lines[refund].AllocatedNow != "400" || got.Lines[refund].AvailableNow != "0" || got.SourceDocument != "TR-REG/0042" || len(got.Evidence) != 1 {
		t.Fatalf("tax_opening.get %+v", got)
	}
	list, err := w.cnx.TaxOpenings(w.tax)
	w.must(err)
	if len(list) != 3 || list[1].Case == nil || list[2].Case == nil {
		t.Fatalf("tax_opening.list %+v", list)
	}
	if n := len(w.cnxEvents(cnxapi.EventTaxOpeningRecorded)); n != 3 {
		t.Fatalf("%d tax_opening_recorded events", n)
	}
}

// tax_refund_case.record_opening is human only: an AI agent holding the permission is PERMISSION_DENIED. Over HTTP
// on the composed process: the tax specialist records an invoice balance and reconciles it; the same entry again is
// 409 OPENING_BALANCE_DUPLICATE; a position that is an outcome (received) is a schema refusal at /args/position/stage;
// GET tax_openings/{id} and the list answer it.
func TestTaxOpeningsAreHumanOnlyAndOverHTTP(t *testing.T) {
	w := newInvoiceWorld(t)
	register := w.openingRegister()
	cutoff := kernel.Instant(time.Now().Add(-10 * 24 * time.Hour).Truncate(time.Second))
	a := w.invoice(w.facts("SYNOPENH", "100"))
	agent := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("ai", "agent.opening"),
		Permissions: []string{cnxapi.CommandRecordTaxOpening, "cnx.read", "evd.read", "pol.read"}, DataScope: "company"})
	ai := balanceOpening("op.ai", "TR-REG/AI", a.PurchaseInvoiceID, cutoff, register, cnxapi.PurposeExportRefund, "10")
	ai.IdempotencyKey = w.key()
	if _, err := w.cnx.RecordTaxOpening(agent, ai); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("AI agent: %s", code(err))
	}
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	tax := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "tax.1", "human"), n: 300}
	args := map[string]any{"opening_id": "op.http", "kind": "invoice_balance", "cutoff_at": kernel.FormatTimestamp(cutoff),
		"source_document": "TR-REG/HTTP", "evidence_ids": []any{register}, "purchase_invoice_id": a.PurchaseInvoiceID,
		"balances": []any{map[string]any{"line_no": 1, "purpose": "export_refund", "consumed": "60"}}}
	body := map[string]any{"contract": cnxapi.Contract, "command": cnxapi.CommandRecordTaxOpening, "idempotency_key": "cnx.http.opening", "args": args}
	status, res := tax.do("POST", "/api/brant/v1/cnx/commands/tax_refund_case.record_opening", body)
	if status != http.StatusOK || res["object_id"] != "op.http" || res["status"] != "recorded" {
		t.Fatalf("record %d %v", status, res)
	}
	if status, rec := tax.do("POST", "/api/brant/v1/cnx/commands/tax_refund_case.record_opening/reconcile", body); status != http.StatusOK ||
		rec["state"] != "committed" || rec["action_id"] != res["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	body["idempotency_key"] = "cnx.http.opening.2"
	if status, out := tax.do("POST", "/api/brant/v1/cnx/commands/tax_refund_case.record_opening", body); status != http.StatusConflict ||
		out["code"] != cnxapi.CodeOpeningBalanceDuplicate {
		t.Fatalf("again: %d %v", status, out)
	}
	outcome := map[string]any{"opening_id": "op.http.case", "kind": "refund_case", "cutoff_at": kernel.FormatTimestamp(cutoff),
		"source_document": "TR-REG/HTTP2", "evidence_ids": []any{register},
		"export": map[string]any{"reference": "223320260000000001", "exported_at": kernel.FormatTimestamp(cutoff.AddDate(0, 0, -5)),
			"lines": []any{map[string]any{"line_id": "1", "quantity": "10", "unit": "PCS"}}},
		"policy_id": "export.tax", "allocations": []any{map[string]any{"allocation_id": "oa.http", "export_line_id": "1",
			"purchase_invoice_id": a.PurchaseInvoiceID, "line_no": 1, "purpose": "export_refund", "quantity": "10"}},
		"position": map[string]any{"stage": "received"}}
	bad := map[string]any{"contract": cnxapi.Contract, "command": cnxapi.CommandRecordTaxOpening, "idempotency_key": "cnx.http.opening.3", "args": outcome}
	if status, out := tax.do("POST", "/api/brant/v1/cnx/commands/tax_refund_case.record_opening", bad); status != http.StatusUnprocessableEntity ||
		out["code"] != kernel.CodeSchemaInvalid {
		t.Fatalf("a received position: %d %v", status, out)
	}
	status, v := tax.do("GET", "/api/brant/v1/cnx/tax_openings/op.http", nil)
	lines, _ := v["lines"].([]any)
	if status != http.StatusOK || v["kind"] != "invoice_balance" || len(lines) != 1 || lines[0].(map[string]any)["available_now"] != "40" {
		t.Fatalf("tax_opening.get %d %v", status, v)
	}
	status, list := tax.do("GET", "/api/brant/v1/cnx/tax_openings", nil)
	if items, _ := list["tax_openings"].([]any); status != http.StatusOK || len(items) != 1 {
		t.Fatalf("tax_opening.list %d %v", status, list)
	}
}
