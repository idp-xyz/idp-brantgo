//go:build integration

package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	clmapi "go.idp.xyz/idp-brant/internal/contexts/clm/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S6-03 on PostgreSQL 16 (SM-CLM-CLAIM, CLM-INV-001..003, R34, R35, S6-D03). Python V0.50 has no
// claims context. The shipment is a real FUL confirmation of the order world: CLM reads it and
// does not change it.

func (w *fulWorld) confirmQuantity(id, qty string) {
	w.t.Helper()
	rel := w.released("commitment."+id, qty)
	d := w.delivered("commitment."+id, qty)
	_, err := w.prepare(w.merch, id, qty, d, rel)
	w.must(err)
	w.granted(id)
	_, err = w.confirm(w.merch, id, 1)
	w.must(err)
}

func (w *fulWorld) prepareOnly(id, qty string) {
	w.t.Helper()
	rel := w.released("commitment."+id, qty)
	d := w.delivered("commitment."+id, qty)
	_, err := w.prepare(w.merch, id, qty, d, rel)
	w.must(err)
}

func sameQty(t *testing.T, a, b string) bool {
	t.Helper()
	da, ea := kernel.ParseDecimal(a, kernel.QuantityFormat)
	db, eb := kernel.ParseDecimal(b, kernel.QuantityFormat)
	return ea == nil && eb == nil && da.Equal(db)
}

func fieldPath(v map[string]any) string {
	fs, _ := v["field_errors"].([]any)
	if len(fs) == 0 {
		return ""
	}
	m, _ := fs[0].(map[string]any)
	p, _ := m["path"].(string)
	return p
}

func versionOf(v map[string]any) int {
	return int(v["row_version"].(float64))
}

