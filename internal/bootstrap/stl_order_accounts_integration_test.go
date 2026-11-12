//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// STL's order account consumer stl.order_account (unit B6b; S5 plan D-4, D-18; tactical model
// STL-AGG-OrderAccount; Context Map R32, R45 PaymentConditionGuard v1) on PostgreSQL 16, through the
// assembled relay with the in-process dispatcher and the assembled replayer, on the order event
// harness of stl_order_events_integration_test.go: TRD's OrderActivated opens the order's account at
// the activated revision with that revision's payment stages, as system:inbox.stl.order_account,
// once. The credit world's tests steer TRD's answers through its stand-in port;
// TestOrderAccountAppearsAfterActivationOverHTTP relays TRD's own events of a real order.

// accountActor is the system actor stl.order_account runs as.
var accountActor = kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.stl.order_account"}

// accountSubscription is STL's stl.order_account subscription of the credit world's STL.
func (cw *creditWorld) accountSubscription() messaging.Subscription {
	cw.t.Helper()
	for _, s := range cw.stl.Subscriptions() {
		if s.Consumer == "stl.order_account" {
			return s
		}
	}
	cw.t.Fatal("STL declares no stl.order_account")
	return messaging.Subscription{}
}

func (cw *creditWorld) account(orderID string) (stlapi.OrderAccountView, error) {
	return cw.stl.GetOrderAccount(cw.officer, orderID)
}

// activeAt gives TRD's stand-in an active order of customer.1 in USD at revision rev with stages.
func (cw *creditWorld) activeAt(id string, rev int64, stages ...stl.OrderStage) stl.Order {
	o := cw.draftOrder(id, cw.usd, stages...)
	o.Binding.OrderRevision, o.Binding.OrderHash, o.Status = rev, orderHashOf(id, rev), stl.OrderActive
	cw.orders.put(o)
	return o
}

// prepaid is 30 % of amountMinor due before shipment and 70 % 30 days after shipment.
func prepaid(amountMinor int64) []stl.OrderStage {
	return []stl.OrderStage{{StageID: "P1", Trigger: polapi.TriggerBeforeShipment, Percent: "30", AmountMinor: fmt.Sprint(amountMinor * 3 / 10)},
		{StageID: "P2", Trigger: polapi.TriggerAfterShipment, Days: 30, Percent: "70", AmountMinor: fmt.Sprint(amountMinor * 7 / 10), OnCredit: true}}
}

// openAccounts counts the committed order_account.open results of order in company.A, by actor kind
// when kind is not empty.
func (w *world) openAccounts(order, kind string) int {
	where := "command = 'order_account.open' AND object_id = '" + order + "'"
	if kind != "" {
		where += " AND actor_kind = '" + kind + "'"
	}
	return w.count("company.A", "brant_stl.action_result WHERE "+where)
}

// condition calls PaymentConditionGuard v1 for order at revision and scope in the downstream's
// transaction with its executor, as FUL will.
func (cw *creditWorld) condition(d *downstream, order string, revision int64, scope string) stlapi.PaymentVerdict {
	cw.t.Helper()
	v, err := d.paymentGuard(stlapi.PaymentQuery{OrderID: order, OrderRevision: revision, Scope: scope, At: time.Now()})
	cw.must(err)
	return v
}

// sameStages reports whether the account's stages are the order's payment plan, stage for stage.
func sameStages(v stlapi.OrderAccountView, stages []stl.OrderStage) bool {
	if len(v.Stages) != len(stages) {
		return false
	}
	for i, s := range stages {
		a := v.Stages[i]
		if a.StageID != s.StageID || a.Trigger != s.Trigger || a.Days != s.Days || a.Percent != s.Percent || a.AmountMinor != s.AmountMinor ||
			a.ReleaseControl != s.ReleaseControl || a.Seq != i+1 {
			return false
		}
	}
	return true
}

