//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5R unit R7 (WBS S5R-07; simulated sign-off E0 of 2026-11-13 — not a real owner's sign-off): STL credit and
// order accounts on PostgreSQL 16. S5R-D02 order_account.open as STL's own permission; S5R-D08 the credit
// reservation follows an amended order revision (SM-STL-CREDIT-RESERVATION 承接 and superseded, STL-INV-004);
// S5R-D21 permitted and verified third-party payers (SM-STL-FUND fund.verify_payer, STL-INV-006, CN-AT08); S5R-D22
// the exposure converts receivables in other currencies at the credit rate (STL-INV-004). Python V0.50 has no STL:
// the tests cite the decisions and the design.

// creditSubscription is stl.order_credit of the credit world's STL.
func (cw *creditWorld) creditSubscription() messaging.Subscription {
	cw.t.Helper()
	for _, s := range cw.stl.Subscriptions() {
		if s.Consumer == "stl.order_credit" {
			return s
		}
	}
	cw.t.Fatal("STL declares no stl.order_credit")
	return messaging.Subscription{}
}

// creditRelay relays order events through stl.order_credit of cw, provisioned in company.A.
type creditRelay struct {
	cw    *creditWorld
	ev    *orderEvents
	scope kernel.Scope
	sub   messaging.Subscription
}

func newCreditRelay(cw *creditWorld) *creditRelay {
	cw.t.Helper()
	sub := cw.creditSubscription()
	r := &creditRelay{cw: cw, ev: newOrderEvents(cw.t, sub), scope: cw.scope("company.A"), sub: sub}
	if _, err := cw.platform.ProvisionInbox(context.Background(), r.scope, []messaging.Subscription{sub}); err != nil {
		cw.t.Fatal(err)
	}
	return r
}

func (r *creditRelay) relay(es ...eventing.Envelope) {
	r.cw.t.Helper()
	r.ev.enqueue(es...)
	r.ev.drain(r.ev.relay(5))
	r.ev.published(es...)
}

// activated reserves revision 1 of draft order id on credit of amountMinor (after shipment), activates it and
// relays its activation: the reservation res.<id>.r1 is consumed (PL-12).
func (r *creditRelay) activated(id, amountMinor string) stl.Order {
	r.cw.t.Helper()
	o := r.cw.onCredit(id, amountMinor)
	_, err := r.cw.reserve(r.cw.sales, "res."+id+".r1", o)
	r.cw.must(err)
	r.cw.orders.setStatus(id, stl.OrderActive)
	o.Status = stl.OrderActive
	a, _ := activation(r.scope, o, 5)
	r.relay(a)
	if v := r.cw.reservation("res." + id + ".r1"); v.Status != stlapi.ReservationConsumed {
		r.cw.t.Fatalf("activation of %s: %+v", id, v)
	}
	return o
}

// onCreditAt moves TRD's stand-in of order id to revision rev, amountMinor due 60 days after shipment.
func (cw *creditWorld) onCreditAt(id string, rev int64, amountMinor string) stl.Order {
	return cw.activeAt(id, rev, stl.OrderStage{StageID: "P2", Trigger: polapi.TriggerAfterShipment, Days: 60, Percent: "100", AmountMinor: amountMinor,
		OnCredit: true})
}

// held returns customer.1's reservations of order that are reserved or consumed, by order revision.
func (cw *creditWorld) held(order string) map[int64][]stlapi.ReservationView {
	cw.t.Helper()
	v, err := cw.stl.GetCustomerCredit(cw.officer, "customer.1")
	cw.must(err)
	out := map[int64][]stlapi.ReservationView{}
	for _, r := range v.Reservations {
		if r.OrderID == order && r.Status != stlapi.ReservationReleased {
			out[r.OrderRevision] = append(out[r.OrderRevision], r)
		}
	}
	return out
}

func (cw *creditWorld) exposureNow() string {
	cw.t.Helper()
	v, err := cw.stl.GetCustomerCredit(cw.officer, "customer.1")
	cw.must(err)
	return v.ExposureMinor
}