// The claim lifecycle over HTTP: a confirmed shipment of 100 PCS, two claims, returns of 60 and
// 40, one more unit refused, an independent remedy and close, and the original shipment unchanged.
// An unconfirmed line, a cancelled line and an unknown line are refused. An AI token is refused
// before the body is read. CreditRequest v1 and the order-claims lookup follow the claim.
func TestClaimLifecycleOverHTTP(t *testing.T) {
	w := newFulWorld(t)
	w.confirmQuantity("shipment.100", "100")
	w.prepareOnly("shipment.prep", "10")
	w.prepareOnly("shipment.can", "10")
	_, err := w.ful.CancelShipment(w.merch, fulapi.CancelShipment{IdempotencyKey: w.key(), ShipmentID: "shipment.can", ExpectedVersion: 1, Reason: "客户推迟出运"})
	w.must(err)
	before := w.shipment("shipment.100")
	_ = w.assign("reviewer.1", "order_reviewer")
	// The role templates do not give claim.open and remedy.decide to one job. Independence is what
	// the command checks when one actor holds both (CLM-INV-003).
	_ = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "both.1"), DataScope: authorization.DataScopeCompany,
		Permissions: []string{"claim.open", "remedy.decide", "claim.close", "clm.read", "evd.read"}})

	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	merch := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "merch.1", "human")}
	merch2 := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "merch.2", "human"), n: 80}
	review := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "reviewer.1", "human"), n: 160}
	both := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "both.1", "human"), n: 240}
	ai := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "merch.1", "ai"), n: 320}

	openArgs := map[string]any{"claim_id": "claim.1", "order_id": w.order.ObjectID, "shipment_id": "shipment.100",
		"shipment_line_id": "S1", "reason": "客户投诉色差"}
	if status, out := ai.try("clm", clmapi.Contract, clmapi.CommandOpenClaim, openArgs); status != http.StatusForbidden || out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("AI token: %d %v", status, out)
	}
	opened := merch.command("clm", clmapi.Contract, clmapi.CommandOpenClaim, openArgs)
	if opened["status"] != clmapi.StatusOpen || versionOf(opened) != 1 {
		t.Fatalf("open %+v", opened)
	}
	ret := merch.command("clm", clmapi.Contract, clmapi.CommandRegisterReturn, map[string]any{"claim_id": "claim.1", "return_id": "ret.1",
		"quantity": "60", "condition": "received"})
	if ret["status"] != clmapi.StatusOpen || versionOf(ret) != 2 {
		t.Fatalf("return 60 %+v", ret)
	}
	merch2.command("clm", clmapi.Contract, clmapi.CommandOpenClaim, map[string]any{"claim_id": "claim.2", "order_id": w.order.ObjectID,
		"shipment_id": "shipment.100", "shipment_line_id": "S1", "reason": "同一出运行的第二笔客诉"})
	second := merch2.command("clm", clmapi.Contract, clmapi.CommandRegisterReturn, map[string]any{"claim_id": "claim.2", "return_id": "ret.2",
		"quantity": "40", "condition": "received"})
	if second["status"] != clmapi.StatusOpen || versionOf(second) != 2 {
		t.Fatalf("return 40 %+v", second)
	}
	status, out := merch2.try("clm", clmapi.Contract, clmapi.CommandRegisterReturn, map[string]any{"claim_id": "claim.2", "return_id": "ret.3",
		"quantity": "1", "condition": "received"})
	if status != http.StatusUnprocessableEntity || out["code"] != clmapi.CodeClaimReturnExceedsShipped || fieldPath(out) != "/args/quantity" {
		t.Fatalf("one more unit: %d %v", status, out)
	}

	claims, err := w.c.CLM.ResolveOrderClaims(w.merch, w.order.ObjectID)
	w.must(err)
	if len(claims) != 2 || !claims[0].Open || !claims[1].Open {
		t.Fatalf("both open before close %+v", claims)
	}

	both.command("clm", clmapi.Contract, clmapi.CommandOpenClaim, map[string]any{"claim_id": "claim.sod", "order_id": w.order.ObjectID,
		"shipment_id": "shipment.100", "shipment_line_id": "S1", "reason": "同一人持有开案与补救权限"})
	status, out = both.try("clm", clmapi.Contract, clmapi.CommandDecideRemedy, map[string]any{"claim_id": "claim.sod", "remedy_id": "rem.sod",
		"kind": clmapi.KindCreditNote, "amount_minor": "10000", "reason": "开案人自决", "expected_version": 1})
	if status != http.StatusUnprocessableEntity || out["code"] != clmapi.CodeIndependentReviewRequired || fieldPath(out) != "/args/claim_id" {
		t.Fatalf("self decide: %d %v", status, out)
	}
	decided := review.command("clm", clmapi.Contract, clmapi.CommandDecideRemedy, map[string]any{"claim_id": "claim.1", "remedy_id": "rem.1",
		"kind": clmapi.KindCreditNote, "amount_minor": "10000", "reason": "色差折让", "expected_version": 2})
	if decided["status"] != clmapi.StatusOpen || versionOf(decided) != 3 {
		t.Fatalf("remedy %+v", decided)
	}
	credit, err := w.c.CLM.ResolveCreditRequest(w.merch, "claim.1", "rem.1")
	w.must(err)
	if !credit.Approved || credit.Status != clmapi.StatusOpen || credit.Kind != clmapi.KindCreditNote || credit.Amount.MinorString() != "10000" ||
		credit.Amount.Currency().Code() != "USD" || credit.OrderID != w.order.ObjectID || credit.CustomerID == "" || credit.ShipmentID != "shipment.100" ||
		credit.ShipmentLineID != "S1" {
		t.Fatalf("credit request before close %+v", credit)
	}

	status, out = merch.try("clm", clmapi.Contract, clmapi.CommandCloseClaim, map[string]any{"claim_id": "claim.1", "expected_version": 3,
		"summary": "开案人不能结案"})
	if status != http.StatusUnprocessableEntity || out["code"] != clmapi.CodeIndependentReviewRequired {
		t.Fatalf("self close: %d %v", status, out)
	}
	closed := merch2.command("clm", clmapi.Contract, clmapi.CommandCloseClaim, map[string]any{"claim_id": "claim.1", "expected_version": 3,
		"summary": "折让已决，结案"})
	if closed["status"] != clmapi.StatusClosed || versionOf(closed) != 4 {
		t.Fatalf("close %+v", closed)
	}
	claims, err = w.c.CLM.ResolveOrderClaims(w.merch, w.order.ObjectID)
	w.must(err)
	var closedOne, openTwo, openSod bool
	for _, c := range claims {
		switch c.ClaimID {
		case "claim.1":
			closedOne = !c.Open && c.Status == clmapi.StatusClosed
		case "claim.2":
			openTwo = c.Open && c.Status == clmapi.StatusOpen
		case "claim.sod":
			openSod = c.Open
		}
	}
	if !closedOne || !openTwo || !openSod || len(claims) != 3 {
		t.Fatalf("after close %+v", claims)
	}
	credit, err = w.c.CLM.ResolveCreditRequest(w.merch, "claim.1", "rem.1")
	w.must(err)
	if !credit.Approved || credit.Status != clmapi.StatusClosed {
		t.Fatalf("credit request after close %+v", credit)
	}
	got, err := w.c.CLM.GetClaim(w.merch, "claim.1")
	w.must(err)
	if !sameQty(t, got.ShippedQuantity, "100") || len(got.Returns) != 1 || !sameQty(t, got.Returns[0].Quantity, "60") || got.Remedy == nil ||
		got.Remedy.AmountMinor != "10000" {
		t.Fatalf("claim view %+v", got)
	}

	after := w.shipment("shipment.100")
	if after.Status != fulapi.StatusConfirmed || after.RowVersion != before.RowVersion || len(after.Lines) != 1 ||
		!sameQty(t, after.Lines[0].Quantity, before.Lines[0].Quantity) || !sameQty(t, after.Lines[0].Quantity, "100") {
		t.Fatalf("shipment changed before %+v after %+v", before.Lines, after.Lines)
	}

	for _, c := range []struct {
		name string
		args map[string]any
		code string
		path string
	}{
		{"prepared", map[string]any{"claim_id": "claim.prep", "order_id": w.order.ObjectID, "shipment_id": "shipment.prep", "shipment_line_id": "S1", "reason": "未确认"},
			kernel.CodeInvalidState, "/args/shipment_id"},
		{"cancelled", map[string]any{"claim_id": "claim.can", "order_id": w.order.ObjectID, "shipment_id": "shipment.can", "shipment_line_id": "S1", "reason": "已取消"},
			kernel.CodeInvalidState, "/args/shipment_id"},
		{"unknown", map[string]any{"claim_id": "claim.none", "order_id": w.order.ObjectID, "shipment_id": "shipment.none", "shipment_line_id": "S1", "reason": "无此出运"},
			kernel.CodeReferenceInvalid, "/args/shipment_line_id"},
		{"other order", map[string]any{"claim_id": "claim.order", "order_id": "order.other", "shipment_id": "shipment.100", "shipment_line_id": "S1", "reason": "订单不符"},
			kernel.CodeReferenceInvalid, "/args/order_id"},
	} {
		status, out = merch.try("clm", clmapi.Contract, clmapi.CommandOpenClaim, c.args)
		if status != http.StatusUnprocessableEntity || out["code"] != c.code || fieldPath(out) != c.path {
			t.Fatalf("%s: %d %v", c.name, status, out)
		}
	}

	other := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: actorRef("human", "merch.b"), DataScope: authorization.DataScopeCompany,
		Permissions: []string{"claim.open", "clm.read", "return.register"}})
	_, err = w.c.CLM.GetClaim(other, "claim.1")
	w.want(err, kernel.CodeNotFound)
	foreign, err := w.c.CLM.ResolveOrderClaims(other, w.order.ObjectID)
	w.must(err)
	if len(foreign) != 0 || w.count("company.B", "brant_clm.claim_case") != 0 || w.count("company.A", "brant_clm.claim_case") != 3 {
		t.Fatalf("company isolation foreign %d A %d B %d", len(foreign), w.count("company.A", "brant_clm.claim_case"), w.count("company.B", "brant_clm.claim_case"))
	}
}