// stlKeyBlocker holds idempotency key of command in STL's ledger in an open transaction of
// company.A: the STL command with that key does all its writes, holding its row locks, and then
// waits to insert its result until release; the blocker then rolls back and the command commits.
func (w *world) stlKeyBlocker(command, key string) (release func()) {
	w.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	w.must(err)
	hold, ready, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	rollback := errors.New("the blocker rolls back")
	go func() {
		done <- bento.ScopeTransactor{DB: db, Scope: w.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
			exec, err := db.RequireExecutor(ctx)
			if err != nil {
				return err
			}
			if _, err = exec.Exec(ctx, `INSERT INTO brant_stl.action_result (tenant_id, company_id, command, idempotency_key, request_hash,
				action_id, object_id, row_version, status, actor_kind, actor_id, authority_epoch, correlation_id, recorded_at)
				VALUES ($1, 'company.A', $2, $3, repeat('0', 64), 'act.blocker', 'blocker', 1, 'blocked', 'human', 'blocker', 1, 'corr.blocker', now())`,
				w.tenant, command, key); err != nil {
				return err
			}
			close(ready)
			<-hold
			return rollback
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		w.t.Fatalf("blocker: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(hold)
			if err := <-done; !errors.Is(err, rollback) {
				w.t.Errorf("the blocker must roll back: %v", err)
			}
		})
	}
	w.t.Cleanup(release)
	return release
}

// waitFor polls cond for up to ten seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// D-4, D-18, R45: TRD's OrderActivated opens the order's account at the activated revision with
// TRD's payment plan of that revision, as system:inbox.stl.order_account — denied and retried until
// the operator provisions the consumer, then once: a redelivery and a second dispatcher change
// nothing. An order with nothing due before shipment gets its account too, so PaymentConditionGuard
// v1 answers satisfied for shipment instead of no_account. Activations of four orders relayed by two
// relays at once, while a third worker delivers each of them directly, open each account exactly
// once, and the guard blocks shipment on the stage due before shipment.
func TestOrderActivationOpensTheOrderAccountOnce(t *testing.T) {
	cw := newCreditWorld(t)
	ctx := context.Background()
	scope := cw.scope("company.A")
	d := newDownstream(cw.world)
	sub := cw.accountSubscription()
	ev := newOrderEvents(t, sub)

	oa := cw.onCredit("order.oa", "96000")
	cw.orders.setStatus("order.oa", stl.OrderActive)
	if v := cw.condition(d, "order.oa", 1, stlapi.ScopeShipment); v.Satisfied() || v.Reason != stlapi.PaymentNoAccount {
		t.Fatalf("before the activation: %+v", v)
	}
	activated, _ := activation(scope, oa, 5)
	ev.enqueue(activated)
	ev.once(ev.relay(5))
	if _, err := cw.account("order.oa"); ev.status(activated.ID) == "PUBLISHED" || code(err) != kernel.CodeNotFound {
		t.Fatalf("before provisioning: outbox %s, account %v; want the consumer denied and the delivery retried", ev.status(activated.ID), err)
	}
	if _, err := cw.platform.ProvisionInbox(ctx, scope, []messaging.Subscription{sub}); err != nil {
		t.Fatal(err)
	}
	ev.once(ev.relay(5))
	ev.published(activated)
	v, err := cw.account("order.oa")
	cw.must(err)
	if v.OrderRevision != 1 || v.OrderHash != oa.Binding.OrderHash || v.Status != "open" || v.Version != 1 || v.OpenedBy != accountActor ||
		v.PartyID != "customer.1" || v.Currency != "USD" || v.CurrencyRevision != cw.usd.Revision || !sameStages(v, oa.Stages) {
		t.Fatalf("account %+v", v)
	}
	if g := cw.condition(d, "order.oa", 1, stlapi.ScopeShipment); !g.Satisfied() || g.OrderHash != oa.Binding.OrderHash || g.RequiredMinor != "0" {
		t.Fatalf("guard of an order with nothing due before shipment: %+v", g)
	}
	second, err := messaging.NewDispatcher(sub)
	cw.must(err)
	for _, dd := range []*messaging.Dispatcher{ev.dispatcher, second} {
		if err = dd.Publish(ctx, activated); err != nil {
			t.Fatalf("redelivery: %v", err)
		}
	}
	if v, err := cw.account("order.oa"); err != nil || v.Version != 1 || cw.openAccounts("order.oa", "") != 1 {
		t.Fatalf("after redeliveries: %+v %v, %d opens", v, err, cw.openAccounts("order.oa", ""))
	}

	var events []eventing.Envelope
	var orders []stl.Order
	for i := range 4 {
		o := cw.activeAt(fmt.Sprintf("order.w%d", i), 1, prepaid(100000)...)
		e, _ := activation(scope, o, 5)
		events, orders = append(events, e), append(orders, o)
	}
	ev.enqueue(events...)
	var wg sync.WaitGroup
	errs := make([]error, 2+len(events))
	for i := range 2 {
		wg.Go(func() { errs[i] = drain(ev.relay(5)) })
	}
	for i, e := range events {
		wg.Go(func() {
			// Losing the race is a retry (the inbox entry or the account row is taken), never a rejection.
			if err := second.Publish(ctx, e); errors.Is(err, eventing.ErrPublishRejected) {
				errs[2+i] = err
			}
		})
	}
	wg.Wait()
	cw.must(errors.Join(errs...))
	ev.drain(ev.relay(5))
	ev.published(events...)
	for _, o := range orders {
		id := o.Binding.OrderID
		v, err := cw.account(id)
		cw.must(err)
		if v.Version != 1 || v.OpenedBy != accountActor || !sameStages(v, o.Stages) || cw.openAccounts(id, "") != 1 {
			t.Fatalf("%s with two workers: %+v, %d opens", id, v, cw.openAccounts(id, ""))
		}
		if g := cw.condition(d, id, 1, stlapi.ScopeShipment); g.Satisfied() || g.Reason != stlapi.PaymentStageUnpaid || g.StageID != "P1" ||
			g.ShortfallMinor != "30000" {
			t.Fatalf("%s guard before the money: %+v", id, g)
		}
	}
}

