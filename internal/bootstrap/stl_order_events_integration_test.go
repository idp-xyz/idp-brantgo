//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// STL's order event consumer stl.order_credit (WBS S5-12c, unit B6; event storming PL-12 and PL-13;
// WF-08 step 5; SM-STL-CREDIT-RESERVATION; Context Map R32) on PostgreSQL 16, through the assembled
// relay with the in-process dispatcher as its publisher (Platform.Relay, as brant-worker -dispatch
// runs it) and the assembled replayer (S4-06a; ADR-BR-015 dead letters). Each test relays a Bento
// outbox of its own, so the relay claims only the test's events; the consumer's Inbox and STL's
// rows are STL's own. Python V0.50 has no STL, so the tests cite design IDs only.
//
// TRD's order world cannot activate an order whose payment plan holds credit yet (its payment
// terms are bound through the costing basis and the trading profile), so the credit world's tests
// give STL a stand-in for TRD's orders and relay the SalesOrder events TRD's command ledger writes,
// built field for field; TestTRDOrderEventsReachSTLThroughTheRelay relays TRD's own events of a real
// order, activated through the shared order-world helpers.

// consumerActor is the system actor stl.order_credit runs as.
var consumerActor = kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.stl.order_credit"}

type deadLetterAlerts struct {
	mu   sync.Mutex
	list []messaging.DeadLetterAlert
}

func (a *deadLetterAlerts) Alert(_ context.Context, alert messaging.DeadLetterAlert) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.list = append(a.list, alert)
}

func (a *deadLetterAlerts) of(id eventing.EventID) []messaging.DeadLetterAlert {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []messaging.DeadLetterAlert
	for _, x := range a.list {
		if x.EventID == id {
			out = append(out, x)
		}
	}
	return out
}

// orderEvents is a throwaway Bento outbox whose relay and replayer publish to a dispatcher.
type orderEvents struct {
	t          *testing.T
	platform   *bootstrap.Platform // on the throwaway schema
	db         *bentopg.DB
	store      *bentooutbox.Store
	dispatcher *messaging.Dispatcher
	alerts     *deadLetterAlerts
	copied     map[eventing.EventID]bool
}

