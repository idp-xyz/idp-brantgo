//go:build integration

package bootstrap_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// S5R unit R12 (WBS S5R-12): the S5R decisions on the HTTP chains of S5's D1, D2a and D2b. Each
// scenario below is one decision's end-to-end path. The contexts' own PostgreSQL tests stay the
// detail; these tests run the commands as the archive's humans and record the figures.

// fxConversion has treasury record the bank's sale of sold of fund into CNY received on account at
// rate (S5R-D19: the conversion is attributed to the fund's applications, never applied itself).
func (s *s5sc) fxConversion(id, fund, sold, rate, received string, account map[string]any) {
	s.t.Helper()
	s.source(brcTreasury, "bank."+id, "合成银行结汇水单："+id+"，卖出 "+sold+" USD，汇率 "+rate+"，人民币 "+received+"。")
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, map[string]any{"fund_record_id": id,
		"kind": stlapi.FundFXConversion, "source_fund_record_id": fund, "currency": s.money("CNY"), "bank_account": account,
		"received_amount": received, "value_date": now(), "bank_reference": "FX-" + id, "evidence_ids": []string{"bank." + id},
		"fx": map[string]any{"sold_amount": sold, "bank_rate": rate}})
}

// payOut has treasury record a payment of amount in currency to payee on account.
func (s *s5sc) payOut(id, payee, currency, amount, reference string, account map[string]any) {
	s.t.Helper()
	s.source(brcTreasury, "bank."+id, "合成银行付款回单："+id+"，向 "+payee+" 支付 "+amount+" "+currency+"。")
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, map[string]any{"fund_record_id": id,
		"kind": stlapi.FundPayment, "counterparty_id": payee, "currency": s.money(currency), "bank_account": account,
		"received_amount": amount, "value_date": now(), "bank_reference": reference, "evidence_ids": []string{"bank." + id}})
}

