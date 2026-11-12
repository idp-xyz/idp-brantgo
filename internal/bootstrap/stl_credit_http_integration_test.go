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
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5-12a over HTTP with OIDC access tokens, on the composed STL and TRD's real orders
// (OrderCommercialSnapshot v1, R32): BRC-001's 960.00 USD moved by an amendment to payment terms of
// 30 % on order confirmation and 70 % 30 days after shipment, as unit A2's snapshot test does it
// (revision 2: 288.00 + 672.00, 672.00 on credit; TRD-INV-022; S5 plan §5 steps 2 and 3). The sales
// clerk requests a limit of 10,000.00 USD on evidence and two credit managers publish it (above the
// top tier, D-6); the sales clerk reserves revision 2's 672.00 within the limit (D-7); the reads
// show the reservation and the exposure; the command reconciles; revision 1 is REFERENCE_STALE.
// CreditGuard v1, called in TRD's own transaction with the portion TRD's snapshot states (as
// amendment apply will for the next revision, D-9, unit B4), covers it; once the system consumes
// it (PL-12, unit B6) it still does.
func TestCreditOverHTTPWithTRDOrders(t *testing.T) {
	q := newOrderWorld(t)
	policy := q.configure(q.author, q.reviewer, polapi.KindCreditPolicy, "credit.usd", 0, q.win.from, time.Time{}, e2Policy(), true)
	o := q.activeOrder("quote.1")
	v1 := q.getOrder(o.ObjectID)
	pay := q.configure(q.author, q.reviewer, polapi.KindPaymentTerms, q.payment.ConfigID, q.rootVersion(polapi.KindPaymentTerms,
		q.payment.ConfigID), q.win.from, time.Time{}, polapi.ConfigData{PaymentTerms: &polapi.PaymentTerms{
		Name: "30% on order confirmation, 70% 30 days after shipment", Stages: []polapi.PaymentStage{
			{StageID: "P1", Percent: "30", Trigger: polapi.TriggerOrderConfirmed},
			{StageID: "P2", Percent: "70", Trigger: polapi.TriggerAfterShipment, Days: 30}}}}, true)
	cmd := q.amendCmd(o.ObjectID, "amendment.pay", 0, 1, v1.OrderHash, "120")
	cmd.Terms.PaymentTermsRef = tref(pay)
	p, err := q.c.TRD.PrepareAmendment(q.author, cmd)
	q.must(err)
	s, err := q.c.TRD.SubmitAmendment(q.author, trdapi.SubmitAmendment{IdempotencyKey: q.key(), OrderID: o.ObjectID, AmendmentID: "amendment.pay",
		ExpectedVersion: p.RowVersion})
	q.must(err)
	dec, err := q.c.TRD.DecideAmendment(q.reviewer, trdapi.DecideAmendment{IdempotencyKey: q.key(), OrderID: o.ObjectID,
		AmendmentID: "amendment.pay", ExpectedVersion: s.RowVersion, AmendmentHash: s.AmendmentHash, Decision: trdapi.DecisionApprove,
		Reason: "客户书面确认新付款条件"})
	q.must(err)
	_, err = q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(o.ObjectID, "amendment.pay", dec.RowVersion))
	q.must(err)
	snap := q.snapshots(q.author, o.ObjectID)[o.ObjectID]
	if snap.Status != trdapi.OrderActive || snap.Binding.OrderRevision != 2 || snap.CreditPortionMinor != "67200" || len(snap.Stages) != 2 ||
		!snap.Stages[1].OnCredit {
		t.Fatalf("TRD's snapshot %+v", snap)
	}

	q.assign("sales.1", "sales_clerk")
	q.assign("manager.1", "credit_manager")
	q.assign("manager.2", "credit_manager")
	system := q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "system:inbox.stl"},
		Permissions: []string{"credit.consume", "credit.release", "credit.reserve", "stl.read"}, DataScope: "company"})
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	sales := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "sales.1", "human")}
	m1 := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "manager.1", "human"), n: 100}
	m2 := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "manager.2", "human"), n: 200}
	ref := func(r polapi.ConfigRef) map[string]any {
		return map[string]any{"kind": r.Kind, "id": r.ConfigID, "revision": r.Revision}
	}

	r := sales.command("stl", stlapi.Contract, stlapi.CommandRequestLimit, map[string]any{"request_id": "limit.1", "party_id": "customer.1",
		"currency": ref(q.usd), "sources": []any{map[string]any{"source": "internal_assessment", "amount": "10000.00", "evidence_ids": []any{"evidence"}}},
		"valid_from": kernel.FormatTimestamp(q.win.from), "valid_until": kernel.FormatTimestamp(q.win.from.Add(365 * 24 * time.Hour)),
		"credit_policy": ref(policy), "reason": creditReason})
	if r["object_id"] != "limit.1" || r["status"] != "requested" || r["row_version"] != float64(1) {
		t.Fatalf("request %v", r)
	}
	publish := map[string]any{"request_id": "limit.1", "expected_version": 1, "decision": "publish", "credit_policy": ref(policy), "reason": creditReason}
	if r = m1.command("stl", stlapi.Contract, stlapi.CommandPublishLimit, publish); r["status"] != "requested" || r["row_version"] != float64(2) {
		t.Fatalf("first approval %v", r)
	}
	publish["expected_version"] = 2
	if status, out := m1.try("stl", stlapi.Contract, stlapi.CommandPublishLimit, publish); status != http.StatusForbidden ||
		out["code"] != stlapi.CodeCreditDecisionNotAuthorized {
		t.Fatalf("the same manager twice: %d %v", status, out)
	}
	if r = m2.command("stl", stlapi.Contract, stlapi.CommandPublishLimit, publish); r["status"] != "published" || r["row_version"] != float64(3) {
		t.Fatalf("published %v", r)
	}

	stale := map[string]any{"reservation_id": "res.stale", "order_id": o.ObjectID, "order_revision": 1, "order_hash": v1.OrderHash,
		"credit_policy": ref(policy)}
	if status, out := sales.try("stl", stlapi.Contract, stlapi.CommandReserve, stale); status != http.StatusConflict || out["code"] != stlapi.CodeReferenceStale {
		t.Fatalf("revision 1: %d %v", status, out)
	}
	reserve := map[string]any{"contract": stlapi.Contract, "command": stlapi.CommandReserve, "idempotency_key": "stl.http.reserve",
		"args": map[string]any{"reservation_id": "res.credit", "order_id": o.ObjectID, "order_revision": 2, "order_hash": snap.Binding.OrderHash,
			"credit_policy": ref(policy)}}
	status, res := sales.do("POST", "/api/brant/v1/stl/commands/credit.reserve", reserve)
	if status != http.StatusOK || res["object_id"] != "res.credit" || res["status"] != "reserved" || res["row_version"] != float64(1) {
		t.Fatalf("reserve %d %v", status, res)
	}
	status, v := sales.do("GET", "/api/brant/v1/stl/credit_reservations/res.credit", nil)
	if status != http.StatusOK || v["ok"] != true || v["portion_minor"] != "67200" || v["currency"] != "USD" || v["basis"] != "within_limit" ||
		v["state"] != "reserved" || v["order_hash"] != snap.Binding.OrderHash || v["order_revision"] != float64(2) || v["limit_currency"] != "USD" || v["exposure_minor"] != "67200" {
		t.Fatalf("reservation %d %v", status, v)
	}
	status, credit := m1.do("GET", "/api/brant/v1/stl/customers/customer.1/credit", nil)
	limit, _ := credit["limit_in_force"].(map[string]any)
	if status != http.StatusOK || credit["exposure_minor"] != "67200" || limit["amount_minor"] != "1000000" || len(credit["reservations"].([]any)) != 1 {
		t.Fatalf("customer credit %d %v", status, credit)
	}
	status, rec := sales.do("POST", "/api/brant/v1/stl/commands/credit.reserve/reconcile", reserve)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != res["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	if status, out := sales.try("stl", "brant.other/0.1.0", stlapi.CommandReserve, map[string]any{"reservation_id": "x"}); status != http.StatusBadRequest ||
		out["code"] != kernel.CodeUnsupportedContract {
		t.Fatalf("another contract: %d %v", status, out)
	}
	if status, out := sales.do("GET", "/api/brant/v1/stl/credit_decisions/none", nil); status != http.StatusNotFound || out["code"] != kernel.CodeNotFound {
		t.Fatalf("no decision: %d %v", status, out)
	}

	query := stlapi.CreditQuery{OrderID: o.ObjectID, OrderRevision: snap.Binding.OrderRevision, OrderHash: snap.Binding.OrderHash,
		PortionMinor: snap.CreditPortionMinor, Currency: snap.CurrencyRef.ID, At: time.Now()}
	guard := func() stlapi.CreditVerdict {
		var verdict stlapi.CreditVerdict
		q.must(q.inTRD(func(ctx context.Context, exec bentopg.Executor) error {
			var err error
			verdict, err = stlapi.ScanCreditGuard(query, exec.QueryRow(ctx, stlapi.CreditGuardSQL, stlapi.CreditGuardArgs(query)...).Scan)
			return err
		}))
		return verdict
	}
	if v := guard(); v.Check("/args/order_id") != nil || v.ReservationID != "res.credit" || v.ReservedMinor != "67200" ||
		v.ReservationStatus != stlapi.ReservationReserved || v.PartyID != "customer.1" {
		t.Fatalf("CreditGuard v1 in TRD's transaction: %+v", v)
	}
	c, err := q.c.STL.ConsumeCredit(system, stlapi.ConsumeCredit{IdempotencyKey: q.key(), ReservationID: "res.credit", OrderID: o.ObjectID,
		OrderRevision: 2, OrderHash: snap.Binding.OrderHash, ActivatedAt: kernel.FormatTimestamp(time.Now())})
	q.must(err)
	if v := guard(); c.Status != stlapi.ReservationConsumed || v.Check("/args/order_id") != nil || v.ReservationStatus != stlapi.ReservationConsumed {
		t.Fatalf("consumed %+v, guard %+v", c, v)
	}
}
