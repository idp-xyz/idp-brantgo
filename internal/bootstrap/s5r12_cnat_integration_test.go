//go:build integration

package bootstrap_test

import (
	"net/http"
	"testing"
	"time"

	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5R-D20 on CN-AT05's chain (CNX-INV-005, SM-CNX-INVOICE; R55 PurchaseInvoiceRef v1): the blue invoice
// is allocated and the case is submitted. A partial red invoice is refused until the tax specialist
// records the external rollback of that invoice's evidence; verified, it takes 24 PCS back and the
// purchase payable shows invoice_reversed. Unit tests: R10 TestS5RPartialRedReversalRollbackAndPurchaseInvoiceRefD20,
// R8 TestS5RD20PayableAdjustmentAndInvoiceReversed.
func TestCNATSuite05PartialRedRollbackAndPayableD20(t *testing.T) {
	c := newCNAT(t, "S5R-D20")
	c.toLot()
	c.declaration()
	c.payable()
	c.invoice()
	c.taxCase()
	pay := c.get(brcTreasury, "/api/brant/v1/stl/payables/stmt.purchase.1")
	if pay["invoice_reversed"] == true {
		t.Fatalf("the payable is reversed before any red invoice %v", pay["invoice_reversed"])
	}
	c.observe("应付复核前: stmt.purchase.1 invoice_reversed %v", pay["invoice_reversed"])

	c.phaseOf("the case is filed")
	tc := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+c.taxCaseID)
	rv := brcNum(tc["row_version"])
	step := func(stage string, extra map[string]any) {
		c.t.Helper()
		args := map[string]any{"case_id": c.taxCaseID, "expected_version": rv, "stage": stage}
		for k, v := range extra {
			args[k] = v
		}
		out := c.ok(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxResult, args)
		rv = brcNum(out["row_version"])
	}
	c.source(brcTaxOfficer, "tax.submission.d20", "合成退税申报回执：申报号 SB-D20-0001（测试数据）。")
	step(cnxapi.TaxPlanned, map[string]any{"currency": c.usdRef(), "amount": "78.00", "tax_period": "2026-11",
		"review": map[string]any{"reviewer": "合成税务师（测试）", "evidence_ids": []string{"tax.review"}}})
	step(cnxapi.TaxSubmitted, map[string]any{"external_reference": "SB-D20-0001", "evidence_ids": []string{"tax.submission.d20"}})
	if got := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+c.taxCaseID)["status"]; got != cnxapi.TaxSubmitted {
		t.Fatalf("submitted %v", got)
	}

	c.phaseOf("partial red waits for the rollback")
	c.scanInvoice("invoice.red.d20", "33002611020", "24")
	red := cnatInvoice("33002611020", "red", "24", "33002611001")
	id, row := c.importInvoice("human", brcTaxOfficer, "invoice.red.d20", red)
	verify := map[string]any{"purchase_invoice_id": id, "expected_version": row, "decision": "verify", "facts": red,
		"matches": []any{cnatMatch("L1", "24")}}
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandVerifyInvoice, verify, http.StatusUnprocessableEntity, kernel.CodeInvalidState, "/args/facts/red_of")
	c.source(brcTaxOfficer, "rollback.d20", "合成进货凭证信息回退确认：蓝票 33002611001 已从已申报案件回退（测试数据）。")
	rolled := c.ok(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxResult, map[string]any{"case_id": c.taxCaseID,
		"expected_version": rv, "stage": cnxapi.TaxEvidenceRolledBack, "purchase_invoice_id": c.invoiceID,
		"result_text": "进货凭证信息回退已确认（合成）", "evidence_ids": []string{"rollback.d20"}})
	if rolled["status"] != cnxapi.TaxSubmitted {
		t.Fatalf("a rollback keeps the case submitted %v", rolled)
	}
	verified := c.ok(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandVerifyInvoice, verify)
	if verified["status"] != "verified" {
		t.Fatalf("verified after the rollback %v", verified)
	}
	blue := c.get(brcTaxOfficer, "/api/brant/v1/cnx/purchase_invoices/"+c.invoiceID)
	if blue["status"] != "verified" || blue["reversal"] != cnxapi.ReversalPartial {
		t.Fatalf("partial reversal %v", blue)
	}
	pay = c.get(brcTreasury, "/api/brant/v1/stl/payables/stmt.purchase.1")
	if pay["invoice_reversed"] != true {
		t.Fatalf("payable invoice_reversed %v", pay["invoice_reversed"])
	}
	c.observe("部分红冲: red %s of 24 PCS refused %s /args/facts/red_of until evidence_rolled_back; then verified, blue %s stays verified reversal partial; payable invoice_reversed true; the case stays submitted",
		id, kernel.CodeInvalidState, c.invoiceID)
}

