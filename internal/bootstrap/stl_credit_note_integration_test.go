//go:build integration

package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Credit notes, settlement reversal and a cross-currency application on TRD's, PUR's, QLT's and FUL's real
// records (S5 unit C8, WBS S5-18; STL-INV-007, STL-INV-008, STL-INV-009; CN-AT11 STL half; baseline 4.5;
// Context Map R30, R32 (TRD's analysis rate), R33; S5 plan D-4) on PostgreSQL 16, in B2b's statement world
// (BRC-001: 120 PCS at 8.00 = 960.00 USD; P1 288.00 on order confirmation, P2 672.00 30 days after
// shipment; a consumed credit reservation of 672.00). clerk.c8 is a sales clerk; manager.1 the credit
// manager; treasury.1 the treasurer who confirms statements and applies.

func (w *statementWorld) creditNoteCmd(id, statement, amount string) stlapi.RequestCreditNote {
	return stlapi.RequestCreditNote{IdempotencyKey: w.key(), CreditNoteID: id, CreditKind: stlapi.CreditPriceAllowance, OrderID: w.order.ObjectID,
		StatementID: statement, Amount: amount, Basis: "合成：到港后客户投诉色差，协商价格折让", EvidenceIDs: []string{"evidence"}}
}

func (w *statementWorld) receivableApps(statement string) map[string]stlapi.AllocationView {
	w.t.Helper()
	r, err := w.c.STL.GetReceivable(w.treasury, statement)
	w.must(err)
	out := map[string]stlapi.AllocationView{}
	for _, a := range r.Applications {
		out[a.ApplicationID] = a
	}
	return out
}