// S5R-D19 (STL-INV-011; baseline 4.6 S-A): the same order as D2b's S-A — 100 PCS at 10.00 USD, bought
// at CNY 40.00, analysed in CNY at 7.0, freight CNY 650 — settled from USD receipts the bank converts
// (200.00 at 7.0, 800.00 at 6.875) and a charge payment of the freight. The business settlement FX
// difference is −100.00, realised; net cash is 6,900.00 − 4,000.00 − 650.00 = 2,250.00. Before the
// freight is paid the result lists it unpaid and net cash is 2,900.00. A payment from another party
// is PARTY_ENTITY_MISMATCH. Unit test: R8 TestS5RD19SAFromUSDReceiptsConversionsAndAChargePayment.
func TestS5R12D19SANetCashFromUSDReceiptsOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "S5R-D19", Title: "S5R-D19 S-A: USD receipts converted at 7.0 and 6.875, freight paid through STL, net cash CNY 2,250.00",
		Design:    []string{"S5R-D19", "baseline 4.6 S-A", "baseline 4.5", "STL-INV-011", "SM-STL-APPLICATION"},
		RestsOn:   []string{"R8", "C9", "D2b"},
		UnitTests: []string{"TestS5RD19SAFromUSDReceiptsConversionsAndAChargePayment (R8)", "TestBaseline46SAOrderResult (C9)"},
		Deviations: []string{
			"Payment terms are C9's 20 % on order confirmation and 80 % 30 days after shipment; the baseline states only the 200 prepayment.",
			"BRC-001's 60.00 pattern is not this order: the freight is provider.1's pre_carriage of CNY 650.00 (S5R-D24), allocated 390.00 + 260.00.",
		},
		Omitted: []string{"a partly converted receipt shown unrealised (R8 TestS5RD19PartialConversionIsUnrealized); an agent paying a charge (application.apply is human only)"}})
	s.dealFoundation(s5scTenant{terms: "20% on order confirmation, 80% 30 days after shipment", stages: []any{
		map[string]any{"stage_id": "P1", "percent": "20", "trigger": polapi.TriggerOrderConfirmed, "days": 0},
		map[string]any{"stage_id": "P2", "percent": "80", "trigger": polapi.TriggerAfterShipment, "days": 30}},
		incoterm: "FCA", place: "TEST NAMED PLACE", calc: "CNY", salesRate: "7.0", purchase: "CNY", skus: []string{"ST01.red.S"}, limit: "10000.00"})
	cny := s.config(polapi.KindBankAccount, "bank.cny.d19", map[string]any{"owner_kind": polapi.OwnerCompany, "owner_id": "company.A",
		"bank_name": "synthetic bank", "account_number": "6222000099990001", "currency": "CNY", "purpose": polapi.AccountBoth})

	d := &s5scDeal{id: "d19", lines: []s5scLine{{"L1", "ST01.red.S", "100", "10.00", "40.00"}}, consignee: "cons.de", freight: "500.00"}
	s.form(d)
	s.want("quote", s.figures["d19.quote"], "payable 100000 USD; cost 450000, profit 250000 CNY; margin rule pass; payments P1 20000, P2 80000")
	s.clear(d, false)
	s.reserve(d, "res.d19")
	s.activate(d)
	s.draft(d, nil)
	s.dispatch("after the activation")

	s.phase = "S5R-D19: USD 200 converted at 7.0, applied to P1"
	s.fundOf("fund.d19.200", "USD", s.bank, "200.00", "MT103-D19-200")
	s.fxConversion("fx.d19.200", "fund.d19.200", "200.00", "7.0", "1400.00", cny)
	s.applyOn("app.d19.pre", "fund.d19.200", s5scStage(d.order(), "P1"), "200.00", nil)
	pre := s.get(brcTreasury, "/api/brant/v1/stl/applications/app.d19.pre")
	s.want("prepayment", fmt.Sprint(pre["amount_minor"], " ", pre["currency"]), "20000 USD")

	s.commit(d, "purchase.d19")
	s.inspect(d, nil)
	s.deliver(d, "delivery.d19", [][2]string{{"L1", "100"}}, [][2]string{{"L1", "100"}})
	s.ship(d, "shipment.60", [][2]string{{"L1", "60"}})
	s.ship(d, "shipment.40", [][2]string{{"L1", "40"}})
	s.settle("stmt.d19.600", "shipment.60", 1)
	s.settle("stmt.d19.400", "shipment.40", 1)
	s.want("statement_600", s.installments("stmt.d19.600"), "P1 20000/20000/0 P2 40000/0/40000")
	s.want("statement_400", s.installments("stmt.d19.400"), "P2 40000/0/40000")

	s.phase = "S5R-D19: USD 800 converted at 6.875, applied 400 + 400"
	s.fundOf("fund.d19.800", "USD", s.bank, "800.00", "MT103-D19-800")
	s.applyOn("app.d19.600", "fund.d19.800", s5scInstallment("stmt.d19.600", "P2"), "400.00", nil)
	s.applyOn("app.d19.400", "fund.d19.800", s5scInstallment("stmt.d19.400", "P2"), "400.00", nil)
	s.fxConversion("fx.d19.800", "fund.d19.800", "800.00", "6.875", "5500.00", cny)
	pos := s.get(brcTreasury, "/api/brant/v1/stl/orders/"+d.order()+"/settlement")
	s.want("receivables_open", pos["open_minor"], "0")

	pay := s.payPurchase(d, "CNY", cny, "4000.00")
	s.want("purchase_paid", fmt.Sprint(pay["amount_minor"], " ", pay["currency"]), "400000 CNY")

	s.phase = "S5R-D19: freight confirmed, unpaid, then paid"
	s.cmd(brcCreditOfficer, "stl", stlapi.Contract, stlapi.CommandRecordCharge, map[string]any{"charge_id": "freight.d19", "expected_version": 0,
		"status": "estimated", "source": map[string]any{"kind": "logistics_offer", "party_id": "provider.1", "reference": "log.d19"},
		"beneficiaries": []any{map[string]any{"kind": "order", "order_id": d.order()}}, "fee_category": s.freight, "currency": s.money("CNY"),
		"conversion": map[string]any{"kind": "same_currency"}, "cost_component": stlapi.ComponentPreCarriage, "amount": "500.00", "occurred_on": today(),
		"analysis_on": today(), "basis": "provider.1 交货前内陆运费报价 log.d19：预计 CNY 500.00（pre_carriage）"})
	s.source(brcTreasury, "freight.bill.d19", "合成货代账单 BILL-D19：provider.1 交货前内陆运费 CNY 650.00（pre_carriage），shipment.60 与 shipment.40。")
	s.cmd(brcCreditOfficer, "stl", stlapi.Contract, stlapi.CommandRecordCharge, map[string]any{"charge_id": "freight.d19", "expected_version": 1,
		"status": "pending", "source": map[string]any{"kind": "supplier_bill", "party_id": "provider.1", "reference": "BILL-D19"}, "amount": "650.00",
		"occurred_on": today(), "analysis_on": today(), "basis": "货代账单 BILL-D19，交货前内陆运费", "evidence_ids": []string{"freight.bill.d19"}})
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandConfirmCharge, map[string]any{"charge_id": "freight.d19", "expected_version": 2,
		"evidence_ids": []string{"freight.bill.d19"}})
	s.cmd(brcCreditOfficer, "stl", stlapi.Contract, stlapi.CommandAllocateCharge, map[string]any{"charge_id": "freight.d19", "expected_version": 3,
		"basis": "quantity", "data_basis": "FUL 实际出运数量 60 / 40", "lines": []any{
			map[string]any{"line_id": "A", "target": map[string]any{"kind": "shipment", "order_id": d.order(), "shipment_id": "shipment.60"}, "weight": "60"},
			map[string]any{"line_id": "B", "target": map[string]any{"kind": "shipment", "order_id": d.order(), "shipment_id": "shipment.40"}, "weight": "40"}}})

	live := s.get(brcTreasury, "/api/brant/v1/stl/orders/"+d.order()+"/result")
	s.want("fx_before_close", fmt.Sprint(brcDig(live, "live", "fx_difference", "difference_minor"), " realised ",
		brcDig(live, "live", "fx_difference", "realized_minor"), " unrealised ", brcDig(live, "live", "fx_difference", "unrealized_minor")),
		"-10000 realised -10000 unrealised 0")
	s.want("net_cash_freight_unpaid", fmt.Sprint("receipts ", brcDig(live, "live", "net_cash", "receipts_minor"), " payments ",
		brcDig(live, "live", "net_cash", "payments_minor"), " unpaid ", brcDig(live, "live", "net_cash", "unpaid_charges_minor"),
		" net ", brcDig(live, "live", "net_cash", "net_minor")), "receipts 690000 payments 400000 unpaid 65000 net 290000")

	s.payOut("pay.d19.other", d.supplierID(), "CNY", "650.00", "TT-D19-OTHER", cny)
	s.refused(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, map[string]any{"application_id": "app.d19.other",
		"fund_record_id": "pay.d19.other", "target": map[string]any{"kind": stlapi.TargetCharge, "charge_id": "freight.d19"}, "amount": "650.00"},
		http.StatusConflict, stlapi.CodePartyEntityMismatch)
	s.payOut("pay.d19.freight", "provider.1", "CNY", "650.00", "TT-D19-FREIGHT", cny)
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, map[string]any{"application_id": "app.d19.freight",
		"fund_record_id": "pay.d19.freight", "target": map[string]any{"kind": stlapi.TargetCharge, "charge_id": "freight.d19"}, "amount": "650.00"})
	ch := s.get(brcTreasury, "/api/brant/v1/stl/charges/freight.d19")
	s.want("charge_payment", fmt.Sprint(ch["cost_component"], " paid ", ch["paid_minor"], " open ", ch["open_minor"], " ", ch["payment_status"]),
		"pre_carriage paid 65000 open 0 paid")

	s.closeDeal(d, "receipt."+d.delivery)
	s.dispatch("after the close")
	v := s.settleResult(d)
	snap := brcMap(brcDig(v, "snapshots", 0, "figures"))
	s.want("confirmed", fmt.Sprint(v["status"], " ", brcDig(v, "live", "confirmed", "result_minor")), "settled 235000")
	s.want("fx", brcDig(snap, "fx_difference", "difference_minor"), "-10000")
	s.want("net_cash", fmt.Sprint("receipts ", brcDig(snap, "net_cash", "receipts_minor"), " charge_payments ",
		brcDig(snap, "net_cash", "charge_payments_minor"), " payments ", brcDig(snap, "net_cash", "payments_minor"),
		" net ", brcDig(snap, "net_cash", "net_minor"), " unpaid ", brcDig(snap, "net_cash", "unpaid_charges_minor")),
		"receipts 690000 charge_payments 65000 payments 465000 net 225000 unpaid 0")
}