// S5R-D22 (FundRecordRef v1): document_bundle.check refuses a fund record STL has reversed, and so does
// tax_refund_case.record_result received. A check and a result recorded before the reversal keep the
// record and show it reversed. Unit tests: R13 TestS5RD22TheBundleRefusesAReversedFundRecord and
// TestS5RD22TheReceivedRefundRefusesAReversedFundRecord.
func TestCNATSuite22ReversedFundRecordD22(t *testing.T) {
	c := newCNAT(t, "S5R-D22")
	c.toLot()
	c.declaration()
	c.invoice()
	c.taxCase()
	c.party(brcBuyer, "tax.bureau", []string{ptyapi.RoleSupplier}, "evidence")

	c.phaseOf("the bundle")
	due := time.Now().UTC().AddDate(0, 0, 30).Format(time.DateOnly)
	record := func(id, amount, reference string) {
		c.t.Helper()
		c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, c.fund(id, stlapi.FundReceipt, map[string]any{
			"counterparty_id": "customer.1", "currency": cfgRef("currency", "USD", 1), "bank_account": c.bank,
			"received_amount": amount, "bank_reference": reference}))
	}
	record("fr.kept", "100.00", "MT103-D22-KEPT")
	record("fr.gone", "50.00", "MT103-D22-GONE")
	req := func(ids []string) map[string]any {
		return map[string]any{"requirement_id": "fx", "kind": cnxapi.RequirementFundRecord, "version": "1", "status": "verified",
			"owner": map[string]any{"kind": "role", "id": "documentation"}, "due_on": due, "evidence_ids": []string{"advice.fr.kept"},
			"basis": "原件与合成资料逐项核对一致", "fund_record_ids": ids}
	}
	bundle := c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandCheckBundle, map[string]any{"export_lot_id": "lot.1",
		"expected_version": 0, "requirements": []any{req([]string{"fr.kept"})}})
	c.ok(brcCreditManager1, "stl", stlapi.Contract, stlapi.CommandReverseFund, map[string]any{"fund_record_id": "fr.gone",
		"expected_version": 1, "reason": "银行冲正入账（合成）"})
	c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandCheckBundle, map[string]any{"export_lot_id": "lot.1",
		"expected_version": bundle["row_version"], "requirements": []any{req([]string{"fr.kept", "fr.gone"})}},
		http.StatusConflict, cnxapi.CodeReferenceStale, "/args/requirements/0/fund_record_ids/1")

	c.phaseOf("the received refund")
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, c.fund("fund.d22.gone", stlapi.FundNonGoods, map[string]any{
		"counterparty_id": "tax.bureau", "currency": cfgRef("currency", "USD", 1), "bank_account": c.bank,
		"received_amount": "78.00", "bank_reference": "TAX-D22-GONE"}))
	c.ok(brcCreditManager1, "stl", stlapi.Contract, stlapi.CommandReverseFund, map[string]any{"fund_record_id": "fund.d22.gone",
		"expected_version": 1, "reason": "银行冲正退税款（合成）"})
	tc := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+c.taxCaseID)
	rv := tc["row_version"]
	advance := func(stage string, extra map[string]any) {
		c.t.Helper()
		args := map[string]any{"case_id": c.taxCaseID, "expected_version": rv, "stage": stage}
		for k, v := range extra {
			args[k] = v
		}
		out := c.ok(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxResult, args)
		rv = out["row_version"]
	}
	c.source(brcTaxOfficer, "tax.submission.d22", "合成退税申报回执：申报号 SB-D22-0001（测试数据）。")
	c.source(brcTaxOfficer, "tax.approval.d22", "合成退税核准通知：核准 78.00 USD（测试数据）。")
	advance(cnxapi.TaxPlanned, map[string]any{"currency": c.usdRef(), "amount": "78.00", "tax_period": "2026-11",
		"review": map[string]any{"reviewer": "合成税务师（测试）", "evidence_ids": []string{"tax.review"}}})
	advance(cnxapi.TaxSubmitted, map[string]any{"external_reference": "SB-D22-0001", "evidence_ids": []string{"tax.submission.d22"}})
	advance(cnxapi.TaxApproved, map[string]any{"currency": c.usdRef(), "amount": "78.00", "result_text": "合成核准通知（测试数据）",
		"evidence_ids": []string{"tax.approval.d22"}})
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxResult, map[string]any{"case_id": c.taxCaseID,
		"expected_version": rv, "stage": cnxapi.TaxReceived, "currency": c.usdRef(), "amount": "78.00", "fund_record_id": "fund.d22.gone"},
		http.StatusConflict, cnxapi.CodeReferenceStale, "/args/fund_record_id")
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, c.fund("fund.d22.paid", stlapi.FundNonGoods, map[string]any{
		"counterparty_id": "tax.bureau", "currency": cfgRef("currency", "USD", 1), "bank_account": c.bank,
		"received_amount": "78.00", "bank_reference": "TAX-D22-PAID"}))
	advance(cnxapi.TaxReceived, map[string]any{"currency": c.usdRef(), "amount": "78.00", "fund_record_id": "fund.d22.paid"})
	c.ok(brcCreditManager1, "stl", stlapi.Contract, stlapi.CommandReverseFund, map[string]any{"fund_record_id": "fund.d22.paid",
		"expected_version": 1, "reason": "到账后银行冲正（合成）"})
	v := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+c.taxCaseID)
	results := brcList(v["results"])
	last := brcMap(results[len(results)-1])
	if v["status"] != cnxapi.TaxReceived || last["fund_record_id"] != "fund.d22.paid" || last["fund_record_reversed"] != true {
		t.Fatalf("the result keeps the reversed record %v", last)
	}
	c.ok(brcCreditManager1, "stl", stlapi.Contract, stlapi.CommandReverseFund, map[string]any{"fund_record_id": "fr.kept",
		"expected_version": 1, "reason": "核对后冲正（合成）"})
	shown := c.get(brcDocumentation, "/api/brant/v1/cnx/document_bundles/"+brcStr(bundle["object_id"]))
	linked := brcDig(shown, "checks", 0, "requirements", 0, "fund_records", 0, "reversed")
	if linked != true {
		t.Fatalf("the linked record shown reversed %v", brcDig(shown, "checks", 0, "requirements", 0))
	}
	c.observe("单证套: linking fr.gone after fund.reverse is %s at /args/requirements/0/fund_record_ids/1; fr.kept stays on the earlier check and is shown reversed",
		cnxapi.CodeReferenceStale)
	c.observe("到账: received on fund.d22.gone is %s at /args/fund_record_id; fund.d22.paid is received and, reversed afterwards, stays on the result with fund_record_reversed true",
		cnxapi.CodeReferenceStale)
}