// A person's order_account.open racing the consumer, in both commit orders (B2a's command, D-4):
// while the consumer's transaction holds the account it inserted, the treasurer's open waits and,
// once the consumer commits, answers replayed on the consumer's account; while the treasurer's
// open holds its uncommitted account, the consumer waits, fails on the account the treasurer
// committed, and is delivered again — then it finds the account open at the activated revision
// and leaves it as it is. Each order has one account, opened once.
func TestHumanOrderAccountOpenRacesTheConsumer(t *testing.T) {
	cw := newCreditWorld(t)
	ctx := context.Background()
	scope := cw.scope("company.A")
	treasurer := cw.assign("treasury.1", "treasury")
	sub := cw.accountSubscription()
	hold, opened := make(chan struct{}), make(chan struct{}, 1)
	var holding atomic.Bool
	h := sub.Handler
	sub.Handler = func(ctx context.Context, e messaging.Event) error {
		if err := h(ctx, e); err != nil || !holding.Load() {
			return err
		}
		opened <- struct{}{}
		<-hold
		return nil
	}
	ev := newOrderEvents(t, sub)
	if _, err := cw.platform.ProvisionInbox(ctx, scope, []messaging.Subscription{sub}); err != nil {
		t.Fatal(err)
	}
	openCmd := func(o stl.Order) stlapi.OpenOrderAccount {
		return stlapi.OpenOrderAccount{IdempotencyKey: cw.key(), OrderID: o.Binding.OrderID, OrderRevision: o.Binding.OrderRevision,
			OrderHash: o.Binding.OrderHash}
	}

	first := cw.activeAt("order.c", 1, prepaid(50000)...)
	activated, _ := activation(scope, first, 5)
	ev.enqueue(activated)
	holding.Store(true)
	relayed := make(chan error, 1)
	go func() { relayed <- drain(ev.relay(5)) }()
	<-opened
	type answer struct {
		r   stlapi.OrderAccountReceipt
		err error
	}
	human := make(chan answer, 1)
	go func() {
		r, err := cw.stl.OpenOrderAccount(treasurer, openCmd(first))
		human <- answer{r, err}
	}()
	waitFor(t, "the treasurer's open to wait for the consumer's account", func() bool { return lockWaiter(t, "INSERT INTO brant_stl.order_account") })
	holding.Store(false)
	close(hold)
	cw.must(<-relayed)
	a := <-human
	ev.published(activated)
	v, err := cw.account("order.c")
	cw.must(err)
	if a.err != nil || !a.r.Replayed || a.r.OrderRevision != 1 || v.OpenedBy != accountActor || v.Version != 1 ||
		cw.openAccounts("order.c", "system") != 1 {
		t.Fatalf("consumer first: treasurer %+v %v, account %+v", a.r, a.err, v)
	}

	second := cw.activeAt("order.h", 1, prepaid(50000)...)
	cmd := openCmd(second)
	release := cw.stlKeyBlocker(stlapi.CommandOpenOrderAccount, cmd.IdempotencyKey)
	go func() {
		r, err := cw.stl.OpenOrderAccount(treasurer, cmd)
		human <- answer{r, err}
	}()
	waitFor(t, "the treasurer's open to hold its account", func() bool { return lockWaiter(t, "INSERT INTO brant_stl.action_result") })
	activatedH, _ := activation(scope, second, 5)
	ev.enqueue(activatedH)
	go func() {
		time.Sleep(5 * time.Millisecond)
		_, err := ev.relay(5).RunOnce(ctx)
		relayed <- err
	}()
	waitFor(t, "the consumer to wait for the treasurer's account", func() bool { return lockWaiter(t, "INSERT INTO brant_stl.order_account") })
	release()
	a = <-human
	cw.must(<-relayed)
	if s := ev.status(activatedH.ID); a.err != nil || a.r.Replayed || s != "PENDING" {
		t.Fatalf("treasurer first: treasurer %+v %v, outbox %s; want the consumer's delivery retried", a.r, a.err, s)
	}
	ev.drain(ev.relay(5))
	ev.published(activatedH)
	v, err = cw.account("order.h")
	cw.must(err)
	if v.OpenedBy.ID != "treasury.1" || v.Version != 1 || v.OrderRevision != 1 || cw.openAccounts("order.h", "system") != 0 ||
		cw.openAccounts("order.h", "human") != 1 {
		t.Fatalf("treasurer first: account %+v", v)
	}
}