// appliedAt is the applied_at of OrderAmended e.
func appliedAt(t *testing.T, e eventing.Envelope) time.Time {
	t.Helper()
	var p struct {
		AppliedAt string `json:"applied_at"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatal(err)
	}
	at, err := kernel.ParseTimestamp(p.AppliedAt)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// S5R-D02 (Context Map STL.commands order_account.open; role templates: treasury holds it; commandActors
// order_account.open human|system): opening an order account is STL's own permission. Treasury opens one; a sales
// clerk is refused, and so is an AI agent even when granted the permission; a system actor that holds only
// payment_plan_review.open — the permission S5 borrowed — is refused; the consumer's actor with order_account.open
// opens one. Refusals are audited.
func TestS5ROrderAccountOpenIsItsOwnPermission(t *testing.T) {
	fw := newFundWorld(t)
	stage := payStage("P1", polapi.TriggerBeforeShipment, "50000")
	put := func(id string) stl.Order {
		o := stl.Order{Binding: stl.OrderBinding{OrderID: id, OrderRevision: 1, OrderHash: orderHashOf(id, 1)}, Status: stl.OrderActive,
			CustomerID: "customer.1", Currency: stl.CurrencyRef{ID: fw.usd.ConfigID, Revision: fw.usd.Revision}, Stages: []stl.OrderStage{stage}}
		fw.orders.put(o)
		return o
	}
	o := put("order.d02")
	scope := fw.scope("company.A")
	sales := fw.assign("sales.1", "sales_clerk")
	agent := fw.provision(bootstrap.Grant{Scope: scope, Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.accounts"},
		Permissions: []string{"order_account.open", "stl.read"}, DataScope: "company"})
	borrowed := fw.provision(bootstrap.Grant{Scope: scope, Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "system:inbox.stl.s5"},
		Permissions: []string{"payment_plan_review.open", "stl.read"}, DataScope: "company"})
	for name, who := range map[string]context.Context{"a sales clerk": sales, "an AI agent": agent, "payment_plan_review.open only": borrowed} {
		_, err := fw.stl.OpenOrderAccount(who, fw.openCmd(o))
		if code(err) != kernel.CodePermissionDenied {
			t.Errorf("%s: %v", name, err)
		}
	}
	r, err := fw.stl.OpenOrderAccount(fw.treasury, fw.openCmd(o))
	fw.must(err)
	if r.Status != stlapi.AccountOpen || r.RowVersion != 1 || r.PartyID != "customer.1" || r.Stages != 1 {
		t.Fatalf("treasury opens it: %+v", r)
	}
	if v, err := fw.stl.GetOrderAccount(fw.treasury, o.Binding.OrderID); err != nil || v.OpenedBy.ID != "treasury.1" ||
		v.OpenedBy.Kind != kernel.ActorHuman {
		t.Fatalf("opened by %+v %v", v.OpenedBy, err)
	}
	if _, err = fw.stl.OpenOrderAccount(fw.system, fw.openCmd(put("order.d02.sys"))); err != nil {
		t.Fatalf("the system actor holding order_account.open: %v", err)
	}
	if n := fw.count("company.A", "brant_stl.audit_event WHERE command = 'order_account.open' AND outcome = 'rejected' AND code = 'PERMISSION_DENIED'"); n != 3 {
		t.Fatalf("%d audited refusals", n)
	}
}

// S5R-D08 on SIM-02's figures (simulated G0 walkthrough SIM-02 step 7; STL-INV-004 positive "SIM-02：订单减到 8,280.00
// 后敞口按新信用部分计入"; SM-STL-CREDIT-RESERVATION 承接 and superseded): the order is activated on 9,200.00 O/A 60;
// the customer reduces it by 10 %, the amendment asks no more credit (TRD: not_increased), and OrderAmended names no
// reservation. STL's consumer carries revision 1's reservation over to revision 2 at 8,280.00 — a new reservation,
// consumed at the instant the amendment took effect by the consumer, basis carried_over, carried_from the first —
// and releases the first with the cause superseded, naming it; the exposure falls to 8,280.00, and CreditGuard v1
// answers covered for revision 2. A redelivery changes nothing. A later revision paid before shipment needs no
// credit: the carried reservation is released as superseded and nothing is held. On another order, the order's end
// releases its carried reservation with the cause order_ended.
func TestS5RCreditFollowsTheAmendedOrderSIM02(t *testing.T) {
	cw := newCreditWorld(t)
	cw.limit()
	cr := newCreditRelay(cw)
	cr.activated("order.sim", "920000")
	if e := cw.exposureNow(); e != "920000" {
		t.Fatalf("exposure at revision 1: %s", e)
	}
	o2 := cw.onCreditAt("order.sim", 2, "828000")
	e2 := amendment(cr.scope, o2, 6, "")
	cr.relay(e2)
	r1 := cw.reservation("res.order.sim.r1")
	held := cw.held("order.sim")
	if len(held) != 1 || len(held[2]) != 1 {
		t.Fatalf("held after revision 2: %+v", held)
	}
	c2 := held[2][0]
	if r1.Status != stlapi.ReservationReleased || r1.Version != 3 || r1.ReleaseCause != stlapi.CauseSuperseded || r1.ReleasedBy != consumerActor ||
		!strings.Contains(r1.ReleaseReason, "superseded by reservation "+c2.ReservationID) {
		t.Fatalf("revision 1's reservation: %+v", r1)
	}
	if c2.Status != stlapi.ReservationConsumed || c2.Version != 1 || c2.Basis != stlapi.BasisCarriedOver || c2.CarriedFrom != "res.order.sim.r1" ||
		c2.ExposureMinor != c2.PortionMinor || c2.OrderHash != o2.Binding.OrderHash ||
		c2.ReservedBy != consumerActor || c2.ConsumedBy != consumerActor || !c2.ActivatedAt.Equal(appliedAt(t, e2)) || c2.DecisionID != "" ||
		!c2.ExpiresAt.Equal(r1.ExpiresAt) || c2.CreditPolicy != r1.CreditPolicy {
		t.Fatalf("revision 2's carried-over reservation: %+v", c2)
	}
	if c2.PortionMinor != "828000" || cw.exposureNow() != "828000" {
		t.Fatalf("SIM-02: the exposure falls to 8,280.00: %s / %s", c2.PortionMinor, cw.exposureNow())
	}
	if cw.stlEvents(stlapi.EventCreditReservationConsumed, c2.ReservationID) != 1 || cw.stlEvents(stlapi.EventCreditReservationReleased, r1.ReservationID) != 1 {
		t.Fatal("one consumed and one released event")
	}
	v, err := newDownstream(cw.world).creditGuard(cr.scope, creditQuery(o2, "828000", "USD"))
	if err != nil || !v.Satisfied() || v.ReservationID != c2.ReservationID || v.Basis != stlapi.BasisCarriedOver || v.ReservationStatus != stlapi.ReservationConsumed {
		t.Fatalf("CreditGuard v1 for revision 2: %+v %v", v, err)
	}
	again, err := messaging.NewDispatcher(cr.sub)
	cw.must(err)
	if err = again.Publish(context.Background(), e2); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if h := cw.held("order.sim"); len(h[2]) != 1 || h[2][0].ReservationID != c2.ReservationID || h[2][0].Version != 1 ||
		cw.stlEvents(stlapi.EventCreditReservationConsumed, c2.ReservationID) != 1 {
		t.Fatalf("after the redelivery: %+v", h)
	}

	o3 := cw.activeAt("order.sim", 3, stl.OrderStage{StageID: "P1", Trigger: polapi.TriggerBeforeShipment, Percent: "100", AmountMinor: "828000"})
	cr.relay(amendment(cr.scope, o3, 8, ""))
	if c := cw.reservation(c2.ReservationID); c.Status != stlapi.ReservationReleased || c.Version != 2 || c.ReleaseCause != stlapi.CauseSuperseded ||
		len(cw.held("order.sim")) != 0 || cw.exposureNow() != "" {
		t.Fatalf("revision 3 needs no credit: %+v, held %+v, exposure %q", c, cw.held("order.sim"), cw.exposureNow())
	}

	cr.activated("order.end", "500000")
	o2 = cw.onCreditAt("order.end", 2, "400000")
	cr.relay(amendment(cr.scope, o2, 6, ""))
	c := cw.held("order.end")[2][0]
	cr.relay(salesOrderEvent(cr.scope, trdapi.EventOrderCancelled, o2.Binding, 7, "cancelled", nil))
	if c = cw.reservation(c.ReservationID); c.Status != stlapi.ReservationReleased || c.ReleaseCause != stlapi.CauseOrderEnded ||
		c.ReleasedBy != consumerActor {
		t.Fatalf("the order's end releases the carried reservation: %+v", c)
	}
}

// S5R-D08 (a): a reservation of the amended revision made although the apply asked none (S5 C4b gap 10: a "stray"
// one, or one reserved before apply, D-9) is consumed for it at the instant the amendment took effect; revision 1's
// is released as superseded; nothing is carried over. Then an amendment of the same order delivered out of order
// (STL-INV-004 concurrency "乱序事件（先到后一修订）幂等"): revision 4's OrderAmended arrives before revision 3's; the
// later one carries revision 2's reservation over to revision 4 and the late revision 3 changes nothing.
func TestS5RTheAmendedRevisionsOwnReservationAndOutOfOrderEvents(t *testing.T) {
	cw := newCreditWorld(t)
	cw.limit()
	cr := newCreditRelay(cw)
	cr.activated("order.stray", "920000")
	o2 := cw.onCreditAt("order.stray", 2, "828000")
	if _, err := cw.reserve(cw.sales, "res.order.stray.r2", o2); err != nil {
		t.Fatal(err)
	}
	e2 := amendment(cr.scope, o2, 6, "")
	cr.relay(e2)
	r1, r2 := cw.reservation("res.order.stray.r1"), cw.reservation("res.order.stray.r2")
	if r2.Status != stlapi.ReservationConsumed || r2.Version != 2 || r2.ConsumedBy != consumerActor || !r2.ActivatedAt.Equal(appliedAt(t, e2)) ||
		r1.Status != stlapi.ReservationReleased || r1.ReleaseCause != stlapi.CauseSuperseded || !strings.Contains(r1.ReleaseReason, "res.order.stray.r2") ||
		len(cw.held("order.stray")) != 1 || cw.exposureNow() != "828000" {
		t.Fatalf("the stray reservation of revision 2: %+v / %+v, held %+v", r1, r2, cw.held("order.stray"))
	}

	o3 := cw.onCreditAt("order.stray", 3, "760000")
	e3 := amendment(cr.scope, o3, 8, "")
	o4 := cw.onCreditAt("order.stray", 4, "700000")
	cr.relay(amendment(cr.scope, o4, 10, ""))
	held := cw.held("order.stray")
	if len(held) != 1 || len(held[4]) != 1 || held[4][0].CarriedFrom != "res.order.stray.r2" || held[4][0].PortionMinor != "700000" ||
		cw.reservation("res.order.stray.r2").ReleaseCause != stlapi.CauseSuperseded {
		t.Fatalf("revision 4 before revision 3: %+v", held)
	}
	cr.relay(e3)
	if h := cw.held("order.stray"); len(h) != 1 || h[4][0].ReservationID != held[4][0].ReservationID || h[4][0].Version != 1 || cw.exposureNow() != "700000" {
		t.Fatalf("revision 3 after revision 4 changes nothing: %+v", h)
	}
}

// rootLock holds customer.1's credit root FOR UPDATE in a transaction of company.A until release.
func (cw *creditWorld) rootLock() (pid int32, release func()) {
	cw.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(cw.t))
	cw.must(err)
	hold, pids, done := make(chan struct{}), make(chan int32, 1), make(chan error, 1)
	rollback := errors.New("the holder rolls back")
	go func() {
		done <- bento.ScopeTransactor{DB: db, Scope: cw.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
			exec, err := db.RequireExecutor(ctx)
			if err != nil {
				return err
			}
			var p int32
			if err = exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&p); err != nil {
				return err
			}
			if _, err = exec.Exec(ctx, `SELECT 1 FROM brant_stl.customer_credit WHERE tenant_id = $1 AND company_id = 'company.A'
				AND party_id = 'customer.1' FOR UPDATE`, cw.tenant); err != nil {
				return err
			}
			pids <- p
			<-hold
			return rollback
		})
	}()
	select {
	case pid = <-pids:
	case err := <-done:
		cw.t.Fatalf("holder: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(hold)
			if err := <-done; !errors.Is(err, rollback) {
				cw.t.Errorf("the holder must roll back: %v", err)
			}
		})
	}
	cw.t.Cleanup(release)
	return pid, release
}

// S5R-D08, STL-INV-004 concurrency "修订调整与人工 credit.release 并发按信用根锁裁决": the amendment's credit adjustment
// and a credit officer's release of the amended revision's own reservation (reserved, not asked by the apply)
// serialise on the customer's credit root. Released first, the reservation is abandoned and the consumer carries
// revision 1's over; adjusted first, the reservation is consumed and the release is refused (INVALID_STATE). Either
// way revision 2 holds exactly one consumed reservation of 1,800.00 and revision 1's is released as superseded — in
// both fixed orders and in three rounds where both wait on the root at once (a release that waited holds an
// earlier version: VERSION_CONFLICT).
func TestS5RAmendmentAndAPersonsReleaseMeetAtTheCreditRoot(t *testing.T) {
	cw := newCreditWorld(t)
	cw.limit()
	cr := newCreditRelay(cw)
	setup := func(id string) eventing.Envelope {
		t.Helper()
		cr.activated(id, "200000")
		o2 := cw.onCreditAt(id, 2, "180000")
		_, err := cw.reserve(cw.sales, "res."+id+".r2", o2)
		cw.must(err)
		return amendment(cr.scope, o2, 6, "")
	}
	check := func(id string, released error) string {
		t.Helper()
		held := cw.held(id)
		r1, r2 := cw.reservation("res."+id+".r1"), cw.reservation("res."+id+".r2")
		if len(held) != 1 || len(held[2]) != 1 || held[2][0].Status != stlapi.ReservationConsumed || held[2][0].PortionMinor != "180000" ||
			r1.Status != stlapi.ReservationReleased || r1.ReleaseCause != stlapi.CauseSuperseded {
			t.Fatalf("%s: held %+v, revision 1 %+v", id, held, r1)
		}
		if released == nil {
			if r2.Status != stlapi.ReservationReleased || r2.ReleaseCause != stlapi.CauseAbandoned || held[2][0].CarriedFrom != r1.ReservationID {
				t.Fatalf("%s released first: %+v / %+v", id, r2, held[2][0])
			}
			return "release first"
		}
		if (code(released) != kernel.CodeInvalidState && code(released) != kernel.CodeVersionConflict) || held[2][0].ReservationID != r2.ReservationID ||
			r2.Version != 2 {
			t.Fatalf("%s adjusted first: %v, %+v / %+v", id, released, r2, held[2][0])
		}
		return "adjustment first"
	}

	e := setup("order.p1")
	_, err := cw.release(cw.officer, "res.order.p1.r2", 1)
	cw.must(err)
	cr.relay(e)
	if got := check("order.p1", nil); got != "release first" {
		t.Fatal(got)
	}
	e = setup("order.p2")
	cr.relay(e)
	_, err = cw.release(cw.officer, "res.order.p2.r2", 2) // the officer sees the consumed version and is refused for its state
	if code(err) != kernel.CodeInvalidState {
		t.Fatalf("releasing the consumed reservation: %v", err)
	}
	if got := check("order.p2", err); got != "adjustment first" {
		t.Fatal(got)
	}
	for i := range 3 {
		id := fmt.Sprintf("order.race%d", i)
		e = setup(id)
		cr.ev.enqueue(e)
		pid, release := cw.rootLock()
		relayed, released := make(chan error, 1), make(chan error, 1)
		go func() { relayed <- drain(cr.ev.relay(5)) }()
		go func() {
			_, err := cw.release(cw.officer, "res."+id+".r2", 1)
			released <- err
		}()
		waitBlocked(t, pgtest.AppPool(t), pid, 2)
		release()
		if err = <-relayed; err != nil {
			t.Fatal(err)
		}
		cr.ev.published(e)
		t.Logf("round %d: %s", i, check(id, <-released))
	}
}

// S5R-D21, STL-INV-006, CN-AT08 ("许可付款人的收款可核销；临时第三方付款经 fund.verify_payer 核实（信用经理，独立于资金登记人，
// 附证据）后可核销；未核实前 PARTY_ENTITY_MISMATCH 待核"), SM-STL-FUND fund.verify_payer, Context Map R43 TradingProfileRef v1:
//
//   - agent.9 pays 500.00 for customer.1's order: PARTY_ENTITY_MISMATCH until customer.1's trading profile lists
//     agent.9 as a permitted payer (PTY, published); then the application stands on payer basis permitted_payer
//     with the profile revision. customer.2's profile permits nobody: agent.9's money for customer.2 is refused.
//   - agent.8 pays 500.00 for customer.1 once: refused until a credit manager verifies the payment for customer.1
//     on a payment instruction. Treasury holds no fund.verify_payer, an AI agent is refused (human only), the
//     recorder of a receipt never verifies it (INDEPENDENT_REVIEW_REQUIRED), the customer must be a confirmed
//     customer, the version must be current, and one customer is verified once. The verification moves the fund
//     record's version (2), keeps its state, publishes fund_payer_verified.v1 and shows on fund_record.get; then
//     the application stands on payer basis verified_payer — for customer.1 only.
//   - PostgreSQL refuses what the commands refuse: an application of a payer verified for another customer, a
//     fund record version moved without a verification, a verification by the recorder.
func TestS5RThirdPartyPayersCNAT08(t *testing.T) {
	fw := newFundWorld(t)
	o1 := fw.activeOrder("order.p1", "customer.1", fw.usd, payStage("P1", polapi.TriggerBeforeShipment, "100000"))
	fw.activeOrder("order.p2", "customer.2", fw.usd, payStage("P1", polapi.TriggerBeforeShipment, "100000"))
	fw.party(fw.editor, "agent.8", true, "evidence")
	mandate := fw.source(fw.editor, "mandate.1", "合成付款委托书：agent.8 代 customer.1 支付 500.00 USD（测试数据）")
	receipt := func(who context.Context, id, payer string) {
		t.Helper()
		cmd := fw.receiptCmd(id, "500.00")
		cmd.CounterpartyID = payer
		_, err := fw.stl.RecordFund(who, cmd)
		fw.must(err)
	}
	apply := func(id, fund, order, amount string) (stlapi.ApplicationReceipt, error) {
		return fw.stl.Apply(fw.treasury, stlapi.Apply{IdempotencyKey: fw.key(), ApplicationID: id, FundRecordID: fund,
			Target: stlapi.ApplicationTarget{Kind: stlapi.TargetOrderStage, OrderID: order, StageID: "P1"}, Amount: amount})
	}
	receipt(fw.treasury, "fund.a9", "agent.9")
	_, err := apply("app.a9", "fund.a9", o1.Binding.OrderID, "500.00")
	fw.want(err, stlapi.CodePartyEntityMismatch)

	payment, trade := fw.settlement(fw.editor, fw.reviewer)
	saved, err := fw.c.PTY.SaveTradingProfile(fw.editor, ptyapi.SaveTradingProfile{IdempotencyKey: fw.key(), PartyID: "customer.1",
		PartyRevision: 1, PaymentTerms: payment, TradeTerms: trade, Owner: ptyapi.OwnerRef{ActorID: "sales.owner", TeamID: "trade"},
		PermittedPayers: []string{"agent.9"}})
	fw.must(err)
	_, err = fw.publishProfile(fw.reviewer, "customer.1", saved.RowVersion, 1)
	fw.must(err)
	r, err := apply("app.a9", "fund.a9", o1.Binding.OrderID, "500.00")
	if err != nil || r.PayerBasis != stlapi.PayerPermitted || r.PayerProfileRevision != 1 || r.Status != stlapi.ApplicationApplied {
		t.Fatalf("a permitted payer: %+v %v", r, err)
	}
	if v, err := fw.stl.GetApplication(fw.treasury, "app.a9"); err != nil || v.PayerBasis != stlapi.PayerPermitted || v.PayerProfileRevision != 1 {
		t.Fatalf("application.get: %+v %v", v, err)
	}
	receipt(fw.treasury, "fund.a9b", "agent.9")
	_, err = apply("app.a9b", "fund.a9b", "order.p2", "100.00")
	fw.want(err, stlapi.CodePartyEntityMismatch)

	receipt(fw.treasury, "fund.a8", "agent.8")
	_, err = apply("app.a8", "fund.a8", o1.Binding.OrderID, "300.00")
	fw.want(err, stlapi.CodePartyEntityMismatch)
	verify := func(who context.Context, fund, customer string, expected int64) (stlapi.FundRecordReceipt, error) {
		return fw.stl.VerifyPayer(who, stlapi.VerifyPayer{IdempotencyKey: fw.key(), FundRecordID: fund, ExpectedVersion: expected,
			CustomerID: customer, EvidenceIDs: []string{mandate}, Reason: "客户书面付款委托（合成）"})
	}
	ai := fw.provision(bootstrap.Grant{Scope: fw.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.payers"},
		Permissions: []string{"fund.verify_payer", "stl.read", "evd.read", "pty.read"}, DataScope: "company"})
	for name, c := range map[string]struct {
		who      context.Context
		customer string
		expected int64
		code     string
	}{
		"treasury":           {fw.treasury, "customer.1", 1, kernel.CodePermissionDenied},
		"an AI agent":        {ai, "customer.1", 1, kernel.CodePermissionDenied},
		"nobody":             {fw.manager, "nobody", 1, kernel.CodeReferenceInvalid},
		"a stale version":    {fw.manager, "customer.1", 2, kernel.CodeVersionConflict},
		"the payer as payer": {fw.manager, "agent.8", 1, kernel.CodeInvalidState},
	} {
		if _, err := verify(c.who, "fund.a8", c.customer, c.expected); code(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	v, err := verify(fw.manager, "fund.a8", "customer.1", 1)
	if err != nil || v.Status != stlapi.FundRecorded || v.RowVersion != 2 || v.AvailableMinor != "50000" {
		t.Fatalf("verified: %+v %v", v, err)
	}
	_, err = verify(fw.manager, "fund.a8", "customer.1", 2)
	fw.want(err, kernel.CodeInvalidState)
	receipt(fw.treasury2, "fund.t2", "agent.8")
	_, err = verify(fw.treasury2, "fund.t2", "customer.1", 1)
	fw.want(err, stlapi.CodeIndependentReviewRequired)

	if r, err = apply("app.a8", "fund.a8", o1.Binding.OrderID, "300.00"); err != nil || r.PayerBasis != stlapi.PayerVerified || r.PayerProfileRevision != 0 {
		t.Fatalf("a verified payer: %+v %v", r, err)
	}
	_, err = apply("app.a8.p2", "fund.a8", "order.p2", "200.00")
	fw.want(err, stlapi.CodePartyEntityMismatch)
	f, err := fw.stl.GetFundRecord(fw.manager, "fund.a8")
	fw.must(err)
	if f.Version != 2 || f.Status != stlapi.FundRecorded || len(f.PayerVerifications) != 1 || f.PayerVerifications[0].PayerID != "agent.8" ||
		f.PayerVerifications[0].CustomerID != "customer.1" || f.PayerVerifications[0].VerifiedBy.ID != "manager.1" ||
		f.PayerVerifications[0].Reason != "客户书面付款委托（合成）" || len(f.Allocations) != 1 || f.Allocations[0].PayerBasis != stlapi.PayerVerified {
		t.Fatalf("fund_record.get: %+v", f)
	}
	var verified []map[string]any
	for _, e := range fw.events("company.A", stlapi.Source) {
		if e.Type == stlapi.EventFundPayerVerified {
			verified = append(verified, e.Payload)
		}
	}
	if len(verified) != 1 || verified[0]["fund_record_id"] != "fund.a8" || verified[0]["fund_record_version"] != float64(2) ||
		verified[0]["payer_party_id"] != "agent.8" || verified[0]["customer_party_id"] != "customer.1" {
		t.Fatalf("fund_payer_verified.v1: %v", verified)
	}

	for name, c := range map[string][2]string{
		"an application of a payer verified for another customer": {`INSERT INTO brant_stl.application (tenant_id, company_id, application_id,
			fund_record_id, target_kind, order_id, order_revision, stage_id, currency, minor_units, amount_minor, status, version, owner_actor_id,
			applied_by_kind, applied_by_id, apply_epoch, applied_at, updated_at, payer_basis) SELECT tenant_id, company_id, 'app.raw', 'fund.a8',
			'order_stage', 'order.p2', 1, 'P1', 'USD', 2, 100, 'applied', 1, 'treasury.1', 'human', 'treasury.1', 1, now(), now(), 'verified_payer'
			FROM brant_stl.fund_record WHERE fund_record_id = 'fund.a8'`, "brant_stl.application: app.raw applies no fund"},
		"a version without a verification": {`UPDATE brant_stl.fund_record SET version = version + 1 WHERE fund_record_id = 'fund.a9'`,
			"brant_stl.fund_record: the balance of fund.a9 does not hold"},
		"a verification by the recorder": {`INSERT INTO brant_stl.fund_payer_verification SELECT tenant_id, company_id, 'fund.t2', 1, 'agent.8',
			'customer.1', ARRAY['mandate.1'], 'self', 'human', 'treasury.2', 1, now() FROM brant_stl.fund_record WHERE fund_record_id = 'fund.t2'`,
			"brant_stl.fund_payer_verification: fund.t2 has no independent verification"},
	} {
		if err := fw.exec(c[0]); err == nil || !strings.Contains(err.Error(), c[1]) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// S5R-D21 on a receivable and S5R-D22 (STL-INV-004 "敞口中不同币种的应收（含期初）按 POL fx_rate 中用途为 credit 的汇率折算；缺
// 汇率答 FX_SCOPE_MISMATCH"; register: "跨币种期初应收按 credit 汇率计入敞口；缺汇率时 FX_SCOPE_MISMATCH"):
//
//   - customer.1 (limit 10,000.00 USD) owes an opening receivable of CNY 70,000.00. customer_credit.get cannot state
//     the exposure without a credit rate: it names CNY instead (no longer a failed read). credit.reserve of a USD
//     order of 1,000.00 is FX_SCOPE_MISMATCH at /args/exposure_fx_rates; a settlement-purpose rate is refused at its
//     path; with the CNY→USD credit rate 0.142857 the receivable counts 9,999.99, so 1,000.00 more opens a credit
//     decision (CREDIT_LIMIT_EXCEEDED, exposure 9,999.99, excess 999.99) — S5 B2b's conversion at an order's
//     reservation rate had no rate for an opening receivable at all.
//   - A USD opening receivable of customer.1 paid by agent.9: PARTY_ENTITY_MISMATCH until a credit manager verified
//     the payment for customer.1; then the receivable application stands on verified_payer.
func TestS5RExposureAtTheCreditRateAndAThirdPartyOnAReceivable(t *testing.T) {
	ow := newOpeningWorld(t)
	ow.limit()
	cny := ow.statementItem("ob.cny", stlapi.OpeningReceivable, "AR-CNY-0001", "70000.00", "70000.00", "70000.00", 30)
	cny.Currency = stlRef(ow.cny)
	ow.record(cny)
	v, err := ow.stl.GetCustomerCredit(ow.officer, "customer.1")
	if err != nil || v.ExposureMinor != "" || len(v.UnconvertedCurrencies) != 1 || v.UnconvertedCurrencies[0] != "CNY" || v.ExposureCurrency != "USD" {
		t.Fatalf("customer_credit.get with a CNY receivable: %+v %v", v, err)
	}
	o := ow.onCredit("order.d22", "100000")
	_, err = ow.reserve(ow.sales, "res.d22", o)
	if code(err) != stlapi.CodeFXScopeMismatch || path(err) != "/args/exposure_fx_rates" {
		t.Fatalf("no credit rate for CNY: %v", err)
	}
	rate := func(id, purpose string) polapi.ConfigRef {
		return ow.configure(ow.editor, ow.reviewer, polapi.KindFXRate, id, 0, ow.win.from, ow.win.until,
			polapi.ConfigData{FXRate: &polapi.FXRate{FromCurrency: "CNY", ToCurrency: "USD", Rate: "0.142857", Purpose: purpose,
				SourceName: "SYNTHETIC RATE", ObservedAt: kernel.FormatTimestamp(ow.win.from)}}, true)
	}
	credit, settlement := rate("cny.usd.credit", polapi.PurposeCredit), rate("cny.usd.settlement", polapi.PurposeSettlement)
	cmd := ow.reserveCmd("res.d22", o)
	cmd.ExposureFXRates = []stlapi.ConfigRef{stlRef(settlement)}
	_, err = ow.stl.ReserveCredit(ow.sales, cmd)
	if code(err) != stlapi.CodeFXScopeMismatch || path(err) != "/args/exposure_fx_rates/0" {
		t.Fatalf("a settlement-purpose rate: %v", err)
	}
	cmd = ow.reserveCmd("res.d22", o)
	cmd.ExposureFXRates = []stlapi.ConfigRef{stlRef(credit)}
	r, err := ow.stl.ReserveCredit(ow.sales, cmd)
	if err != nil || r.Status != stlapi.StatusDecisionOpened || r.ExposureMinor != "999999" || r.ExcessMinor != "99999" || r.LimitMinor != "1000000" {
		t.Fatalf("at the credit rate: %+v %v", r, err)
	}
	d, err := ow.stl.GetDecision(ow.officer, r.DecisionID)
	if err != nil || len(d.Reasons) != 1 || d.Reasons[0] != stlapi.CodeCreditLimitExceeded || d.ExposureMinor != "999999" {
		t.Fatalf("decision %+v %v", d, err)
	}

	ow.party(ow.editor, "agent.9", true, "evidence")
	ow.record(ow.statementItem("ob.usd", stlapi.OpeningReceivable, "AR-USD-0002", "500.00", "500.00", "500.00", 30))
	_, err = ow.stl.RecordFund(ow.treasury, stlapi.RecordFund{IdempotencyKey: ow.key(), FundRecordID: "fund.agent", FundKind: stlapi.FundReceipt,
		CounterpartyID: "agent.9", Currency: stlRef(ow.usd), BankAccount: stlRef(ow.bankUSD), ReceivedAmount: "500.00",
		ValueDate: kernel.FormatTimestamp(time.Now()), BankReference: "ref.fund.agent", EvidenceIDs: []string{ow.advice}})
	ow.must(err)
	_, err = ow.apply("app.agent", "fund.agent", stlapi.TargetReceivable, "ob.usd", "500.00")
	ow.want(err, stlapi.CodePartyEntityMismatch)
	_, err = ow.stl.VerifyPayer(ow.manager1, stlapi.VerifyPayer{IdempotencyKey: ow.key(), FundRecordID: "fund.agent", ExpectedVersion: 1,
		CustomerID: "customer.1", EvidenceIDs: []string{ow.advice}, Reason: "三方协议（合成）"})
	ow.must(err)
	a, err := ow.apply("app.agent", "fund.agent", stlapi.TargetReceivable, "ob.usd", "500.00")
	if err != nil || a.PayerBasis != stlapi.PayerVerified || a.StatementOpenMinor != "0" {
		t.Fatalf("the receivable paid by a verified payer: %+v %v", a, err)
	}
}

// S5R-D21 over HTTP (contract brant.settlement/0.1.0) on TRD's real order: the treasurer opens the account
// (order_account.open, S5R-D02); agent.9's receipt for the order is 409 PARTY_ENTITY_MISMATCH; the AI agent's
// fund.verify_payer is 403; the credit manager's is 200 (row_version 2, recorded) and reconciles; application.get
// then shows payer_basis verified_payer, and fund_record.get shows payer_verifications. credit.reserve's
// exposure_fx_rates (S5R-D22) is part of the contract: a reference of another kind is 422 at its path.
func TestS5RThirdPartyPayerOverHTTP(t *testing.T) {
	q := newOrderWorld(t)
	o := q.activeOrder("quote.1")
	snap := q.snapshots(q.author, o.ObjectID)[o.ObjectID]
	usdBank := q.configure(q.author, q.reviewer, polapi.KindBankAccount, "bank.usd", 0, q.win.from, time.Time{},
		bank(polapi.OwnerCompany, "company.A", "USD", polapi.AccountBoth), true)
	q.party(q.author, "agent.9", true, "evidence")
	q.assign("treasury.1", "treasury")
	q.assign("manager.1", "credit_manager")
	q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.payers"},
		Permissions: []string{"fund.verify_payer", "stl.read"}, DataScope: "company"})
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	srv := server.URL
	treasurer := &client{t: t, base: srv, token: is.token(q.tenant, "company.A", "treasury.1", "human"), n: 300}
	manager := &client{t: t, base: srv, token: is.token(q.tenant, "company.A", "manager.1", "human"), n: 400}
	agent := &client{t: t, base: srv, token: is.token(q.tenant, "company.A", "agent.payers", "ai"), n: 500}
	ref := func(r polapi.ConfigRef) map[string]any {
		return map[string]any{"kind": r.Kind, "id": r.ConfigID, "revision": r.Revision}
	}

	treasurer.command("stl", stlapi.Contract, stlapi.CommandOpenOrderAccount, map[string]any{"order_id": o.ObjectID,
		"order_revision": snap.Binding.OrderRevision, "order_hash": snap.Binding.OrderHash})
	treasurer.command("stl", stlapi.Contract, stlapi.CommandRecordFund, map[string]any{"fund_record_id": "fund.agent", "kind": "receipt",
		"counterparty_id": "agent.9", "currency": ref(q.usd), "bank_account": ref(usdBank), "received_amount": "960.00",
		"value_date": kernel.FormatTimestamp(time.Now()), "bank_reference": "MT103-AGENT", "evidence_ids": []any{"evidence"}})
	apply := map[string]any{"application_id": "app.agent", "fund_record_id": "fund.agent",
		"target": map[string]any{"kind": "order_stage", "order_id": o.ObjectID, "stage_id": snap.Stages[0].StageID}, "amount": "960.00"}
	if status, out := treasurer.try("stl", stlapi.Contract, stlapi.CommandApply, apply); status != 409 || out["code"] != stlapi.CodePartyEntityMismatch {
		t.Fatalf("a third party unverified: %d %v", status, out)
	}
	verify := map[string]any{"contract": stlapi.Contract, "command": stlapi.CommandVerifyPayer, "idempotency_key": "stl.http.verify",
		"args": map[string]any{"fund_record_id": "fund.agent", "expected_version": 1, "customer_id": "customer.1", "evidence_ids": []any{"evidence"},
			"reason": "客户付款委托书（合成）"}}
	if status, out := agent.do("POST", "/api/brant/v1/stl/commands/fund.verify_payer", verify); status != 403 || out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("AI agent: %d %v", status, out)
	}
	status, out := manager.do("POST", "/api/brant/v1/stl/commands/fund.verify_payer", verify)
	if status != 200 || out["status"] != stlapi.FundRecorded || out["row_version"] != float64(2) || out["object_id"] != "fund.agent" {
		t.Fatalf("verify %d %v", status, out)
	}
	if status, rec := manager.do("POST", "/api/brant/v1/stl/commands/fund.verify_payer/reconcile", verify); status != 200 || rec["state"] != "committed" {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	if r := treasurer.command("stl", stlapi.Contract, stlapi.CommandApply, apply); r["status"] != stlapi.ApplicationApplied {
		t.Fatalf("apply %v", r)
	}
	if status, app := treasurer.do("GET", "/api/brant/v1/stl/applications/app.agent", nil); status != 200 || app["payer_basis"] != stlapi.PayerVerified {
		t.Fatalf("application.get %d %v", status, app)
	}
	status, fund := manager.do("GET", "/api/brant/v1/stl/fund_records/fund.agent", nil)
	verifications, _ := fund["payer_verifications"].([]any)
	if status != 200 || len(verifications) != 1 || verifications[0].(map[string]any)["customer_id"] != "customer.1" ||
		verifications[0].(map[string]any)["payer_id"] != "agent.9" {
		t.Fatalf("fund_record.get %d %v", status, fund)
	}
	status, out = treasurer.try("stl", stlapi.Contract, stlapi.CommandReserve, map[string]any{"reservation_id": "res.http", "order_id": o.ObjectID,
		"order_revision": 1, "order_hash": snap.Binding.OrderHash, "credit_policy": map[string]any{"kind": "credit_policy", "id": "credit", "revision": 1},
		"exposure_fx_rates": []any{map[string]any{"kind": "currency", "id": "CNY", "revision": 1}}})
	if status != 422 || out["code"] != kernel.CodeSchemaInvalid || brcDig(out, "field_errors", 0, "path") != "/args/exposure_fx_rates/0/kind" {
		t.Fatalf("exposure_fx_rates over HTTP: %d %v", status, out)
	}
}