// S5R-D23 (CN-AT12; POL ConfigAt v1) and S5R-D12: a revision published after the export, whose validity
// covers the export date, shows on the open case as policy_superseded and is what the tax specialist
// rebinds. FUL prepares a certificate of origin that cites a preferential certificate (RCEP).
func TestCNATSuite12ValidityAndRebindD23(t *testing.T) {
	c := newCNAT(t, "S5R-D23")
	c.toShipped()

	c.phaseOf("FUL accepts a preferential certificate of origin")
	c.fileOf(brcDocumentation, "rcep.file", "rcep.txt", "合成 RCEP 原产地证书扫描件（测试数据）。")
	from, until := time.Now().Add(-time.Hour), time.Now().Add(180*24*time.Hour)
	cert := c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRegisterCertificate, map[string]any{
		"expected_version": 0, "certificate_kind": cnxapi.CertificatePreferentialOrigin, "agreement_code": "RCEP",
		"origin_form": cnxapi.OriginFormCertificate, "issuing_body": cnxapi.IssuingBodyCustoms, "number": "RCEP-D12",
		"issuer": "合成海关（测试）", "issued_on": today(), "object": map[string]any{"kind": cnxapi.ObjectShipment, "id": c.shipmentID},
		"market": syntheticClear, "valid_from": kernel.FormatTimestamp(from), "valid_until": kernel.FormatTimestamp(until),
		"file_id": "rcep.file", "basis": "原件与签发机构核验页逐项核对一致（合成）"})
	prepared := c.ok(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandPrepareShipmentDocument, map[string]any{
		"document_set_id": "docs.d12", "shipment_id": c.shipmentID, "expected_version": 0, "document_kind": fulapi.DocumentCertificateOfOrigin,
		"certificate": map[string]any{"number": "RCEP-D12", "file_id": "rcep.file", "issuer": "合成海关（测试）", "issued_on": today(),
			"certificate_ref": map[string]any{"certificate_id": cert["object_id"], "version": 1}}})
	set := c.get(brcDocumentation, "/api/brant/v1/ful/document_sets/docs.d12")
	for _, d := range brcList(brcDig(set, "reconciliation", "differences")) {
		if brcDig(d, "item") == "certificate_applicability" || brcDig(d, "path") == "/certificate_of_origin/certificate_ref" {
			t.Fatalf("FUL refused the preferential certificate %v", set["reconciliation"])
		}
	}
	if prepared["status"] != fulapi.DocumentDraft {
		t.Fatalf("prepare %v", prepared)
	}
	c.observe("S5R-D12: FUL prepared certificate_of_origin citing preferential certificate %s (RCEP, customs); no certificate_applicability difference", cert["object_id"])

	c.phaseOf("lot.a under revision 1")
	c.lotOf("lot.a", c.shipmentID, [3]string{"E1", "S1", "24"}, [3]string{"E2", "S2", "36"})
	c.declare("lot.a", "SYN-2026-D23A", "SW-D23-0001", "E1", "E2")
	c.invoice()
	ra := c.allocate("lot.a", cnatAlloc("alloc.a.E1", "E1", c.invoiceID, "24"))
	caseA := brcStr(ra["object_id"])
	before := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+caseA)
	if brcDig(before, "policy", "revision") != float64(1) || before["policy_superseded"] == true {
		t.Fatalf("revision 1 %v", before["policy"])
	}

	c.phaseOf("a revision published after the export, valid since the chain's start")
	cfg := c.get(brcAdmin, "/api/brant/v1/pol/configs/export_tax_policy/export.tax")
	c.source(brcTaxOfficer, "tax.review.d23", "合成专业复核记录：出口后发布的修订，有效期覆盖已出口日期，方法仍为免抵退（测试）。")
	c.ok(brcAdmin, "pol", polapi.Contract, "config.save_draft", map[string]any{"kind": polapi.KindExportTaxPolicy, "id": "export.tax",
		"expected_version": cfg["root_version"], "valid_from": kernel.FormatTimestamp(c.from), "data": map[string]any{
			"name": "synthetic export tax policy (published after the export)", "refund_method": polapi.RefundExemptCreditRefund,
			"policy_basis": "合成政策依据（出口后发布，测试用）", "professional_review_evidence_id": "tax.review.d23"}})
	cfg = c.get(brcAdmin, "/api/brant/v1/pol/configs/export_tax_policy/export.tax")
	c.ok(brcController, "pol", polapi.Contract, "config.publish", map[string]any{"kind": polapi.KindExportTaxPolicy, "id": "export.tax",
		"expected_version": cfg["root_version"], "revision": 2, "evidence_ids": []string{"evidence", "tax.review.d23"},
		"reason": "出口后发布、有效期覆盖出口日期的修订"})
	after := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+caseA)
	if brcDig(after, "policy", "revision") != float64(1) || after["policy_superseded"] != true || brcDig(after, "policy_at", "revision") != float64(2) {
		t.Fatalf("policy_superseded %v at %v", after["policy"], after["policy_at"])
	}
	rebound := c.ok(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRebindPolicy, map[string]any{"case_id": caseA,
		"expected_version": after["row_version"], "revision": 2, "reason": "出口后发布的修订覆盖出口日期（合成）"})
	bound := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+caseA)
	if rebound["status"] == cnxapi.TaxPlanned || brcDig(bound, "policy", "revision") != float64(2) || bound["policy_superseded"] == true {
		t.Fatalf("rebound %v policy %v superseded %v", rebound, bound["policy"], bound["policy_superseded"])
	}
	c.observe("S5R-D23: export.tax revision 2 published after lot.a's export, valid from the chain's start; the expected case showed policy_superseded and rebind_policy moved it from revision 1 to 2")

	c.phaseOf("a revision that refunds nothing, then planned")
	c.source(brcTaxOfficer, "tax.review.d23b", "合成专业复核记录：方法改为免税，有效期仍覆盖出口日期（测试）。")
	cfg = c.get(brcAdmin, "/api/brant/v1/pol/configs/export_tax_policy/export.tax")
	c.ok(brcAdmin, "pol", polapi.Contract, "config.save_draft", map[string]any{"kind": polapi.KindExportTaxPolicy, "id": "export.tax",
		"expected_version": cfg["root_version"], "valid_from": kernel.FormatTimestamp(c.from), "data": map[string]any{
			"name": "synthetic export tax policy (exempt)", "refund_method": polapi.RefundExempt,
			"policy_basis": "合成政策依据（免税，测试用）", "professional_review_evidence_id": "tax.review.d23b"}})
	cfg = c.get(brcAdmin, "/api/brant/v1/pol/configs/export_tax_policy/export.tax")
	c.ok(brcController, "pol", polapi.Contract, "config.publish", map[string]any{"kind": polapi.KindExportTaxPolicy, "id": "export.tax",
		"expected_version": cfg["root_version"], "revision": 3, "evidence_ids": []string{"evidence", "tax.review.d23b"},
		"reason": "免税修订，有效期覆盖出口日期"})
	fresh := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+caseA)
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRebindPolicy, map[string]any{"case_id": caseA,
		"expected_version": fresh["row_version"], "revision": 3, "reason": "出口退税用途不能改绑到免税"},
		http.StatusUnprocessableEntity, kernel.CodeInvalidState, "/args/revision")
	planned := c.ok(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxResult, map[string]any{"case_id": caseA,
		"expected_version": fresh["row_version"], "stage": cnxapi.TaxPlanned, "currency": c.usdRef(), "amount": "78.00", "tax_period": "2026-11",
		"review": map[string]any{"reviewer": "合成税务师（测试）", "evidence_ids": []string{"tax.review"}}})
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRebindPolicy, map[string]any{"case_id": caseA,
		"expected_version": planned["row_version"], "revision": 2, "reason": "拟申报之后不再改绑"},
		http.StatusUnprocessableEntity, kernel.CodeInvalidState, "/args/case_id")
	c.observe("免税修订不能改绑仍有 export_refund 用途的案件（%s /args/revision）；planned 之后 rebind_policy → %s /args/case_id",
		kernel.CodeInvalidState, kernel.CodeInvalidState)
}