// registerSingleUse has doc.1 register a single_use licence (一批一证, the default) of the SKUs for DE
// and customer.1, customs office 2248, valid over [from, until).
func (s *s5sc) registerSingleUse(number, quantity, value string, skus []string, from, until time.Time) string {
	s.t.Helper()
	s.fileOf(brcDocumentation, "licence."+number, "licence-"+number+".txt", "合成出口许可证扫描件："+number+"（测试数据，一批一证）。")
	var specs []any
	for _, sku := range skus {
		specs = append(specs, map[string]any{"sku_id": sku, "spec_revision": 1})
	}
	r := s.cmd(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRegisterLicense, map[string]any{"license_number": number,
		"issuer": "合成发证机关（测试）", "holder": "合成企业", "goods": map[string]any{"sku_specs": specs}, "licensed_quantity": quantity,
		"unit": "PCS", "currency": map[string]any{"id": "USD", "revision": 1}, "licensed_value": value, "destination": syntheticClear,
		"end_user": map[string]any{"party_id": "customer.1", "name": "customer.1"}, "valid_from": kernel.FormatTimestamp(from),
		"valid_until": kernel.FormatTimestamp(until), "file_id": "licence." + number, "usage_mode": cnxapi.UsageSingleUse,
		"customs_office": "2248"})
	return brcStr(r["object_id"])
}

// allocateTo has doc.1 allocate licence to a shipment or an export lot.
func (s *s5sc) allocateTo(licence, allocation, kind, id string) (int, map[string]any) {
	return s.post(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandAllocateLicense, map[string]any{"license_id": licence,
		"allocation_id": allocation, "target": map[string]any{"kind": kind, "id": id}})
}