// S4-06a (a dead-letter replay arrives after later events of its order) and D-4: an activation
// replayed after the order ended opens no account; one replayed after an amendment moved TRD's
// order to revision 2 opens none either (the payment plan review owns revision 2), and when the
// account was opened at revision 2 meanwhile it stays there. An activation of an order TRD does not
// show is refused for good (stl.order_account_not_openable), with nothing opened.
func TestReplayedActivationNeverOpensOrMovesAnAccountWrongly(t *testing.T) {
	cw := newCreditWorld(t)
	ctx := context.Background()
	scope := cw.scope("company.A")
	treasurer := cw.assign("treasury.1", "treasury")
	var broken atomic.Bool
	sub := breakable(cw.accountSubscription(), &broken, true)
	ev := newOrderEvents(t, sub)
	if _, err := cw.platform.ProvisionInbox(ctx, scope, []messaging.Subscription{sub}); err != nil {
		t.Fatal(err)
	}
	operator := cw.provision(bootstrap.Grant{Scope: scope, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "operator"},
		Permissions: []string{messaging.ReplayPermission}})
	replayer := ev.replayer()
	deadLetter := func(o stl.Order) eventing.Envelope {
		t.Helper()
		e, _ := activation(scope, o, 5)
		ev.enqueue(e)
		broken.Store(true)
		ev.once(ev.relay(1))
		broken.Store(false)
		if s := ev.status(e.ID); s != "ABANDONED" {
			t.Fatalf("broken consumer: outbox %s", s)
		}
		return e
	}
	replay := func(e eventing.Envelope) {
		t.Helper()
		if _, err := replayer.Replay(operator, e.Source, string(e.ID)); err != nil {
			t.Fatalf("replay %s: %v", e.Subject, err)
		}
	}

	ended := cw.activeAt("order.e", 1, prepaid(40000)...)
	endedActivation := deadLetter(ended)
	cw.orders.setStatus("order.e", "cancelled")
	replay(endedActivation)
	if _, err := cw.account("order.e"); code(err) != kernel.CodeNotFound {
		t.Fatalf("replay after the end: %v", err)
	}

	amended := cw.activeAt("order.m", 1, prepaid(40000)...)
	amendedActivation := deadLetter(amended)
	cw.activeAt("order.m", 2, prepaid(60000)...)
	replay(amendedActivation)
	if _, err := cw.account("order.m"); code(err) != kernel.CodeNotFound {
		t.Fatalf("replay after an amendment: %v", err)
	}

	moved := cw.activeAt("order.n", 1, prepaid(40000)...)
	movedActivation := deadLetter(moved)
	rev2 := cw.activeAt("order.n", 2, prepaid(60000)...)
	_, err := cw.stl.OpenOrderAccount(treasurer, stlapi.OpenOrderAccount{IdempotencyKey: cw.key(), OrderID: "order.n", OrderRevision: 2,
		OrderHash: rev2.Binding.OrderHash})
	cw.must(err)
	replay(movedActivation)
	if v, err := cw.account("order.n"); err != nil || v.OrderRevision != 2 || v.OrderHash != rev2.Binding.OrderHash || v.Version != 1 ||
		v.OpenedBy.ID != "treasury.1" || !sameStages(v, rev2.Stages) || cw.openAccounts("order.n", "system") != 0 {
		t.Fatalf("replay after the account moved to revision 2: %+v %v", v, err)
	}

	unknown := stl.Order{Binding: stl.OrderBinding{OrderID: "order.unknown", OrderRevision: 1, OrderHash: orderHashOf("order.unknown", 1)}}
	e, _ := activation(scope, unknown, 5)
	ev.enqueue(e)
	ev.once(ev.relay(5))
	alerts := ev.alerts.of(e.ID)
	if s := ev.status(e.ID); s != "ABANDONED" || len(alerts) != 1 || alerts[0].Code != messaging.FailurePublishRejected ||
		!strings.Contains(alerts[0].Error, "stl.order_account_not_openable") {
		t.Fatalf("an order TRD does not show: outbox %s, alerts %+v", s, alerts)
	}
	if _, err := cw.account("order.unknown"); code(err) != kernel.CodeNotFound {
		t.Fatalf("account of an unknown order: %v", err)
	}
}