func newOrderEvents(t *testing.T, subs ...messaging.Subscription) *orderEvents {
	t.Helper()
	migrator, app := pgtest.MigratorPool(t), pgtest.AppPool(t)
	schema := pgtest.SchemaFunc(t, migrator, app, "brant_t_stl_events_", func(name string) string {
		var b strings.Builder
		for _, m := range bentopg.Migrations() {
			sql, err := bentopg.RenderMigration(m.ID, name)
			if err != nil {
				t.Fatal(err)
			}
			b.WriteString(sql + ";\n")
		}
		b.WriteString("GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA {{schema}} TO {{app}};\n")
		b.WriteString("GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA {{schema}} TO {{app}}")
		return b.String()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	platform, err := bootstrap.Open(ctx, bootstrap.Config{PostgresDSN: pgtest.AppDSN(t), BentoSchema: schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	db, err := bentopg.NewDB(app, bentopg.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	store, err := bentooutbox.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	d, err := messaging.NewDispatcher(subs...)
	if err != nil {
		t.Fatal(err)
	}
	return &orderEvents{t: t, platform: platform, db: db, store: store, dispatcher: d, alerts: &deadLetterAlerts{},
		copied: map[eventing.EventID]bool{}}
}

func (e *orderEvents) enqueue(envelopes ...eventing.Envelope) {
	e.t.Helper()
	for _, env := range envelopes {
		if err := e.db.Transactor().WithinTransaction(context.Background(), func(ctx context.Context) error {
			return e.store.Enqueue(ctx, env)
		}); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *orderEvents) status(id eventing.EventID) string {
	e.t.Helper()
	q, err := e.db.ReadExecutor(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	var status string
	if err = q.QueryRow(context.Background(), `SELECT status FROM `+pgx.Identifier{e.db.Schema()}.Sanitize()+`.outbox WHERE event_id = $1`,
		string(id)).Scan(&status); err != nil {
		e.t.Fatal(err)
	}
	return status
}

// relay is the assembled relay of the throwaway outbox into the dispatcher; a delivery is
// dead-lettered after maxFailures failed attempts, a rejected one at once.
func (e *orderEvents) relay(maxFailures uint32) messaging.Relay {
	e.t.Helper()
	r, err := e.platform.Relay(bootstrap.RelayConfig{Publisher: e.dispatcher, Batch: 1, LeaseFor: 30 * time.Second, PublishTimeout: 20 * time.Second,
		MaxAttempts: 20, MaxFailures: maxFailures, RetryBase: time.Millisecond, RetryMax: time.Millisecond, IdleWait: time.Second,
		Alerter: e.alerts})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// once has r settle one delivery.
func (e *orderEvents) once(r messaging.Relay) {
	e.t.Helper()
	time.Sleep(5 * time.Millisecond) // past the retry delay
	if n, err := r.RunOnce(context.Background()); err != nil || n != 1 {
		e.t.Fatalf("RunOnce = %d, %v", n, err)
	}
}

// drain has r settle deliveries until it finds none.
func drain(r messaging.Relay) error {
	for range 200 {
		time.Sleep(2 * time.Millisecond)
		n, err := r.RunOnce(context.Background())
		if err != nil || n == 0 {
			return err
		}
	}
	return errors.New("the relay keeps claiming deliveries")
}

func (e *orderEvents) drain(r messaging.Relay) {
	e.t.Helper()
	if err := drain(r); err != nil {
		e.t.Fatal(err)
	}
}

func (e *orderEvents) published(envelopes ...eventing.Envelope) {
	e.t.Helper()
	for _, env := range envelopes {
		if s := e.status(env.ID); s != "PUBLISHED" {
			e.t.Fatalf("%s %s: outbox %s", env.Type, env.ID, s)
		}
	}
}

// replayer replays the throwaway outbox's dead letters into the dispatcher.
func (e *orderEvents) replayer() messaging.Replayer {
	e.t.Helper()
	r, err := e.platform.Replayer(bootstrap.ReplayConfig{Publisher: e.dispatcher, PublishTimeout: 20 * time.Second})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// copyFrom enqueues into the throwaway outbox, oldest first, the events of source in scope that
// the platform's Outbox holds and that were not copied before, unchanged: their IDs, partitions
// and payloads are the ones the producing context wrote.
func (e *orderEvents) copyFrom(scope kernel.Scope, source string) []eventing.Envelope {
	e.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(e.t))
	if err != nil {
		e.t.Fatal(err)
	}
	q, err := db.ReadExecutor(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	rows, err := q.Query(context.Background(), `SELECT event_id, event_type, event_version, spec_version, subject, partition_key, occurred_at,
		recorded_at, coalesce(correlation_id, ''), coalesce(causation_id, ''), coalesce(traceparent, ''), coalesce(tracestate, ''), content_type, payload
		FROM bento.outbox WHERE source = $1 AND scope = $2 ORDER BY enqueued_seq`, source, scope.String())
	if err != nil {
		e.t.Fatal(err)
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (eventing.Envelope, error) {
		env := eventing.Envelope{Source: source, Scope: scope.String()}
		var version int64
		var payload []byte
		err := row.Scan(&env.ID, &env.Type, &version, &env.SpecVersion, &env.Subject, &env.PartitionKey, &env.OccurredAt, &env.RecordedAt,
			&env.CorrelationID, &env.CausationID, &env.TraceParent, &env.TraceState, &env.ContentType, &payload)
		env.Version, env.Payload = eventing.EventVersion(version), json.RawMessage(payload)
		return env, err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []eventing.Envelope
	for _, env := range all {
		if !e.copied[env.ID] {
			e.copied[env.ID] = true
			out = append(out, env)
		}
	}
	e.enqueue(out...)
	return out
}

// salesOrderEvent is an event of TRD's SalesOrder aggregate as TRD's command ledger writes it (the
// ledger's members, then the order binding every order event carries, then fields), at the
// order's row version.
func salesOrderEvent(scope kernel.Scope, typ string, b stl.OrderBinding, version int64, status string, fields map[string]any) eventing.Envelope {
	now := kernel.Instant(time.Now())
	payload := map[string]any{"tenant_id": string(scope.Tenant), "company_id": string(scope.Company), "aggregate_type": "SalesOrder",
		"aggregate_id": b.OrderID, "aggregate_version": version, "actor_kind": "human", "actor_id": "author", "authority_epoch": 1,
		"action_id": action.NewID("act", now), "order_id": b.OrderID, "quote_id": "quote.1", "business_revision": 1,
		"business_hash": strings.Repeat("b", 64), "order_revision": b.OrderRevision, "order_hash": b.OrderHash, "order_status": status}
	for k, v := range fields {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return eventing.Envelope{SpecVersion: eventing.SpecVersionV1, ID: eventing.EventID(action.NewID("evt", now)), Source: trdapi.Source,
		Type: eventing.EventType(typ), Version: 1, Scope: scope.String(), Subject: b.OrderID, PartitionKey: scope.String() + "/SalesOrder:" + b.OrderID,
		OccurredAt: now, RecordedAt: now, CorrelationID: "corr.stl.order_events", ContentType: eventing.JSONContentType, Payload: body}
}

// activation is TRD's OrderActivated of order o at row version, activated now.
func activation(scope kernel.Scope, o stl.Order, version int64) (eventing.Envelope, time.Time) {
	at := kernel.Instant(time.Now())
	return salesOrderEvent(scope, trdapi.EventOrderActivated, o.Binding, version, "active", map[string]any{"number": "SO-1",
		"activated_at": kernel.FormatTimestamp(at)}), at
}

func (cw *creditWorld) reservation(id string) stlapi.ReservationView {
	cw.t.Helper()
	v, err := cw.stl.GetReservation(cw.officer, id)
	cw.must(err)
	return v
}

// stlEvents counts STL's Outbox events of type about reservation id.
func (w *world) stlEvents(typ, id string) int {
	n := 0
	for _, e := range w.events("company.A", stlapi.Source) {
		if e.Type == typ && e.Payload["reservation_id"] == id {
			n++
		}
	}
	return n
}

// breakable is subscription s whose handler fails while broken is set: before it runs (first)
// or after it ran to completion (otherwise), so its transaction rolls back.
func breakable(s messaging.Subscription, broken *atomic.Bool, first bool) messaging.Subscription {
	h := s.Handler
	s.Handler = func(ctx context.Context, e messaging.Event) error {
		if first && broken.Load() {
			return errors.New("consumer broken")
		}
		if err := h(ctx, e); err != nil {
			return err
		}
		if broken.Load() {
			return errors.New("consumer crashed after its commands")
		}
		return nil
	}
	return s
}

// PL-12, SM-STL-CREDIT-RESERVATION reserved → consumed (系统消费方处理 OrderActivated；按事件 ID 去重),
// S4-06a: TRD's OrderActivated of an order whose revision holds a reservation is relayed to
// stl.order_credit. Until the operator provisions system:inbox.stl.order_credit the consumer's
// credit.consume is denied and the delivery retried, the reservation untouched; once provisioned the
// reservation is consumed for the activation instant, as the consumer's system actor, once — a
// redelivery and a second dispatcher delivering the same event change nothing. Activations of
// several orders relayed by two relays at once, while a third worker delivers each of them
// directly (a worker whose lease expired), each consume their order's reservation exactly once.
func TestOrderActivationConsumesTheCreditReservationOnce(t *testing.T) {
	cw := newCreditWorld(t)
	cw.limit()
	ctx := context.Background()
	scope := cw.scope("company.A")
	o := cw.onCredit("order.1", "67200")
	_, err := cw.reserve(cw.sales, "res.1", o)
	cw.must(err)
	subs := cw.stl.Subscriptions()
	ev := newOrderEvents(t, subs...)

	cw.orders.setStatus("order.1", stl.OrderActive)
	activated, at := activation(scope, o, 5)
	ev.enqueue(activated)
	ev.once(ev.relay(5))
	if s, v := ev.status(activated.ID), cw.reservation("res.1"); s == "PUBLISHED" || v.Status != stlapi.ReservationReserved || v.Version != 1 {
		t.Fatalf("before provisioning: outbox %s, reservation %+v; want the consumer denied and the delivery retried", s, v)
	}
	if _, err = cw.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	ev.once(ev.relay(5))
	ev.published(activated)
	v := cw.reservation("res.1")
	if v.Status != stlapi.ReservationConsumed || v.Version != 2 || v.ConsumedBy != consumerActor || !v.ActivatedAt.Equal(at) ||
		v.OrderRevision != 1 || v.OrderHash != o.Binding.OrderHash || cw.stlEvents(stlapi.EventCreditReservationConsumed, "res.1") != 1 {
		t.Fatalf("after provisioning: %+v", v)
	}
	second, err := messaging.NewDispatcher(subs...)
	cw.must(err)
	for _, d := range []*messaging.Dispatcher{ev.dispatcher, second} {
		if err = d.Publish(ctx, activated); err != nil {
			t.Fatalf("redelivery: %v", err)
		}
	}
	if v := cw.reservation("res.1"); v.Version != 2 || cw.stlEvents(stlapi.EventCreditReservationConsumed, "res.1") != 1 {
		t.Fatalf("after redeliveries: %+v", v)
	}

	var events []eventing.Envelope
	for i := range 4 {
		id := fmt.Sprintf("order.w%d", i)
		oi := cw.onCredit(id, "10000")
		_, err := cw.reserve(cw.sales, "res.w"+fmt.Sprint(i), oi)
		cw.must(err)
		cw.orders.setStatus(id, stl.OrderActive)
		e, _ := activation(scope, oi, 5)
		events = append(events, e)
	}
	ev.enqueue(events...)
	var wg sync.WaitGroup
	errs := make([]error, 2+len(events))
	for i := range 2 {
		wg.Go(func() { errs[i] = drain(ev.relay(5)) })
	}
	for i, e := range events {
		wg.Go(func() {
			// Losing the race is a retry (eventing.ErrInboxBusy, or the entry's lock), never a rejection.
			if err := second.Publish(ctx, e); errors.Is(err, eventing.ErrPublishRejected) {
				errs[2+i] = err
			}
		})
	}
	wg.Wait()
	if err = errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	ev.drain(ev.relay(5))
	ev.published(events...)
	for i := range events {
		id := "res.w" + fmt.Sprint(i)
		if v := cw.reservation(id); v.Status != stlapi.ReservationConsumed || v.Version != 2 || v.ConsumedBy != consumerActor ||
			cw.stlEvents(stlapi.EventCreditReservationConsumed, id) != 1 {
			t.Fatalf("%s with two workers: %+v, %d consumed events", id, v, cw.stlEvents(stlapi.EventCreditReservationConsumed, id))
		}
	}
}

// PL-13, SM-STL-CREDIT-RESERVATION consumed → released (系统消费方处理 OrderCancelled、
// OrderRemainderClosed 或 OrderClosed；按事件 ID 去重) and reserved → released: the end of an order
// releases every reservation it holds, of every revision — revision 1's consumed on activation and
// the amended revision 2's still reserved — as the consumer's system actor, naming the event. A
// consumer that fails after its releases rolls them back with its transaction; the redelivery
// releases each once. A draft order closed unfulfilled releases its reservation; an order that
// holds none ends without effect.
func TestOrderEndReleasesEveryReservationOfTheOrder(t *testing.T) {
	cw := newCreditWorld(t)
	cw.limit()
	ctx := context.Background()
	scope := cw.scope("company.A")
	var broken atomic.Bool
	subs := cw.stl.Subscriptions()
	ev := newOrderEvents(t, breakable(subs[0], &broken, false))
	if _, err := cw.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}

	o := cw.onCredit("order.1", "67200")
	_, err := cw.reserve(cw.sales, "res.1", o)
	cw.must(err)
	cw.orders.setStatus("order.1", stl.OrderActive)
	activated, _ := activation(scope, o, 5)
	ev.enqueue(activated)
	ev.drain(ev.relay(5))
	if v := cw.reservation("res.1"); v.Status != stlapi.ReservationConsumed {
		t.Fatalf("activation: %+v", v)
	}
	amended := o
	amended.Binding = stl.OrderBinding{OrderID: "order.1", OrderRevision: 2, OrderHash: orderHashOf("order.1", 2)}
	amended.Status, amended.Stages, amended.CreditPortionMinor = stl.OrderActive, []stl.OrderStage{{StageID: "P2", Trigger: "after_shipment",
		Days: 30, Percent: "100", AmountMinor: "80000", OnCredit: true}}, "80000"
	cw.orders.put(amended)
	_, err = cw.reserve(cw.sales, "res.2", amended)
	cw.must(err)

	ended := salesOrderEvent(scope, trdapi.EventOrderRemainderClosed, amended.Binding, 9, "closed", map[string]any{"ended_from": "active",
		"end_command": trdapi.CommandCloseOrderRemainder})
	ev.enqueue(ended)
	broken.Store(true)
	ev.once(ev.relay(5))
	if s, v1, v2 := ev.status(ended.ID), cw.reservation("res.1"), cw.reservation("res.2"); s == "PUBLISHED" || v1.Status != stlapi.ReservationConsumed ||
		v1.Version != 2 || v2.Status != stlapi.ReservationReserved || v2.Version != 1 ||
		cw.stlEvents(stlapi.EventCreditReservationReleased, "res.1")+cw.stlEvents(stlapi.EventCreditReservationReleased, "res.2") != 0 {
		t.Fatalf("failed consumer: outbox %s, reservations %+v %+v; want everything rolled back", s, v1, v2)
	}
	broken.Store(false)
	ev.once(ev.relay(5))
	ev.published(ended)
	if err = ev.dispatcher.Publish(ctx, ended); err != nil {
		t.Fatal(err)
	}
	for id, version := range map[string]int64{"res.1": 3, "res.2": 2} {
		v := cw.reservation(id)
		if v.Status != stlapi.ReservationReleased || v.Version != version || v.ReleasedBy != consumerActor ||
			!strings.Contains(v.ReleaseReason, string(ended.ID)) || !strings.Contains(v.ReleaseReason, trdapi.EventOrderRemainderClosed) ||
			cw.stlEvents(stlapi.EventCreditReservationReleased, id) != 1 {
			t.Fatalf("%s after the end: %+v", id, v)
		}
	}

	draft := cw.onCredit("order.d", "5000")
	_, err = cw.reserve(cw.sales, "res.d", draft)
	cw.must(err)
	closed := salesOrderEvent(scope, trdapi.EventOrderClosedUnfulfilled, draft.Binding, 3, "closed_unfulfilled", map[string]any{"conversion_slot": "taken"})
	none := cw.onCredit("order.none", "5000")
	cancelled := salesOrderEvent(scope, trdapi.EventOrderCancelled, none.Binding, 2, "cancelled", map[string]any{"conversion_slot": "taken"})
	ev.enqueue(closed, cancelled)
	ev.drain(ev.relay(5))
	ev.published(closed, cancelled)
	if v := cw.reservation("res.d"); v.Status != stlapi.ReservationReleased || v.ReleasedBy != consumerActor || v.Version != 2 {
		t.Fatalf("draft closed unfulfilled: %+v", v)
	}
	if n := cw.count("company.A", "brant_stl.credit_reservation"); n != 3 {
		t.Fatalf("%d reservations; the end of an order without one made none", n)
	}
}

// ADR-BR-015, S4-06a (a dead-letter replay arrives after later events of its aggregate) and
// SM-STL-CREDIT-RESERVATION: an OrderActivated dead-lettered while the consumer was broken is
// replayed once and consumes the reservation; one whose order ended before the replay consumes
// nothing — the end released the reservation, and a released reservation is never consumed. An
// activation whose reservation a person released (the release ∥ activation race, A6 gap 1) is
// refused for good: dead-lettered at once with an alert naming stl.credit_reservation_released,
// nothing changed, and its replay refused.
func TestDeadLetteredOrderEventsReplayWithoutLateEffects(t *testing.T) {
	cw := newCreditWorld(t)
	cw.limit()
	ctx := context.Background()
	scope := cw.scope("company.A")
	var broken atomic.Bool
	subs := cw.stl.Subscriptions()
	ev := newOrderEvents(t, breakable(subs[0], &broken, true))
	if _, err := cw.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	operator := cw.provision(bootstrap.Grant{Scope: scope, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "operator"},
		Permissions: []string{messaging.ReplayPermission}})
	replayer := ev.replayer()
	deadLetter := func(e eventing.Envelope) {
		t.Helper()
		ev.enqueue(e)
		broken.Store(true)
		ev.once(ev.relay(1))
		broken.Store(false)
		if s, a := ev.status(e.ID), ev.alerts.of(e.ID); s != "ABANDONED" || len(a) != 1 || !a[0].Stored || a[0].Code != messaging.FailurePublishFailed {
			t.Fatalf("broken consumer: outbox %s, alerts %+v", s, a)
		}
	}

	r := cw.onCredit("order.r", "20000")
	_, err := cw.reserve(cw.sales, "res.r", r)
	cw.must(err)
	cw.orders.setStatus("order.r", stl.OrderActive)
	activatedR, _ := activation(scope, r, 5)
	deadLetter(activatedR)
	if v := cw.reservation("res.r"); v.Status != stlapi.ReservationReserved {
		t.Fatalf("dead-lettered activation changed %+v", v)
	}
	if _, err = replayer.Replay(operator, activatedR.Source, string(activatedR.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err = replayer.Replay(operator, activatedR.Source, string(activatedR.ID)); !errors.Is(err, messaging.ErrAlreadyReplayed) {
		t.Fatalf("second replay: %v", err)
	}
	if v := cw.reservation("res.r"); v.Status != stlapi.ReservationConsumed || v.ConsumedBy != consumerActor ||
		cw.stlEvents(stlapi.EventCreditReservationConsumed, "res.r") != 1 {
		t.Fatalf("replayed activation: %+v", v)
	}

	x := cw.onCredit("order.x", "20000")
	_, err = cw.reserve(cw.sales, "res.x", x)
	cw.must(err)
	cw.orders.setStatus("order.x", stl.OrderActive)
	activatedX, _ := activation(scope, x, 5)
	deadLetter(activatedX)
	cancelled := salesOrderEvent(scope, trdapi.EventOrderCancelled, x.Binding, 6, "cancelled", map[string]any{"ended_from": "active",
		"end_command": trdapi.CommandCancelOrder})
	ev.enqueue(cancelled)
	ev.drain(ev.relay(5))
	ev.published(cancelled)
	if v := cw.reservation("res.x"); v.Status != stlapi.ReservationReleased || v.ReleasedBy != consumerActor || v.Version != 2 {
		t.Fatalf("end before the replay: %+v", v)
	}
	if _, err = replayer.Replay(operator, activatedX.Source, string(activatedX.ID)); err != nil {
		t.Fatalf("replay after the end: %v", err)
	}
	if v := cw.reservation("res.x"); v.Status != stlapi.ReservationReleased || v.Version != 2 || v.ConsumedBy != (kernel.ActorRef{}) ||
		cw.stlEvents(stlapi.EventCreditReservationConsumed, "res.x") != 0 {
		t.Fatalf("replayed activation after the end consumed: %+v", v)
	}

	h := cw.onCredit("order.h", "20000")
	_, err = cw.reserve(cw.sales, "res.h", h)
	cw.must(err)
	_, err = cw.release(cw.officer, "res.h", 1)
	cw.must(err)
	cw.orders.setStatus("order.h", stl.OrderActive)
	activatedH, _ := activation(scope, h, 5)
	ev.enqueue(activatedH)
	ev.once(ev.relay(5))
	a := ev.alerts.of(activatedH.ID)
	if s := ev.status(activatedH.ID); s != "ABANDONED" || len(a) != 1 || !a[0].Stored || a[0].Code != messaging.FailurePublishRejected ||
		!strings.Contains(a[0].Error, "stl.credit_reservation_released") {
		t.Fatalf("activation without its reservation: outbox %s, alerts %+v", s, a)
	}
	if v := cw.reservation("res.h"); v.Status != stlapi.ReservationReleased || v.Version != 2 || v.ReleasedBy.ID != "officer.1" {
		t.Fatalf("refused activation changed %+v", v)
	}
	if _, err = replayer.Replay(operator, activatedH.Source, string(activatedH.ID)); !errors.Is(err, messaging.ErrReplayFailed) {
		t.Fatalf("replay of a refused activation: %v", err)
	}
}

// PL-12, PL-13 and TRD-INV-022 with TRD's own events (R32), relayed from TRD's Outbox: the order
// world's order, activated through the shared helpers, pays before shipment, so STL never reserved
// for it — its OrderActivated is settled and STL records nothing. Amended to 30 % on confirmation and
// 70 % 30 days after shipment (revision 2, 672.00 on credit, as TestCreditOverHTTPWithTRDOrders), it
// holds the sales clerk's reservation of revision 2, made for the revision the approved amendment
// proposed before it was applied (S5 D-9); TRD's OrderAmended, which names it, has the consumer
// consume it (unit C4b), and TRD's OrderCancelled has the consumer release it. Every TRD event,
// quotes and amendments included, is settled by the dispatcher of every composed context.
func TestTRDOrderEventsReachSTLThroughTheRelay(t *testing.T) {
	q := newOrderWorld(t)
	ctx := context.Background()
	scope := q.scope("company.A")
	policy := q.configure(q.author, q.reviewer, polapi.KindCreditPolicy, "credit.usd", 0, q.win.from, time.Time{}, e2Policy(), true)
	sales := q.assign("sales.1", "sales_clerk")
	m1, m2 := q.assign("manager.1", "credit_manager"), q.assign("manager.2", "credit_manager")
	subs := q.c.Subscriptions()
	ev := newOrderEvents(t, subs...)
	if _, err := q.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}

	o := q.activeOrder("quote.1")
	first := ev.copyFrom(scope, trdapi.Source)
	ev.drain(ev.relay(5))
	ev.published(first...)
	var sawActivation bool
	for _, e := range first {
		sawActivation = sawActivation || (e.Type == trdapi.EventOrderActivated && e.Subject == o.ObjectID)
	}
	if !sawActivation || q.count("company.A", "brant_stl.credit_reservation") != 0 || len(q.events("company.A", stlapi.Source)) != 0 {
		t.Fatalf("activation without credit: activation relayed %v, STL rows %d, events %d", sawActivation,
			q.count("company.A", "brant_stl.credit_reservation"), len(q.events("company.A", stlapi.Source)))
	}

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
	// The approved amendment grows the credit need: STL reserves the revision it proposes before
	// order.amendment.apply asks CreditGuard v1 for it (S5 D-9, unit B4).
	proposed := q.proposed(o.ObjectID, "amendment.pay")
	if proposed.Binding.OrderRevision != 2 || proposed.CreditPortionMinor != "67200" {
		t.Fatalf("TRD's proposed revision %+v", proposed)
	}

	_, err = q.c.STL.RequestCreditLimit(sales, stlapi.RequestCreditLimit{IdempotencyKey: q.key(), RequestID: "limit.1", PartyID: "customer.1",
		Currency: stlRef(q.usd), Sources: []stlapi.LimitSource{{Source: stlapi.SourceInternalAssessment, Amount: "10000.00", EvidenceIDs: []string{"evidence"}}},
		ValidFrom: kernel.FormatTimestamp(q.win.from), ValidUntil: kernel.FormatTimestamp(q.win.from.Add(365 * 24 * time.Hour)),
		CreditPolicy: stlRef(policy), Reason: creditReason})
	q.must(err)
	for i, m := range []context.Context{m1, m2} {
		_, err = q.c.STL.PublishCreditLimit(m, stlapi.PublishCreditLimit{IdempotencyKey: q.key(), RequestID: "limit.1", ExpectedVersion: int64(i + 1),
			Decision: stlapi.DecisionPublish, CreditPolicy: stlRef(policy), Reason: creditReason})
		q.must(err)
	}
	r, err := q.c.STL.ReserveCredit(sales, stlapi.ReserveCredit{IdempotencyKey: q.key(), ReservationID: "res.credit", OrderID: o.ObjectID,
		OrderRevision: 2, OrderHash: proposed.Binding.OrderHash, CreditPolicy: stlRef(policy)})
	if err != nil || r.Status != stlapi.ReservationReserved {
		t.Fatalf("reserve revision 2: %+v %v", r, err)
	}
	_, err = q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(o.ObjectID, "amendment.pay", dec.RowVersion))
	q.must(err)
	if snap := q.snapshots(q.author, o.ObjectID)[o.ObjectID]; snap.Binding != proposed.Binding || snap.CreditPortionMinor != "67200" {
		t.Fatalf("TRD's snapshot %+v", snap)
	}

	_, err = q.c.TRD.CancelOrder(q.author, trdapi.CancelOrder{IdempotencyKey: q.key(), OrderID: o.ObjectID, ExpectedVersion: q.orderVersion(o.ObjectID),
		Reason: "客户撤单", Evidence: q.proof})
	q.must(err)
	later := ev.copyFrom(scope, trdapi.Source)
	ev.drain(ev.relay(5))
	ev.published(later...)
	var cancelled eventing.Envelope
	for _, e := range later {
		if e.Type == trdapi.EventOrderCancelled {
			cancelled = e
		}
	}
	v, err := q.c.STL.GetReservation(sales, "res.credit")
	q.must(err)
	if cancelled.ID == "" || v.Status != stlapi.ReservationReleased || v.Version != 3 || v.ConsumedBy != consumerActor || v.ReleasedBy != consumerActor ||
		!strings.Contains(v.ReleaseReason, string(cancelled.ID)) || q.stlEvents(stlapi.EventCreditReservationConsumed, "res.credit") != 1 ||
		q.stlEvents(stlapi.EventCreditReservationReleased, "res.credit") != 1 {
		t.Fatalf("after TRD's OrderCancelled %s: %+v", cancelled.ID, v)
	}
}