// S5R-D03 (CNX-INV-002, MD-44): a single_use licence allocated to an export lot is not released on
// local_cancellation while the shipment still stands, nor while a declaration of the lot has been
// submitted. Customs rejects that declaration; the release then frees the outstanding quantity, and
// the licence, never declared successfully, is allocated again.
func TestS5R12D03LocalCancellationReleasesTheLicenceOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "S5R-D03", Title: "S5R-D03: a licence allocated to a lot is released on local cancellation only after customs rejects the declaration",
		Design:    []string{"S5R-D03", "CNX-INV-002", "MD-44"},
		RestsOn:   []string{"R9", "B5", "C3a"},
		UnitTests: []string{"TestS5RD03LocalCancellationReleasesAnUndeclaredAllocation (R9)"},
		Omitted:   []string{"an AI agent is refused (export_license.release is human only): R9's test"}})
	sku := []string{"ST01.red.S"}
	s.dealFoundation(s5scUSDTenant(sku...))
	d := &s5scDeal{id: "d03", lines: []s5scLine{{"L1", "ST01.red.S", "50", "8.00", "5.00"}}, consignee: "cons.de", freight: "10.00"}
	s.form(d)
	s.clear(d, true)
	s.reserve(d, "res.d03")
	s.activate(d)
	s.draft(d, nil)
	s.dispatch("after the activation")
	s.commit(d, "purchase.d03")
	s.inspect(d, nil)
	s.deliver(d, "delivery.d03", [][2]string{{"L1", "50"}}, [][2]string{{"L1", "50"}})

	s.phase = "S5R-D03: allocate, refuse the release, declare, cancel, refuse again"
	status, prepared := s.prepareOn(d, "shipment.d03", [][2]string{{"L1", "50"}})
	if status != http.StatusOK {
		t.Fatalf("prepare %d %v", status, prepared)
	}
	shipVersion := prepared["row_version"]
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandRegisterExportLot, map[string]any{"export_lot_id": "lot.d03",
		"shipment_id": "shipment.d03", "lines": []any{map[string]any{"line_id": "E1", "shipment_line_id": "S1", "quantity": "50"}}})
	at := time.Now().UTC().Truncate(time.Second)
	lic := s.registerSingleUse("SYN-EL-D03", "1000", "8000.00", sku, at.Add(-time.Hour), at.Add(90*24*time.Hour))
	status, alloc := s.allocateTo(lic, "alloc.d03", cnxapi.TargetExportLot, "lot.d03")
	s.want("allocated", status, "200")
	version := alloc["row_version"]
	release := func(id string) (int, map[string]any) {
		return s.post(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandReleaseLicense, map[string]any{"license_id": lic,
			"expected_version": version, "allocation_id": "alloc.d03", "release_id": id, "basis": cnxapi.BasisLocalCancellation,
			"reason": "出运在报关前取消（合成）"})
	}
	status, out := release("rel.d03.1")
	s.want("release_before_cancel", fmt.Sprint(status, " ", out["code"], " ", brcDig(out, "field_errors", 0, "path")),
		"422 "+kernel.CodeInvalidState+" /args/allocation_id")

	prep := s.cmd(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandPrepareDeclaration, map[string]any{"expected_version": 0,
		"export_lot_id": "lot.d03", "code_table_version": codeTable, "customs_office": "2248",
		"items": []any{map[string]any{"item_no": 1, "lines": []any{map[string]any{"line_id": "E1"}}}}})
	decl := brcStr(prep["object_id"])
	confirmed := s.cmd(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandConfirmDeclaration, map[string]any{"declaration_id": decl,
		"expected_version": prep["row_version"], "version": 1, "reason": "申报要素与已核实的申报档案一致（合成）"})
	s.source(brcDocumentation, "sent.D03", "合成报关单人工申报记录：单一窗口提交 lot.d03，操作编号 SW-D03-0001。")
	submitted := s.cmd(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordSubmission, map[string]any{"declaration_id": decl,
		"expected_version": confirmed["row_version"], "operation_reference": "SW-D03-0001",
		"submitter": map[string]any{"kind": "own_clerk", "name": brcDocumentation}, "submitted_at": now(), "evidence_ids": []string{"sent.D03"}})
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandCancelShipment, map[string]any{"shipment_id": "shipment.d03",
		"expected_version": shipVersion, "reason": "客户取消（合成）"})
	status, refused := release("rel.d03.2")
	s.want("release_while_declared", fmt.Sprint(status, " ", refused["code"], " ", brcDig(refused, "field_errors", 0, "path")),
		"422 "+kernel.CodeInvalidState+" /args/basis")

	s.phase = "S5R-D03: customs rejects, the release frees the licence"
	s.source(brcDocumentation, "receipt.text.d03", "合成海关回执：报关单未受理，退回（合成原文）。")
	rejected := s.cmd(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, map[string]any{"declaration_id": decl,
		"expected_version": submitted["row_version"], "receipt_id": "rcpt.d03", "outcome": cnxapi.DeclarationRejected,
		"external":        map[string]any{"sequence": 1, "status_code": "SYN-REJECTED", "original_text": "【合成】海关退回", "received_at": now()},
		"mapping_version": "v1", "declaration_number": "SYN-2026-D03", "evidence_ids": []string{"receipt.text.d03"},
		"reason": "海关退回，未予受理（合成）"})
	s.want("declaration_rejected", rejected["status"], cnxapi.DeclarationRejected)
	status, freed := release("rel.d03.3")
	if status != http.StatusOK {
		t.Fatalf("release after the rejection %d %v", status, freed)
	}
	view := s.get(brcDocumentation, "/api/brant/v1/cnx/export_licenses/"+lic)
	s.want("released", fmt.Sprint(brcDig(view, "usage", "allocated_quantity"), " allocated, ", brcDig(view, "usage", "available_quantity"),
		" available, basis ", brcDig(view, "allocations", 0, "releases", 0, "basis")), "0 allocated, 1000 available, basis "+cnxapi.BasisLocalCancellation)

	status, again := s.prepareOn(d, "shipment.d03b", [][2]string{{"L1", "50"}})
	if status != http.StatusOK {
		t.Fatalf("the cancelled shipment frees the delivery %d %v", status, again)
	}
	status, _ = s.allocateTo(lic, "alloc.d03b", cnxapi.TargetShipment, "shipment.d03b")
	s.want("allocated_again", status, "200")
}