// D-4, D-18 and the STL half of S5-07 over HTTP with TRD's own events (R32, R45): the order world's
// order, activated through the shared helpers and paid 100 % before shipment, has no account until
// TRD's events are relayed; then the account appears, opened by system:inbox.stl.order_account at
// the activated revision with TRD's sealed payment plan — nobody opened it by hand. Shipment is
// blocked on the stage until the treasurer applies the customer's 960.00 receipt to it; then
// PaymentConditionGuard v1, called in TRD's transaction as FUL will, is satisfied.
func TestOrderAccountAppearsAfterActivationOverHTTP(t *testing.T) {
	q := newOrderWorld(t)
	ctx := context.Background()
	scope := q.scope("company.A")
	usdBank := q.configure(q.author, q.reviewer, polapi.KindBankAccount, "bank.usd", 0, q.win.from, time.Time{},
		bank(polapi.OwnerCompany, "company.A", "USD", polapi.AccountBoth), true)
	q.assign("treasury.1", "treasury")
	subs := q.c.Subscriptions()
	ev := newOrderEvents(t, subs...)
	if _, err := q.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	treasurer := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "treasury.1", "human"), n: 600}
	ref := func(r polapi.ConfigRef) map[string]any {
		return map[string]any{"kind": r.Kind, "id": r.ConfigID, "revision": r.Revision}
	}

	o := q.activeOrder("quote.1")
	snap := q.snapshots(q.author, o.ObjectID)[o.ObjectID]
	if len(snap.Stages) != 1 || snap.Stages[0].Trigger != polapi.TriggerBeforeShipment || snap.Stages[0].AmountMinor != "96000" {
		t.Fatalf("TRD's snapshot %+v", snap)
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
	if status, out := treasurer.do("GET", "/api/brant/v1/stl/order_accounts/"+o.ObjectID, nil); status != http.StatusNotFound ||
		guard().Reason != stlapi.PaymentNoAccount {
		t.Fatalf("before the relay: %d %v", status, out)
	}

	relayed := ev.copyFrom(scope, trdapi.Source)
	ev.drain(ev.relay(5))
	ev.published(relayed...)
	status, acc := treasurer.do("GET", "/api/brant/v1/stl/order_accounts/"+o.ObjectID, nil)
	stages, _ := acc["stages"].([]any)
	conditions, _ := acc["conditions"].([]any)
	if status != http.StatusOK || acc["order_revision"] != float64(snap.Binding.OrderRevision) || acc["order_hash"] != snap.Binding.OrderHash ||
		len(stages) != 1 || stages[0].(map[string]any)["stage_id"] != snap.Stages[0].StageID ||
		stages[0].(map[string]any)["amount_minor"] != "96000" || stages[0].(map[string]any)["trigger"] != polapi.TriggerBeforeShipment ||
		!strings.Contains(fmt.Sprint(acc["opened_by"]), "inbox.stl.order_account") || len(conditions) != 2 ||
		conditions[0].(map[string]any)["status"] != "blocked" || q.openAccounts(o.ObjectID, "human") != 0 || q.openAccounts(o.ObjectID, "system") != 1 {
		t.Fatalf("account after the relay: %d %v", status, acc)
	}
	if v := guard(); v.Satisfied() || v.Reason != stlapi.PaymentStageUnpaid || v.ShortfallMinor != "96000" {
		t.Fatalf("guard before the money: %+v", v)
	}

	record := map[string]any{"fund_record_id": "fund.b6b", "kind": "receipt", "counterparty_id": "customer.1", "currency": ref(q.usd),
		"bank_account": ref(usdBank), "received_amount": "960.00", "value_date": kernel.FormatTimestamp(time.Now()),
		"bank_reference": "MT103-B6B", "evidence_ids": []any{"evidence"}}
	if r := treasurer.command("stl", stlapi.Contract, stlapi.CommandRecordFund, record); r["status"] != "recorded" {
		t.Fatalf("fund.record %v", r)
	}
	if r := treasurer.command("stl", stlapi.Contract, stlapi.CommandApply, map[string]any{"application_id": "app.b6b", "fund_record_id": "fund.b6b",
		"target": map[string]any{"kind": "order_stage", "order_id": o.ObjectID, "stage_id": snap.Stages[0].StageID}, "amount": "960.00"}); r["status"] != "applied" {
		t.Fatalf("apply %v", r)
	}
	if v := guard(); !v.Satisfied() || v.AppliedMinor != "96000" || v.OrderHash != snap.Binding.OrderHash {
		t.Fatalf("guard after the application: %+v", v)
	}
}
