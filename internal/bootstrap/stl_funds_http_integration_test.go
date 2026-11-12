//go:build integration

package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5-04a and the STL half of S5-07 over HTTP with OIDC access tokens, on the composed STL and an
// order TRD activated through the shared order-world helpers (OrderCommercialSnapshot v1, R32):
// BRC-001's 960.00 USD paid 100 % before shipment. The treasurer opens the order's account, records
// the customer's remittance of 960.00 credited 958.00 with 2.00 charges on the company's USD account
// with bank evidence, and is refused applying 960.00 while 2.00 is unexplained
// (FUND_DIFFERENCE_UNEXPLAINED, STL-INV-006); the credit manager resolves the 2.00 as our bank charge
// (D-16), the treasurer applies 960.00; the reads show the account satisfied for shipment and the
// fund's allocation; the application reconciles; PaymentConditionGuard v1, called in another
// context's transaction with its executor (as FUL will, unit C1), is blocked before and satisfied
// after.
func TestFundsOverHTTPWithTRDOrders(t *testing.T) {
	q := newOrderWorld(t)
	o := q.activeOrder("quote.1")
	snap := q.snapshots(q.author, o.ObjectID)[o.ObjectID]
	if len(snap.Stages) != 1 || snap.Stages[0].AmountMinor != "96000" || snap.Stages[0].Trigger != polapi.TriggerBeforeShipment {
		t.Fatalf("TRD's snapshot %+v", snap)
	}
	stageID := snap.Stages[0].StageID
	usdBank := q.configure(q.author, q.reviewer, polapi.KindBankAccount, "bank.usd", 0, q.win.from, time.Time{},
		bank(polapi.OwnerCompany, "company.A", "USD", polapi.AccountBoth), true)
	q.assign("treasury.1", "treasury")
	q.assign("manager.1", "credit_manager")
	q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.funds"},
		Permissions: []string{"application.apply", "stl.read"}, DataScope: "company"})
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	treasurer := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "treasury.1", "human"), n: 300}
	manager := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "manager.1", "human"), n: 400}
	agent := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "agent.funds", "ai"), n: 500}
	ref := func(r polapi.ConfigRef) map[string]any {
		return map[string]any{"kind": r.Kind, "id": r.ConfigID, "revision": r.Revision}
	}
	guard := func() stlapi.PaymentVerdict {
		var v stlapi.PaymentVerdict
		query := stlapi.PaymentQuery{OrderID: o.ObjectID, OrderRevision: snap.Binding.OrderRevision, Scope: stlapi.ScopeShipment, At: time.Now()}
		q.must(q.inTRD(func(ctx context.Context, exec bentopg.Executor) error {
			var err error
			v, err = stlapi.ScanPaymentCondition(query, exec.QueryRow(ctx, stlapi.PaymentConditionSQL, stlapi.PaymentConditionArgs(query)...).Scan)
			return err
		}))
		return v
	}

	r := treasurer.command("stl", stlapi.Contract, stlapi.CommandOpenOrderAccount, map[string]any{"order_id": o.ObjectID,
		"order_revision": snap.Binding.OrderRevision, "order_hash": snap.Binding.OrderHash})
	if r["object_id"] != o.ObjectID || r["status"] != "open" || r["row_version"] != float64(1) {
		t.Fatalf("open %v", r)
	}
	if v := guard(); v.Satisfied() || v.Reason != stlapi.PaymentStageUnpaid || v.ShortfallMinor != "96000" || v.OrderHash != snap.Binding.OrderHash {
		t.Fatalf("PaymentConditionGuard v1 before the money: %+v", v)
	}
	record := map[string]any{"fund_record_id": "fund.http", "kind": "receipt", "counterparty_id": "customer.1", "currency": ref(q.usd),
		"bank_account": ref(usdBank), "received_amount": "958.00", "remitted_amount": "960.00", "bank_charges": "2.00",
		"value_date": kernel.FormatTimestamp(time.Now()), "bank_reference": "MT103-0001", "evidence_ids": []any{"evidence"}}
	if r = treasurer.command("stl", stlapi.Contract, stlapi.CommandRecordFund, record); r["object_id"] != "fund.http" || r["status"] != "recorded" {
		t.Fatalf("fund.record %v", r)
	}
	apply := map[string]any{"contract": stlapi.Contract, "command": stlapi.CommandApply, "idempotency_key": "stl.http.apply",
		"args": map[string]any{"application_id": "app.http", "fund_record_id": "fund.http",
			"target": map[string]any{"kind": "order_stage", "order_id": o.ObjectID, "stage_id": stageID}, "amount": "960.00"}}
	if status, out := agent.do("POST", "/api/brant/v1/stl/commands/application.apply", apply); status != http.StatusForbidden ||
		out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("AI agent: %d %v", status, out)
	}
	if status, out := treasurer.do("POST", "/api/brant/v1/stl/commands/application.apply", apply); status != http.StatusConflict ||
		out["code"] != stlapi.CodeFundDifferenceUnexplained {
		t.Fatalf("2.00 unexplained: %d %v", status, out)
	}
	resolve := map[string]any{"fund_record_id": "fund.http", "expected_version": 1, "resolution": "bank_charge", "amount": "2.00",
		"evidence_ids": []any{"evidence"}, "reason": "中间行扣费，经批准由我方承担"}
	if status, out := treasurer.try("stl", stlapi.Contract, stlapi.CommandResolveDifference, resolve); status != http.StatusForbidden ||
		out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("treasury resolves: %d %v", status, out)
	}
	if r = manager.command("stl", stlapi.Contract, stlapi.CommandResolveDifference, resolve); r["status"] != "difference_resolved" ||
		r["row_version"] != float64(2) {
		t.Fatalf("resolved %v", r)
	}
	apply["idempotency_key"] = "stl.http.apply.2"
	status, res := treasurer.do("POST", "/api/brant/v1/stl/commands/application.apply", apply)
	if status != http.StatusOK || res["object_id"] != "app.http" || res["status"] != "applied" || res["row_version"] != float64(1) {
		t.Fatalf("apply %d %v", status, res)
	}
	status, acc := treasurer.do("GET", "/api/brant/v1/stl/order_accounts/"+o.ObjectID, nil)
	conditions, _ := acc["conditions"].([]any)
	stages, _ := acc["stages"].([]any)
	if status != http.StatusOK || len(conditions) != 2 || conditions[0].(map[string]any)["status"] != "satisfied" || len(stages) != 1 ||
		stages[0].(map[string]any)["applied_minor"] != "96000" || acc["order_revision"] != float64(snap.Binding.OrderRevision) {
		t.Fatalf("order account %d %v", status, acc)
	}
	status, fund := manager.do("GET", "/api/brant/v1/stl/fund_records/fund.http", nil)
	allocations, _ := fund["allocations"].([]any)
	if status != http.StatusOK || fund["kind"] != "receipt" || fund["available_minor"] != "0" || fund["resolved_minor"] != "200" || len(allocations) != 1 {
		t.Fatalf("fund record %d %v", status, fund)
	}
	if status, app := treasurer.do("GET", "/api/brant/v1/stl/applications/app.http", nil); status != http.StatusOK || app["amount_minor"] != "96000" ||
		app["status"] != "applied" {
		t.Fatalf("application %d %v", status, app)
	}
	status, rec := treasurer.do("POST", "/api/brant/v1/stl/commands/application.apply/reconcile", apply)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != res["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	if v := guard(); !v.Satisfied() || v.AppliedMinor != "96000" || v.Check("/args/shipment_id") != nil {
		t.Fatalf("PaymentConditionGuard v1 after the application: %+v", v)
	}
}