// S5R-D04 (CNX-INV-002): one shipment of two lines is two export lots, each under its own single_use
// licence. The licence's second allocation is refused, and so is an allocation of the shipment beside
// its lots. Both lots allocated, the shipment's release is granted and it confirms.
func TestS5R12D04TwoLicencesTwoLotsOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "S5R-D04", Title: "S5R-D04: two single_use licences cover one shipment through two export lots; a second allocation is refused",
		Design:    []string{"S5R-D04", "CNX-INV-002", "R56", "FUL-INV-002"},
		RestsOn:   []string{"R9", "B5", "C1"},
		UnitTests: []string{"TestS5RD04TwoLicencesThroughTwoExportLotsReleaseTheShipment (R9)"}})
	skus := []string{"ST01.red.S", "ST01.blue.S"}
	s.dealFoundation(s5scUSDTenant(skus...))
	d := &s5scDeal{id: "d04", lines: []s5scLine{{"L1", "ST01.red.S", "60", "8.00", "5.00"}, {"L2", "ST01.blue.S", "60", "8.00", "5.00"}},
		consignee: "cons.de", freight: "60.00"}
	s.form(d)
	s.clear(d, true)
	s.reserve(d, "res.d04")
	s.activate(d)
	s.draft(d, nil)
	s.dispatch("after the activation")
	s.fundOf("fund.d04", "USD", s.bank, "288.00", "MT103-D04")
	s.applyOn("app.d04", "fund.d04", s5scStage(d.order(), "P1"), "288.00", nil)
	s.commit(d, "purchase.d04")
	s.inspect(d, nil)
	s.deliver(d, "delivery.d04", [][2]string{{"L1", "60"}, {"L2", "60"}}, [][2]string{{"L1", "60"}, {"L2", "60"}})

	s.phase = "S5R-D04: two lots, two licences"
	status, ship := s.prepareOn(d, "shipment.d04", [][2]string{{"L1", "60"}, {"L2", "60"}})
	if status != http.StatusOK {
		t.Fatalf("prepare %d %v", status, ship)
	}
	s.shipmentID = "shipment.d04"
	r, e := s.evaluate(fulapi.ScopeShipment)
	s.want("release_without_lots", fmt.Sprint(r["status"], " ", e["code"]), "blocked "+cnxapi.CodeExportLicenseRequired)
	for _, lot := range []struct{ id, line, ship string }{{"lot.d04a", "E1", "S1"}, {"lot.d04b", "E2", "S2"}} {
		s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandRegisterExportLot, map[string]any{"export_lot_id": lot.id,
			"shipment_id": "shipment.d04", "lines": []any{map[string]any{"line_id": lot.line, "shipment_line_id": lot.ship, "quantity": "60"}}})
	}
	now := time.Now().UTC().Truncate(time.Second)
	x := s.registerSingleUse("SYN-EL-D04-X", "1000", "8000.00", skus, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	y := s.registerSingleUse("SYN-EL-D04-Y", "1000", "8000.00", skus, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	status, _ = s.allocateTo(x, "alloc.a", cnxapi.TargetExportLot, "lot.d04a")
	s.want("lot_a", status, "200")
	r, e = s.evaluate(fulapi.ScopeShipment)
	s.want("one_lot_short", fmt.Sprint(r["status"], " ", e["code"]), "blocked "+cnxapi.CodeExportLicenseRequired)
	status, out := s.allocateTo(x, "alloc.b.x", cnxapi.TargetExportLot, "lot.d04b")
	s.want("second_allocation", fmt.Sprint(status, " ", out["code"], " ", brcDig(out, "field_errors", 0, "path")),
		"422 "+kernel.CodeInvalidState+" /args/license_id")
	status, out = s.allocateTo(y, "alloc.whole", cnxapi.TargetShipment, "shipment.d04")
	s.want("shipment_beside_its_lots", fmt.Sprint(status, " ", out["code"], " ", brcDig(out, "field_errors", 0, "path")),
		"422 "+kernel.CodeInvalidState+" /args/target/id")
	status, _ = s.allocateTo(y, "alloc.b", cnxapi.TargetExportLot, "lot.d04b")
	s.want("lot_b", status, "200")
	r, e = s.evaluate(fulapi.ScopeShipment)
	bound := brcStr(brcDig(e, "bindings", "export_license", "allocation_id"))
	if r["status"] != fulapi.ReleaseGranted || !strings.Contains(bound, "alloc.a") || !strings.Contains(bound, "alloc.b") {
		t.Fatalf("both lots: status %v binding %v evaluation %v", r["status"], bound, e)
	}
	s.figure("release", fulapi.ReleaseGranted+" "+bound)
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": "shipment.d04",
		"expected_version": ship["row_version"], "evidence_ids": []string{"receipt.delivery.d04"}})
	confirmed := s.get(brcMerchandiser, "/api/brant/v1/ful/shipments/shipment.d04")
	s.want("confirmed", confirmed["status"], fulapi.StatusConfirmed)
	mode := s.get(brcDocumentation, "/api/brant/v1/cnx/export_licenses/"+y)
	s.want("usage_mode_default", mode["usage_mode"], cnxapi.UsageSingleUse)
}