// STL-INV-008 and STL-INV-009 on BRC-001's real records:
//
//   - The 288.00 prepayment moved to the receivable of statement.sales (960.00, open 672.00); the exposure is
//     672.00. The sales clerk requests a price allowance of 100.00; the credit manager confirms it; applied
//     to P2's installment it leaves 572.00 open and the exposure follows: 572.00 (STL-INV-004 stays right).
//     Net sales are 960.00 − 100.00 = 860.00; FUL's shipment line still ships 120 PCS: the credit note wrote
//     no quantity, and STL called no other context (no refund, no tax-refund material).
//   - The credit manager's reversal is SETTLEMENT_HAS_APPLICATIONS while P1's moved prepayment and the
//     credit note stand on the receivable. Unapplied — the 288.00 is back on the receipt, not on the stage —
//     the statement is reversed: the credit note no longer counts, the exposure is the reservation's 672.00
//     again, and P1 reads unpaid (PaymentConditionGuard v1) until the treasurer applies the receipt to it
//     again.
//   - The shipment line settles again on a new statement — once (SETTLEMENT_SOURCE_DUPLICATE): the
//     prepayment moves to it and the exposure is 672.00. A purchase statement reverses the same way and its
//     verified delivery line settles again.
func TestCreditNoteAndReversalOnRealRecordsBRC001(t *testing.T) {
	w := newStatementWorld(t)
	order := w.order.ObjectID
	clerk := w.assign("clerk.c8", "sales_clerk")
	w.prepaid()
	w.shipped("shipment.1", "120", w.verified("delivery.1", "120", "120"))
	_, err := w.c.STL.ConfirmSettlement(w.treasury, salesCmd(w.key(), "statement.sales", "shipment.1"))
	w.must(err)
	if got := w.exposure(); got != "USD 67200" {
		t.Fatalf("exposure after the statement: %s", got)
	}

	r, err := w.c.STL.RequestCreditNote(clerk, w.creditNoteCmd("cn.brc", "statement.sales", "100.00"))
	w.must(err)
	if r.Status != stlapi.CreditRequested || r.StatementVersion != 2 || r.OrderID != order {
		t.Fatalf("requested %+v", r)
	}
	c, err := w.c.STL.ConfirmCredit(w.manager, stlapi.ConfirmCredit{IdempotencyKey: w.key(), CreditNoteID: "cn.brc", ExpectedVersion: 1,
		Decision: stlapi.DecisionConfirm, Reason: "验货报告确认色差，核准折让"})
	w.must(err)
	if c.Status != stlapi.CreditConfirmed || c.StatementVersion != 3 {
		t.Fatalf("confirmed %+v", c)
	}
	a, err := w.c.STL.Apply(w.treasury, stlapi.Apply{IdempotencyKey: w.key(), ApplicationID: "app.cn.brc", CreditNoteID: "cn.brc",
		Target: toStatement(stlapi.TargetReceivable, "statement.sales", "P2"), Amount: "100.00"})
	w.must(err)
	if a.StatementOpenMinor != "57200" || a.StageID != "P2" {
		t.Fatalf("credit note applied %+v", a)
	}
	if got := w.exposure(); got != "USD 57200" {
		t.Fatalf("exposure after the credit note: %s", got)
	}
	sh, err := w.c.FUL.GetShipment(w.merch, "shipment.1")
	w.must(err)
	pos, err := w.c.STL.GetOrderSettlement(w.treasury, order)
	w.must(err)
	if len(sh.Lines) != 1 || sh.Lines[0].Quantity != "120" || pos.SettledMinor != "96000" || pos.CreditedMinor != "10000" ||
		pos.NetSalesMinor != "86000" || pos.OpenMinor != "57200" {
		t.Fatalf("quantities %+v, position %+v", sh.Lines, pos)
	}

	_, err = w.c.STL.ReverseSettlement(w.manager, stlapi.ReverseSettlement{IdempotencyKey: w.key(), StatementID: "statement.sales", ExpectedVersion: 3,
		Reason: "结算单价格错误，冲销后重新结算"})
	wantRefused(t, "a reversal with applications standing", err, stlapi.CodeSettlementHasApplications, "/args/statement_id")
	var replacement string
	for id, x := range w.receivableApps("statement.sales") {
		if x.MovedFrom == "app.p1" {
			replacement = id
		}
	}
	for _, id := range []string{replacement, "app.cn.brc"} {
		_, err = w.c.STL.Unapply(w.treasury, stlapi.Unapply{IdempotencyKey: w.key(), ApplicationID: id, ExpectedVersion: 1, Reason: "冲销结算单前撤销核销"})
		w.must(err)
	}
	rv, err := w.c.STL.ReverseSettlement(w.manager, stlapi.ReverseSettlement{IdempotencyKey: w.key(), StatementID: "statement.sales", ExpectedVersion: 3,
		Reason: "结算单价格错误，冲销后重新结算", EvidenceIDs: []string{"evidence"}})
	w.must(err)
	if rv.Status != stlapi.StatementReversed || rv.RowVersion != 4 || len(rv.Sources) != 1 || rv.Sources[0].ShipmentID != "shipment.1" {
		t.Fatalf("reversal %+v", rv)
	}
	if got := w.exposure(); got != "USD 67200" {
		t.Fatalf("exposure after the reversal: %s", got)
	}
	n, err := w.c.STL.GetCreditNote(w.manager, "cn.brc")
	w.must(err)
	ref, err := w.c.STL.ResolveFundRecords(w.treasury, []string{"fund.p1"})
	w.must(err)
	if n.Effective || n.StatementStatus != stlapi.StatementReversed || n.RestMinor != "10000" || ref["fund.p1"].AvailableMinor != "28800" ||
		len(n.Sources) != 1 || n.Sources[0] != (stlapi.SettlementSourceRef{ShipmentID: "shipment.1", LineID: "S1"}) {
		t.Fatalf("credit note %+v, receipt %+v", n, ref["fund.p1"])
	}
	if v := w.paymentCondition(order, w.revision, stlapi.ScopeShipment); v.Satisfied() || v.StageID != "P1" {
		t.Fatalf("P1 after the reversal: %+v", v)
	}
	pos, err = w.c.STL.GetOrderSettlement(w.treasury, order)
	w.must(err)
	if pos.SettledMinor != "0" || pos.NetSalesMinor != "0" || pos.CreditedMinor != "0" || pos.OpenMinor != "0" || len(pos.Receivables) != 1 {
		t.Fatalf("position after the reversal %+v", pos)
	}

	_, err = w.c.STL.Apply(w.treasury, stlapi.Apply{IdempotencyKey: w.key(), ApplicationID: "app.p1.again", FundRecordID: "fund.p1",
		Target: stlapi.ApplicationTarget{Kind: stlapi.TargetOrderStage, OrderID: order, StageID: "P1"}, Amount: "288.00"})
	w.must(err)
	again, err := w.c.STL.ConfirmSettlement(w.treasury, salesCmd(w.key(), "statement.sales2", "shipment.1"))
	w.must(err)
	if again.AmountMinor != "96000" || again.OpenMinor != "67200" || len(again.MovedApplications) != 1 || again.MovedApplications[0] != "app.p1.again" {
		t.Fatalf("settled again %+v", again)
	}
	_, err = w.c.STL.ConfirmSettlement(w.treasury2, salesCmd(w.key(), "statement.sales3", "shipment.1"))
	wantRefused(t, "the shipment line settled a third time", err, stlapi.CodeSettlementSourceDuplicate, "/args/sources/0")
	if got := w.exposure(); got != "USD 67200" {
		t.Fatalf("exposure after the new statement: %s", got)
	}
	if n := w.count("company.A", "brant_stl.settlement_line WHERE status = 'settled'"); n != 1 {
		t.Fatalf("settled lines %d", n)
	}
	if v := w.paymentCondition(order, w.revision, stlapi.ScopeShipment); !v.Satisfied() {
		t.Fatalf("P1 paid again: %+v", v)
	}

	// A purchase statement is reversed the same way (by a person other than its confirmer, nothing applied):
	// the verified delivery line settles again, and the order's position lists the new one's line only.
	_, err = w.c.STL.ConfirmSettlement(w.treasury, purchaseCmd(w.key(), "purchase.wrong", "delivery.1"))
	w.must(err)
	if _, err = w.c.STL.ReverseSettlement(w.manager, stlapi.ReverseSettlement{IdempotencyKey: w.key(), StatementID: "purchase.wrong",
		ExpectedVersion: 1, Reason: "供应商账单单价错误"}); err != nil {
		t.Fatal(err)
	}
	_, err = w.c.STL.ConfirmSettlement(w.treasury, purchaseCmd(w.key(), "purchase.right", "delivery.1"))
	w.must(err)
	pos, err = w.c.STL.GetOrderSettlement(w.treasury, order)
	w.must(err)
	if len(pos.PurchaseLines) != 1 || pos.PurchaseLines[0].StatementID != "purchase.right" || len(pos.Payables) != 2 {
		t.Fatalf("purchase position after the reversal %+v", pos)
	}
}