// Two returns of 60 against one confirmed line of 100, started together: the shipment-line lock
// lets exactly one commit (CLM-INV-002).
func TestConcurrentReturnsCannotExceedShipped(t *testing.T) {
	w := newFulWorld(t)
	w.confirmQuantity("shipment.race", "100")
	for _, id := range []string{"claim.a", "claim.b"} {
		_, err := w.c.CLM.OpenClaim(w.merch, clmapi.OpenClaim{IdempotencyKey: w.key(), ClaimID: id, OrderID: w.order.ObjectID,
			ShipmentID: "shipment.race", ShipmentLineID: "S1", Reason: "并发退货"})
		w.must(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	actors := []context.Context{w.merch, w.merch2}
	ids := []string{"claim.a", "claim.b"}
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(actors[i], 30*time.Second)
			defer cancel()
			_, errs[i] = w.c.CLM.RegisterReturn(ctx, clmapi.RegisterReturn{IdempotencyKey: "race." + ids[i], ClaimID: ids[i],
				ReturnID: "ret.race", Quantity: "60", Condition: "received"})
		}(i)
	}
	close(start)
	wg.Wait()
	ok, exceeded := 0, 0
	for _, err := range errs {
		switch code(err) {
		case "OK":
			ok++
		case clmapi.CodeClaimReturnExceedsShipped:
			if path(err) != "/args/quantity" {
				t.Fatalf("path %s", path(err))
			}
			exceeded++
		default:
			t.Fatalf("concurrent result %s", code(err))
		}
	}
	if ok != 1 || exceeded != 1 {
		t.Fatalf("want one commit and one cap refusal, got ok %d exceeded %d (%s, %s)", ok, exceeded, code(errs[0]), code(errs[1]))
	}
	var sum string
	for _, id := range ids {
		view, err := w.c.CLM.GetClaim(w.merch, id)
		w.must(err)
		if len(view.Returns) == 1 {
			sum = view.Returns[0].Quantity
		}
	}
	if !sameQty(t, sum, "60") {
		t.Fatalf("counted return %s", sum)
	}
	if v := w.shipment("shipment.race"); v.Status != fulapi.StatusConfirmed || !sameQty(t, v.Lines[0].Quantity, "100") {
		t.Fatalf("shipment after the race %+v", v.Lines)
	}
}