// dispose has the purchaser request a write-off of quantity of the deal's line and the order reviewer approve it.
func (s *s5sc) dispose(d *s5scDeal, id, quantity string) {
	s.t.Helper()
	s.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandDisposeCommitment, map[string]any{"disposition_id": id,
		"commitment_id": d.commitment, "order_line_id": "L1", "disposition": purapi.DispositionWriteOff, "quantity": quantity,
		"reason": "已交付未出运的货物转作核销（合成，S5R-D05）"})
	s.cmd(brcOrderReviewer, "pur", purapi.Contract, purapi.CommandDecideDisposition, map[string]any{"disposition_id": id,
		"expected_version": 1, "decision": purapi.DecisionApprove})
}

// S5R-D05 (PUR-INV-013, TRD-INV-019, FUL-INV-003): an order whose delivered goods are written off
// cancels, and a shipment that still needs those goods is refused. On a second order the shipment
// confirms first: a write-off of 50 of the 40 left (100 verified, 60 shipped) is refused, a write-off
// of 40 is taken and approved, and nothing further ships (R82, CommitmentShipped v1).
func TestS5R12D05DispositionAndTheShipmentOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "S5R-D05", Title: "S5R-D05: a write-off lets the order cancel and refuses the shipment; after 60 shipped a write-off of 50 is refused and one of 40 is approved",
		Design:    []string{"S5R-D05", "PUR-INV-013", "TRD-INV-019", "FUL-INV-003", "R59", "R82"},
		RestsOn:   []string{"R4", "R13", "R14"},
		UnitTests: []string{"TestS5RD05DisposedGoodsNeverShipWhileTheOrderIsActive (R13)", "TestS5RD05ConfirmationFirstMakesTheApprovalWait (R14)"},
	})
	s.dealFoundation(s5scUSDTenant("ST01.red.S"))
	cancel := &s5scDeal{id: "can", lines: []s5scLine{{"L1", "ST01.red.S", "40", "8.00", "5.00"}}, consignee: "cons.de", freight: "10.00"}
	s.form(cancel)
	s.clear(cancel, false)
	s.reserve(cancel, "res.can")
	s.activate(cancel)
	s.draft(cancel, nil)
	s.dispatch("after the cancel-deal activation")
	s.commit(cancel, "purchase.can")
	s.inspect(cancel, nil)
	s.deliver(cancel, "delivery.can", [][2]string{{"L1", "40"}}, [][2]string{{"L1", "40"}})

	s.phase = "S5R-D05: cancel waits for the disposition"
	o := s.get(brcSales, "/api/brant/v1/trd/orders/"+cancel.order())
	cancelArgs := map[string]any{"order_id": cancel.order(), "expected_version": o["row_version"], "reason": "客户取消，货物尚未出运（合成）",
		"evidence": []any{map[string]any{"source_id": "receipt.delivery.can", "locator": "进仓单"}}}
	s.refused(brcSales, "trd", trdapi.Contract, trdapi.CommandCancelOrder, cancelArgs, http.StatusConflict, "DOWNSTREAM_COMMITMENT_OPEN")
	s.dispose(cancel, "disp.can", "40")
	status, out := s.prepareOn(cancel, "shipment.can", [][2]string{{"L1", "1"}})
	s.want("disposed_goods_do_not_ship", fmt.Sprint(status, " ", out["code"]), "409 "+fulapi.CodeSupplierDeliveryInsufficient)
	cancelled := s.cmd(brcSales, "trd", trdapi.Contract, trdapi.CommandCancelOrder, cancelArgs)
	s.want("cancelled", cancelled["status"], trdapi.OrderCancelled)

	s.phase = "S5R-D05: confirmation first, then the write-off"
	gap := &s5scDeal{id: "gap", lines: []s5scLine{{"L1", "ST01.red.S", "100", "8.00", "5.00"}}, consignee: "cons.de", freight: "10.00"}
	s.form(gap)
	s.clear(gap, false)
	s.reserve(gap, "res.gap")
	s.activate(gap)
	s.draft(gap, nil)
	s.dispatch("after the gap-deal activation")
	s.fundOf("fund.gap", "USD", s.bank, "240.00", "MT103-GAP")
	s.applyOn("app.gap", "fund.gap", s5scStage(gap.order(), "P1"), "240.00", nil)
	s.commit(gap, "purchase.gap")
	s.inspect(gap, nil)
	s.deliver(gap, "delivery.gap", [][2]string{{"L1", "100"}}, [][2]string{{"L1", "100"}})
	s.ship(gap, "shipment.gap", [][2]string{{"L1", "60"}})
	status, out = s.post(brcPurchaser, "pur", purapi.Contract, purapi.CommandDisposeCommitment, map[string]any{"disposition_id": "disp.gap",
		"commitment_id": gap.commitment, "order_line_id": "L1", "disposition": purapi.DispositionWriteOff, "quantity": "50",
		"reason": "已交付未出运的货物转作核销（合成，S5R-D05）"})
	reason := brcStr(brcDig(out, "field_errors", 0, "reason"))
	s.want("request_after_confirmation", fmt.Sprint(status, " ", out["code"], " ", brcDig(out, "field_errors", 0, "path")),
		"422 "+kernel.CodeInvalidKnownValue+" /args/quantity")
	if !strings.Contains(reason, "100") || !strings.Contains(reason, "60") || !strings.Contains(reason, "shipped") {
		t.Fatalf("the refusal names verified and shipped: %s", reason)
	}
	s.dispose(gap, "disp.fit", "40")
	s.figure("approval_after_confirmation", "approved 40 after 60 shipped")
	status, out = s.prepareOn(gap, "shipment.gap2", [][2]string{{"L1", "1"}})
	reason = brcStr(brcDig(out, "field_errors", 0, "reason"))
	if status != http.StatusUnprocessableEntity && status != http.StatusConflict || out["code"] != fulapi.CodeSupplierDeliveryInsufficient || !strings.Contains(reason, "60") {
		t.Fatalf("after the approval %d %v", status, out)
	}
	s.figure("later_shipment", fmt.Sprint(status, " ", out["code"]))
}

