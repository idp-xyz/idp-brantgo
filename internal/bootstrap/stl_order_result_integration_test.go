//go:build integration

package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The order result on TRD's, PUR's, QLT's and FUL's real records, over HTTP (S5 unit C9, WBS S5-17; S5 plan §5 D1
// steps 25–27; STL-INV-010, STL-INV-011; baseline 4.5, BRC-001 figures 960.00 − 600.00 − 60.00 = 300.00, 31.25 %).
// The statement world (stl_settlement_integration_test.go) is BRC-001's composed process; STL's ports read TRD's
// OrderCommercialSnapshot v1 (net sales, end, analysis rate in USD), FUL's shipment lines and PUR's commitments and
// verified deliveries. Python V0.50 has no STL.

// statementSettled runs BRC-001 steps 7 and 19–21 on the statement world (B2b's real-world test): 288.00 prepaid,
// 120 PCS shipped from the verified delivery, the sales statement of 960.00, 670.00 received for the 672.00 open
// with the 2.00 bank charge resolved, the purchase statement of 600.00 paid.
func (w *statementWorld) statementSettled() {
	w.t.Helper()
	w.prepaid()
	w.shipped("shipment.1", "120", w.verified("delivery.1", "120", "120"))
	_, err := w.c.STL.ConfirmSettlement(w.treasury, salesCmd(w.key(), "statement.sales", "shipment.1"))
	w.must(err)
	w.fund("fund.670", stlapi.FundReceipt, "670.00", "672.00")
	_, err = w.c.STL.ResolveFundDifference(w.manager, stlapi.ResolveFundDifference{IdempotencyKey: w.key(), FundRecordID: "fund.670",
		ExpectedVersion: 1, Resolution: stlapi.ResolutionBankCharge, Amount: "2.00", EvidenceIDs: []string{"evidence"}, Reason: "中间行扣费，经批准由我方承担"})
	w.must(err)
	_, err = w.apply("app.672", "fund.670", toStatement(stlapi.TargetReceivable, "statement.sales", ""), "672.00")
	w.must(err)
	_, err = w.c.STL.ConfirmSettlement(w.treasury, purchaseCmd(w.key(), "statement.purchase", "delivery.1"))
	w.must(err)
	w.fund("fund.pay", stlapi.FundPayment, "600.00", "")
	_, err = w.apply("app.pay", "fund.pay", toStatement(stlapi.TargetPayable, "statement.purchase", ""), "600.00")
	w.must(err)
}

func figure(v map[string]any, path ...string) any {
	var x any = v
	for _, p := range path {
		m, _ := x.(map[string]any)
		x = m[p]
	}
	return x
}

// BRC-001 steps 25–27 over HTTP with OIDC access tokens (S5 plan §5; D1 runs this chain end to end):
//
//   - After statements, receipts and payment, the treasurer reviews the result; closing it while TRD's order is
//     still active is 409 ORDER_RESULT_NOT_SETTLED naming /conditions/fulfilment only (everything else is met).
//   - The merchandiser closes the order in TRD (120 = 120 shipped, order.close).
//   - The credit officer records the freight of 60.00 USD (estimated, the forwarder's offer): closing now is 409
//     again, /conditions/direct_charges (BT18). The treasurer confirms it with the forwarder's bill, the credit
//     officer allocates it to the order (one beneficiary); the AI agent's confirmation and close are 403; the
//     treasurer closes the result (and reconciles the command).
//   - GET /stl/orders/{id}/result: settled; confirmed result 960.00 − 600.00 − 60.00 = 300.00 USD, margin 31.25
//     (equal to the quote's calculation); the forecast too (PUR's commitment of 600.00, the 60.00 estimate); claims
//     not_enabled; one snapshot. GET /stl/charges/{id} shows the charge and its allocation.
func TestOrderResultOnRealRecordsBRC001OverHTTP(t *testing.T) {
	w := newStatementWorld(t)
	order := w.order.ObjectID
	w.assign("officer.1", "credit_officer")
	w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.c9"},
		Permissions: []string{"charge.record", "charge.confirm", "charge.allocate", "order_result.review", "order_result.close", "stl.read",
			"trd.read", "ful.read", "pur.read", "pol.read", "pty.read", "evd.read"}, DataScope: "company"})
	w.statementSettled()
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	officer := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "officer.1", "human"), n: 300}
	treasurer := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "treasury.1", "human"), n: 400}
	agent := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "agent.c9", "ai"), n: 500}

	if r := treasurer.command("stl", stlapi.Contract, stlapi.CommandReviewOrderResult, map[string]any{"order_id": order, "expected_version": 0,
		"note": "出运、结算、收付款已完成，复核"}); r["status"] != "reviewed" || r["row_version"] != float64(1) {
		t.Fatalf("review %v", r)
	}
	status, out := treasurer.try("stl", stlapi.Contract, stlapi.CommandCloseOrderResult, map[string]any{"order_id": order, "expected_version": 1})
	fields, _ := out["field_errors"].([]any)
	if status != http.StatusConflict || out["code"] != stlapi.CodeOrderResultNotSettled || len(fields) != 1 ||
		figure(fields[0].(map[string]any), "path") != "/conditions/fulfilment" {
		t.Fatalf("close while the order is active: %d %v", status, out)
	}
	if _, err = w.c.TRD.CloseOrder(w.merch, trdapi.CloseOrder{IdempotencyKey: w.key(), OrderID: order, ExpectedVersion: w.orderVersion(order)}); err != nil {
		t.Fatalf("TRD order.close: %v", err)
	}

	today := time.Now().UTC().Format("2006-01-02")
	if r := officer.command("stl", stlapi.Contract, stlapi.CommandRecordCharge, map[string]any{"charge_id": "freight.brc", "expected_version": 0,
		"status": "estimated", "source": map[string]any{"kind": "logistics_offer", "party_id": "provider.1", "reference": "LOG-BRC-001"},
		"beneficiaries": []any{map[string]any{"kind": "order", "order_id": order}},
		"fee_category":  map[string]any{"kind": "fee_category", "id": w.freight.ConfigID, "revision": w.freight.Revision},
		"currency":      map[string]any{"kind": "currency", "id": w.usd.ConfigID, "revision": w.usd.Revision},
		"conversion":    map[string]any{"kind": "same_currency"}, "amount": "60.00", "occurred_on": today, "analysis_on": today,
		"basis": "货代报价：FCA 交货前运杂费"}); r["status"] != "estimated" || r["row_version"] != float64(1) {
		t.Fatalf("charge.record %v", r)
	}
	status, out = treasurer.try("stl", stlapi.Contract, stlapi.CommandCloseOrderResult, map[string]any{"order_id": order, "expected_version": 1})
	if fields, _ = out["field_errors"].([]any); status != http.StatusConflict || len(fields) != 1 ||
		!strings.Contains(figure(fields[0].(map[string]any), "reason").(string), "charge freight.brc is estimated") {
		t.Fatalf("close with the freight estimated (BT18): %d %v", status, out)
	}
	confirm := map[string]any{"charge_id": "freight.brc", "expected_version": 1, "evidence_ids": []any{"evidence"}, "reason": "货代账单核对一致"}
	if status, out := agent.try("stl", stlapi.Contract, stlapi.CommandConfirmCharge, confirm); status != http.StatusForbidden ||
		out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("AI confirms: %d %v", status, out)
	}
	if r := treasurer.command("stl", stlapi.Contract, stlapi.CommandConfirmCharge, confirm); r["status"] != "confirmed" || r["row_version"] != float64(2) {
		t.Fatalf("charge.confirm %v", r)
	}
	if r := officer.command("stl", stlapi.Contract, stlapi.CommandAllocateCharge, map[string]any{"charge_id": "freight.brc", "expected_version": 2,
		"basis": "amount", "data_basis": "单一受益订单", "lines": []any{map[string]any{"line_id": "L1",
			"target": map[string]any{"kind": "order", "order_id": order}, "weight": "1"}}}); r["row_version"] != float64(3) {
		t.Fatalf("charge.allocate %v", r)
	}
	closeArgs := map[string]any{"order_id": order, "expected_version": 1, "reason": "BRC-001 成本结算完成"}
	if status, out := agent.try("stl", stlapi.Contract, stlapi.CommandCloseOrderResult, closeArgs); status != http.StatusForbidden {
		t.Fatalf("AI closes: %d %v", status, out)
	}
	envelope := map[string]any{"contract": stlapi.Contract, "command": stlapi.CommandCloseOrderResult, "idempotency_key": "stl.c9.close",
		"args": closeArgs}
	status, res := treasurer.do("POST", "/api/brant/v1/stl/commands/order_result.close", envelope)
	if status != http.StatusOK || res["status"] != "settled" || res["row_version"] != float64(2) {
		t.Fatalf("close %d %v", status, res)
	}
	if status, rec := treasurer.do("POST", "/api/brant/v1/stl/commands/order_result.close/reconcile", envelope); status != http.StatusOK ||
		rec["state"] != "committed" || rec["action_id"] != res["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}

	status, v := treasurer.do("GET", "/api/brant/v1/stl/orders/"+order+"/result", nil)
	if status != http.StatusOK || v["status"] != "settled" || v["settleable"] != true || v["order_ended"] != "order.close" {
		t.Fatalf("order result %d %v", status, v)
	}
	live := v["live"].(map[string]any)
	for path, want := range map[string]any{
		"currency": "USD", "analysis_rate": "1",
		"confirmed/net_sales_minor": "96000", "confirmed/purchase_cost_minor": "60000", "confirmed/direct_charges_minor": "6000",
		"confirmed/result_minor": "30000", "confirmed/margin": "31.25", "confirmed/order_level_charges_minor": "6000",
		"forecast/net_sales_minor": "96000", "forecast/purchase_cost_minor": "60000", "forecast/direct_charges_minor": "6000",
		"forecast/result_minor": "30000", "forecast/margin": "31.25", "executing/result_minor": "30000",
		"fx_difference/difference_minor": "0", "net_cash/receipts_minor": "96000", "net_cash/payments_minor": "60000",
		"net_cash/net_minor": "36000",
	} {
		if got := figure(live, strings.Split(path, "/")...); got != want {
			t.Fatalf("live %s = %v, want %v (%v)", path, got, want, live)
		}
	}
	batches, _ := figure(live, "confirmed", "batches").([]any)
	snapshots, _ := v["snapshots"].([]any)
	if len(batches) != 1 || figure(batches[0].(map[string]any), "shipment_id") != "shipment.1" ||
		figure(batches[0].(map[string]any), "result_minor") != "36000" || len(snapshots) != 1 ||
		figure(snapshots[0].(map[string]any), "figures", "confirmed", "result_minor") != "30000" {
		t.Fatalf("batches %v snapshots %v", batches, snapshots)
	}
	conditions, _ := v["conditions"].([]any)
	if len(conditions) != 6 || figure(conditions[5].(map[string]any), "dimension") != "claims" ||
		figure(conditions[5].(map[string]any), "status") != "not_enabled" {
		t.Fatalf("conditions %v", conditions)
	}
	status, ch := officer.do("GET", "/api/brant/v1/stl/charges/freight.brc", nil)
	allocations, _ := ch["allocations"].([]any)
	if status != http.StatusOK || ch["status"] != "confirmed" || ch["amount_minor"] != "6000" || ch["estimate_minor"] != "6000" ||
		len(allocations) != 1 || figure(allocations[0].(map[string]any), "current") != true {
		t.Fatalf("charge %d %v", status, ch)
	}
}