// S5-18 over HTTP with OIDC access tokens on the composed process: the sales clerk requests a credit note;
// an AI agent holding credit.confirm and settlement.reverse is refused both (humanOnlyCommands); the credit
// manager confirms it (and reconciles); the treasurer applies it; credit_note.get shows it applied. The
// reversal is 409 SETTLEMENT_HAS_APPLICATIONS. A CNY receipt is applied to the USD receivable with a POL
// settlement rate (STL-INV-007): the USD it gives is CNY × rate HALF_UP; TRD analyses this order in USD, so
// the business settlement FX difference is 0; the order's position lists it and nets the credit note.
func TestCreditNotesReversalAndCrossCurrencyOverHTTP(t *testing.T) {
	w := newStatementWorld(t)
	order := w.order.ObjectID
	w.assign("clerk.c8", "sales_clerk")
	w.prepaid()
	w.shipped("shipment.1", "120", w.verified("delivery.1", "120", "120"))
	_, err := w.c.STL.ConfirmSettlement(w.treasury, salesCmd(w.key(), "statement.http", "shipment.1"))
	w.must(err)
	w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.credit"},
		Permissions: []string{"credit.confirm", "settlement.reverse", "stl.read"}, DataScope: "company"})
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	clerk := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "clerk.c8", "human"), n: 300}
	manager := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "manager.1", "human"), n: 400}
	treasurer := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "treasury.1", "human"), n: 500}
	agent := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "agent.credit", "ai"), n: 600}

	if r := clerk.command("stl", stlapi.Contract, stlapi.CommandRequestCreditNote, map[string]any{"credit_note_id": "cn.http",
		"kind": "price_allowance", "order_id": order, "statement_id": "statement.http", "amount": "100.00", "basis": "合成：色差折让",
		"evidence_ids": []any{"evidence"}}); r["status"] != "requested" || r["object_id"] != "cn.http" {
		t.Fatalf("request %v", r)
	}
	confirm := map[string]any{"credit_note_id": "cn.http", "expected_version": 1, "decision": "confirm", "reason": "核准折让"}
	if status, out := agent.try("stl", stlapi.Contract, stlapi.CommandConfirmCredit, confirm); status != http.StatusForbidden ||
		out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("AI confirms: %d %v", status, out)
	}
	envelope := map[string]any{"contract": stlapi.Contract, "command": stlapi.CommandConfirmCredit, "idempotency_key": "stl.http.confirm",
		"args": confirm}
	status, res := manager.do("POST", "/api/brant/v1/stl/commands/credit.confirm", envelope)
	if status != http.StatusOK || res["status"] != "confirmed" || res["row_version"] != float64(2) {
		t.Fatalf("confirm %d %v", status, res)
	}
	if status, rec := manager.do("POST", "/api/brant/v1/stl/commands/credit.confirm/reconcile", envelope); status != http.StatusOK ||
		rec["state"] != "committed" || rec["action_id"] != res["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	if r := treasurer.command("stl", stlapi.Contract, stlapi.CommandApply, map[string]any{"application_id": "app.cn.http", "credit_note_id": "cn.http",
		"target": map[string]any{"kind": "receivable", "statement_id": "statement.http", "stage_id": "P2"}, "amount": "100.00"}); r["status"] != "applied" {
		t.Fatalf("credit note applied %v", r)
	}
	status, cn := manager.do("GET", "/api/brant/v1/stl/credit_notes/cn.http", nil)
	if status != http.StatusOK || cn["status"] != "confirmed" || cn["kind"] != "price_allowance" || cn["applied_minor"] != "10000" ||
		cn["rest_minor"] != "0" || cn["effective"] != true || cn["statement_id"] != "statement.http" {
		t.Fatalf("credit note %d %v", status, cn)
	}
	reverse := map[string]any{"statement_id": "statement.http", "expected_version": 3, "reason": "冲销"}
	if status, out := agent.try("stl", stlapi.Contract, stlapi.CommandReverseSettlement, reverse); status != http.StatusForbidden {
		t.Fatalf("AI reverses: %d %v", status, out)
	}
	if status, out := manager.try("stl", stlapi.Contract, stlapi.CommandReverseSettlement, reverse); status != http.StatusConflict ||
		out["code"] != stlapi.CodeSettlementHasApplications {
		t.Fatalf("reversal with applications: %d %v", status, out)
	}

	cny := polapi.ConfigRef{Kind: polapi.KindCurrency, ConfigID: "CNY"}
	if v, err := w.c.POL.GetConfig(w.author, polapi.KindCurrency, "CNY"); err == nil && v.ActiveRevision > 0 {
		cny.Revision = v.ActiveRevision
	} else {
		cny = w.currency(w.author, w.reviewer, "CNY", w.win)
	}
	bankCNY := w.configure(w.author, w.reviewer, polapi.KindBankAccount, "bank.cny", 0, w.win.from, time.Time{},
		bank(polapi.OwnerCompany, "company.A", "CNY", polapi.AccountReceipt), true)
	win := newWindow()
	rate := w.configure(w.author, w.reviewer, polapi.KindFXRate, "cny.usd.settlement", 0, win.from, win.until, polapi.ConfigData{FXRate: &polapi.FXRate{
		FromCurrency: "CNY", ToCurrency: "USD", Rate: "0.14", Purpose: polapi.PurposeSettlement, SourceName: "SYNTHETIC SETTLEMENT RATE",
		ObservedAt: kernel.FormatTimestamp(win.from)}}, true)
	_, err = w.c.STL.RecordFund(w.treasury, stlapi.RecordFund{IdempotencyKey: w.key(), FundRecordID: "fund.cny", FundKind: stlapi.FundReceipt,
		CounterpartyID: "customer.1", Currency: stlRef(cny), BankAccount: stlRef(bankCNY), ReceivedAmount: "1000.05",
		ValueDate: kernel.FormatTimestamp(time.Now()), BankReference: "cny.http", EvidenceIDs: []string{"evidence"}})
	w.must(err)
	toP2 := map[string]any{"kind": "receivable", "statement_id": "statement.http", "stage_id": "P2"}
	if status, out := treasurer.try("stl", stlapi.Contract, stlapi.CommandApply, map[string]any{"application_id": "app.cny.no",
		"fund_record_id": "fund.cny", "target": toP2, "amount": "1000.05"}); status != http.StatusUnprocessableEntity ||
		out["code"] != stlapi.CodeFXScopeMismatch {
		t.Fatalf("no settlement rate: %d %v", status, out)
	}
	treasurer.command("stl", stlapi.Contract, stlapi.CommandApply, map[string]any{"application_id": "app.cny", "fund_record_id": "fund.cny",
		"target": toP2, "amount": "1000.05", "fx_rate": map[string]any{"kind": "fx_rate", "id": rate.ConfigID, "revision": rate.Revision}})
	status, app := treasurer.do("GET", "/api/brant/v1/stl/applications/app.cny", nil)
	cv, _ := app["conversion"].(map[string]any)
	// 1,000.05 × 0.14 = 140.007 → 140.01 USD (HALF_UP); analysed in USD: difference 0.
	if status != http.StatusOK || app["amount_minor"] != "14001" || app["currency"] != "USD" || cv == nil || cv["source_currency"] != "CNY" ||
		cv["source_amount_minor"] != "100005" || cv["rate"] != "0.14" || cv["analysis_currency"] != "USD" || cv["fx_difference_minor"] != "0" {
		t.Fatalf("cross-currency application %d %v", status, app)
	}
	status, pos := treasurer.do("GET", "/api/brant/v1/stl/orders/"+order+"/settlement", nil)
	fx, _ := pos["fx_differences"].([]any)
	if status != http.StatusOK || pos["credited_minor"] != "10000" || pos["net_sales_minor"] != "86000" || pos["open_minor"] != "43199" || len(fx) != 1 {
		t.Fatalf("settlement position %d %v", status, pos)
	}
}