// importProgrammeVersion has compliance import the next version of a synthetic country programme list.
func (s *s5sc) importProgrammeVersion(code, version, published string) {
	s.t.Helper()
	f := syntheticCountryProgrammes(code, version, published)
	var programmes []any
	for _, p := range f.Programmes {
		programmes = append(programmes, map[string]any{"programme_id": p.ProgrammeID, "name": p.Name, "measure": p.Measure,
			"countries": p.Countries, "scope": p.Scope})
	}
	s.cmd(brcCompliance, "ref", refapi.Contract, "country_programme.import", map[string]any{"list_code": f.ListCode, "jurisdiction": f.Jurisdiction,
		"version": f.Version, "source_name": f.SourceName, "source_url": f.SourceURL, "published_on": f.PublishedOn, "file_sha256": f.FileSHA256,
		"programme_count": f.ProgrammeCount, "country_count": f.CountryCount, "programmes": programmes})
}

// S5R-D10 (FUL-INV-002, TradeComplianceGuard v1): the documents release of a confirmed shipment is
// refused while a blocked conclusion is under re-evaluation, and recorded once compliance concludes clear.
func TestS5R12D10DocumentsReleaseUnderReviewOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "S5R-D10", Title: "S5R-D10: documents release stays refused while a blocked conclusion is under review",
		Design:    []string{"S5R-D10", "FUL-INV-002", "SM-CNX-COMPLIANCE", "R56"},
		RestsOn:   []string{"R9", "R6", "D1"},
		UnitTests: []string{"TestS5RD10DocumentsReleaseRefusesABlockedConclusionUnderReview (R6)", "TestS5RD10BlockedUnderReviewStaysBlocked (R9)"}})
	s.toS5()
	s.commitment()
	s.quality()
	s.milestones()
	s.delivery()
	s.shipment()
	s.shipmentRelease()
	s.confirm()
	s.documents()

	s.phase = "S5R-D10: blocked, then under review"
	s.importProgrammeVersion(cnProgrammeList, "v2", "2026-11-10")
	list := s.get(brcCompliance, "/api/brant/v1/cnx/orders/order.1/trade_compliance")
	var check map[string]any
	for _, c := range brcList(list["checks"]) {
		if brcNum(brcDig(c, "order_revision")) == 1 {
			check = brcMap(c)
		}
	}
	if check == nil {
		t.Fatalf("no check %v", list)
	}
	id := brcStr(check["check_id"])
	reopened := s.cmd(brcCompliance, "cnx", cnxapi.Contract, cnxapi.CommandReopen, map[string]any{"cause": cnxapi.CauseProgrammeUpdated,
		"check_id": id, "expected_version": check["row_version"], "reason": "合成国别项目名单新版本"})
	evidence := []string{"evidence"}
	evaluate := func(version any, country string) map[string]any {
		return s.cmd(brcCompliance, "cnx", cnxapi.Contract, cnxapi.CommandEvaluate, map[string]any{"order_id": "order.1", "order_revision": 1,
			"expected_version": version, "regulation_id": "screening", "regulation_revision": 1,
			"programme_lists": []string{cnProgrammeList, unProgrammeList}, "code_table_version": codeTable,
			"destination":       map[string]any{"country": country, "reason": "客户 PO 载明的卸货国（合成）", "evidence_ids": evidence},
			"address_countries": []any{map[string]any{"party_id": "customer.1", "address_id": "hq", "country": country, "evidence_ids": evidence}},
			"end_use":           map[string]any{"statement": "服装零售（合成）", "evidence_ids": evidence}})
	}
	evaluated := evaluate(reopened["row_version"], syntheticEmbargoed)
	view := s.get(brcCompliance, "/api/brant/v1/cnx/trade_compliance_checks/"+id)
	ev := brcMap(brcList(view["evaluations"])[len(brcList(view["evaluations"]))-1])
	blocked := s.cmd(brcCompliance, "cnx", cnxapi.Contract, cnxapi.CommandDecide, map[string]any{"check_id": id,
		"expected_version": evaluated["row_version"], "evaluation_id": ev["evaluation_id"], "conclusion": cnxapi.ConclusionBlocked,
		"reason": "目的国在合成禁运名单内（测试）", "evidence_ids": evidence})
	s.want("blocked", blocked["status"], cnxapi.StatusBlocked)
	pending := evaluate(blocked["row_version"], syntheticClear)
	if pending["status"] != cnxapi.StatusPending {
		t.Fatalf("re-evaluation %v", pending)
	}
	s.shipmentID = "shipment.1"
	r, e := s.evaluate(fulapi.ScopeDocuments)
	reason := brcStr(brcDig(e, "bindings", "rechecked", "trade_compliance", 0, "reason"))
	if r["status"] != fulapi.ReleaseBlocked || e["code"] != fulapi.CodeTradeComplianceNotClear || reason != cnxapi.ReasonBlockedUnderReview {
		t.Fatalf("documents release under review: status %v code %v reason %q evaluation %v", r["status"], e["code"], reason, e)
	}
	s.figure("documents_under_review", fmt.Sprint(r["status"], " ", e["code"], " ", reason))
	s.source(brcDocumentation, "bl.release.d10", "合成正本提单放单记录：三份正本提单交买方指定货代 provider.1（FCA）。")
	s.refused(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandRecordDocumentRelease, map[string]any{"shipment_id": "shipment.1",
		"expected_version": r["row_version"], "release_kind": fulapi.ReleaseOriginalBL, "evidence_ids": []string{"bl.release.d10"}},
		http.StatusConflict, fulapi.CodeTradeComplianceNotClear)

	s.phase = "S5R-D10: clear, then the original B/L is released"
	view = s.get(brcCompliance, "/api/brant/v1/cnx/trade_compliance_checks/"+id)
	ev = brcMap(brcList(view["evaluations"])[len(brcList(view["evaluations"]))-1])
	cleared := s.cmd(brcCompliance, "cnx", cnxapi.Contract, cnxapi.CommandDecide, map[string]any{"check_id": id,
		"expected_version": pending["row_version"], "evaluation_id": ev["evaluation_id"], "conclusion": cnxapi.ConclusionClear,
		"reason": "复评后目的国不在禁运名单（合成）", "license_required": false, "evidence_ids": evidence})
	s.want("cleared", cleared["status"], cnxapi.StatusClear)
	r, _ = s.evaluate(fulapi.ScopeDocuments)
	s.want("documents_granted", r["status"], fulapi.ReleaseGranted)
	recorded := s.cmd(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandRecordDocumentRelease, map[string]any{"shipment_id": "shipment.1",
		"expected_version": r["row_version"], "release_kind": fulapi.ReleaseOriginalBL, "evidence_ids": []string{"bl.release.d10"}})
	s.want("original_bl", recorded["status"], fulapi.ReleaseConsumed)
}
