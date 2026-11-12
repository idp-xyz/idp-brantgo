//go:build integration

package messaging_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.idp.xyz/idp-bento-go/application"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	stlProbe       eventing.InboxConsumer = "stl.dispatch_probe"
	cnxProbe       eventing.InboxConsumer = "cnx.dispatch_probe"
	trdSource                             = "idp-brant.trd"
	orderActivated eventing.EventType     = "idp-brant.trd.order_activated.v1"
	orderAmended   eventing.EventType     = "idp-brant.trd.order_amended.v1"
)

// dispatchRig is in-process dispatch in miniature: the dlq fixture (a throwaway Bento schema with
// the Outbox and the Inbox, a scope whose operators may replay dead letters, a test clock and
// recorded alerts) and two consuming contexts, STL and CNX, each with an effect table under FORCE
// RLS in a schema of its own. STL subscribes to OrderActivated and OrderAmended, CNX to
// OrderActivated only.
type dispatchRig struct {
	*dlq
	effect map[eventing.InboxConsumer]string // schema-qualified effect table
	mu     sync.Mutex
	calls  []string // "<consumer> <event ID>" per handler run, in order
}

func newDispatchRig(t *testing.T) *dispatchRig {
	t.Helper()
	r := &dispatchRig{dlq: newDLQ(t), effect: map[eventing.InboxConsumer]string{}}
	migrator, app := pgtest.MigratorPool(t), pgtest.AppPool(t)
	for _, c := range []eventing.InboxConsumer{stlProbe, cnxProbe} {
		schema := pgtest.Schema(t, migrator, app, "brant_tdispatch_", `CREATE TABLE {{schema}}.effect (
    seq bigint GENERATED ALWAYS AS IDENTITY, tenant_id text NOT NULL, company_id text NOT NULL,
    event_id text NOT NULL, actor_id text NOT NULL);
ALTER TABLE {{schema}}.effect ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.effect FORCE ROW LEVEL SECURITY;
CREATE POLICY effect_scope ON {{schema}}.effect
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
GRANT SELECT, INSERT ON {{schema}}.effect TO {{app}}`)
		r.effect[c] = pgx.Identifier{schema, "effect"}.Sanitize()
	}
	return r
}

// handler writes one effect row through the consumer's executor, as the consumer's Principal and
// under RLS for the event's scope, records the run and then calls then.
func (r *dispatchRig) handler(consumer eventing.InboxConsumer, then func(context.Context, messaging.Event) error) messaging.EventFunc {
	return func(ctx context.Context, e messaging.Event) error {
		r.mu.Lock()
		r.calls = append(r.calls, string(consumer)+" "+string(e.Envelope.ID))
		r.mu.Unlock()
		if scope, ok := bento.ScopeFromContext(ctx); !ok || scope != e.Principal.Scope {
			return fmt.Errorf("handler outside the event's scope: %v", scope)
		}
		_, err := e.Exec.Exec(ctx, `INSERT INTO `+r.effect[consumer]+` (tenant_id, company_id, event_id, actor_id) VALUES ($1, $2, $3, $4)`,
			string(e.Principal.Scope.Tenant), string(e.Principal.Scope.Company), string(e.Envelope.ID), e.Principal.Actor.ID)
		if err != nil || then == nil {
			return err
		}
		return then(ctx, e)
	}
}

func (r *dispatchRig) runs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// effects returns the event of each effect row of consumer in the order written, and the actors.
func (r *dispatchRig) effects(consumer eventing.InboxConsumer) (events, actors []string) {
	r.t.Helper()
	r.inScope(r.scope, func(ctx context.Context, exec bentopg.Executor) error {
		rows, err := exec.Query(ctx, `SELECT event_id, actor_id FROM `+r.effect[consumer]+` ORDER BY seq`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e, a string
			if err := rows.Scan(&e, &a); err != nil {
				return err
			}
			events, actors = append(events, e), append(actors, a)
		}
		return rows.Err()
	})
	return events, actors
}

// worker is one worker process: a pool of its own, a transaction owner for the relay and one for
// each consuming context, and the dispatcher over the contexts' subscriptions.
type worker struct {
	db         *bentopg.DB
	store      *bentooutbox.Store
	dispatcher *messaging.Dispatcher
}

func (r *dispatchRig) worker(stl, cnx messaging.EventFunc) *worker {
	r.t.Helper()
	pool := pgtest.AppPool(r.t)
	newDB := func() *bentopg.DB {
		db, err := bentopg.NewDB(pool, bentopg.WithSchema(r.db.Schema()))
		if err != nil {
			r.t.Fatal(err)
		}
		return db
	}
	d, err := messaging.NewDispatcher(
		messaging.Subscription{Consumer: stlProbe, Source: trdSource, EventTypes: []eventing.EventType{orderActivated, orderAmended},
			DB: newDB(), Handler: stl},
		messaging.Subscription{Consumer: cnxProbe, Source: trdSource, EventTypes: []eventing.EventType{orderActivated},
			DB: newDB(), Handler: cnx})
	if err != nil {
		r.t.Fatal(err)
	}
	db := newDB()
	return &worker{db: db, store: newStore(r.t, db), dispatcher: d}
}

func (w *worker) relay(r *dispatchRig, publisher eventing.Publisher, clock application.Clock, maxFailures uint32) messaging.Relay {
	relay := testRelay(w.db, w.store, publisher, clock, maxFailures)
	relay.Alerter = r.alerts
	return relay
}

// event is an event of TRD's sales order aggregate in the rig's scope; the partition is the
// aggregate's, as the command ledger writes it.
func (r *dispatchRig) event(id string, typ eventing.EventType, order string, version int) eventing.Envelope {
	at := deadAt.Add(-time.Hour)
	return eventing.Envelope{
		SpecVersion: eventing.SpecVersion, ID: eventing.EventID(id), Source: trdSource, Type: typ, Version: 1,
		Scope: r.scope.String(), Subject: order, PartitionKey: r.scope.String() + "/SalesOrder:" + order,
		OccurredAt: at, RecordedAt: at, CorrelationID: "corr.dispatch", ContentType: eventing.JSONContentType,
		Payload: json.RawMessage(fmt.Sprintf(`{"aggregate_type":"SalesOrder","aggregate_id":%q,"aggregate_version":%d}`, order, version)),
	}
}

// letter reads the state and failure code of the dead letter of a TRD event.
func (r *dispatchRig) letter(id eventing.EventID) (state, code string) {
	r.t.Helper()
	r.inScope(r.scope, func(ctx context.Context, exec bentopg.Executor) error {
		err := exec.QueryRow(ctx, `SELECT state, failure_code FROM brant_msg.dead_letter WHERE source = $1 AND event_id = $2`,
			trdSource, string(id)).Scan(&state, &code)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return state, code
}

// loseAnswer passes each envelope to next and loses the answer of the first delivery of lost, as
// a crash after the consumers committed and before the relay marked the delivery published would.
type loseAnswer struct {
	next eventing.Publisher
	lost eventing.EventID
	done atomic.Bool
}

func (p *loseAnswer) Publish(ctx context.Context, e eventing.Envelope) error {
	if err := p.next.Publish(ctx, e); err != nil {
		return err
	}
	if e.ID == p.lost && p.done.CompareAndSwap(false, true) {
		return errors.New("answer lost")
	}
	return nil
}

// The relay hands each event to every consumer that subscribes to its type, each in its context's
// own transaction as system:inbox.<consumer>. A consumer that fails after writing is redelivered
// without a double effect; a delivery whose answer was lost is redelivered and deduplicated; and a
// failing event holds up the later events of its aggregate, so each consumer sees them in order.
func TestDispatchDeliversEveryEventToEachConsumerOnceAndInOrder(t *testing.T) {
	r := newDispatchRig(t)
	var stlFailed atomic.Bool
	w := r.worker(r.handler(stlProbe, func(_ context.Context, e messaging.Event) error {
		if e.Envelope.ID == "evt-order1-v1" && stlFailed.CompareAndSwap(false, true) {
			return errors.New("credit store unavailable")
		}
		return nil
	}), r.handler(cnxProbe, nil))
	first, second, other := r.event("evt-order1-v1", orderActivated, "order.1", 1), r.event("evt-order1-v2", orderAmended, "order.1", 2),
		r.event("evt-order2-v1", orderActivated, "order.2", 1)
	enqueueAll(t, w.db, w.store, first, second, other)
	relay := w.relay(r, &loseAnswer{next: w.dispatcher, lost: other.ID}, r.clock, 5)

	runOnce(t, relay, 2) // the heads of both orders: STL fails on order.1, the answer for order.2 is lost
	if s := readOutbox(t, w.db, first.ID); s.published || s.failures != 1 {
		t.Fatalf("order.1 v1 after the failure: %+v", s)
	}
	r.clock.now = deadAt.Add(500 * time.Millisecond) // before the retry is due
	runOnce(t, relay, 0)                             // order.1 v2 waits behind its head
	r.clock.now = deadAt.Add(3 * time.Second)
	runOnce(t, relay, 2) // order.1 v1 again (STL processes, CNX duplicate) and order.2 v1 again (both duplicates)
	r.clock.now = deadAt.Add(4 * time.Second)
	runOnce(t, relay, 1) // order.1 v2 for STL only
	for _, e := range []eventing.Envelope{first, second, other} {
		if !readOutbox(t, w.db, e.ID).published {
			t.Fatalf("%s was not published", e.ID)
		}
	}

	stlEvents, stlActors := r.effects(stlProbe)
	cnxEvents, cnxActors := r.effects(cnxProbe)
	if !slices.Equal(stlEvents, []string{"evt-order2-v1", "evt-order1-v1", "evt-order1-v2"}) ||
		!slices.Equal(cnxEvents, []string{"evt-order1-v1", "evt-order2-v1"}) {
		t.Fatalf("effects STL %v, CNX %v; want each event once, order.1 in its order", stlEvents, cnxEvents)
	}
	if slices.ContainsFunc(stlActors, func(a string) bool { return a != "inbox.stl.dispatch_probe" }) ||
		slices.ContainsFunc(cnxActors, func(a string) bool { return a != "inbox.cnx.dispatch_probe" }) {
		t.Fatalf("effects written by %v and %v; want each consumer's system actor", stlActors, cnxActors)
	}
	want := []string{"stl.dispatch_probe evt-order1-v1", "cnx.dispatch_probe evt-order1-v1", "stl.dispatch_probe evt-order2-v1",
		"cnx.dispatch_probe evt-order2-v1", "stl.dispatch_probe evt-order1-v1", "stl.dispatch_probe evt-order1-v2"}
	if got := r.runs(); !slices.Equal(got, want) {
		t.Fatalf("handler runs %v, want %v", got, want)
	}
	if alerts := r.alerts.all(); len(alerts) != 0 {
		t.Fatalf("alerts %+v", alerts)
	}
}

// Two worker processes deliver the same event: the first worker's lease expires while its STL
// handler holds the inbox entry, the second re-claims the delivery, finds STL busy, delivers to
// CNX and retries. Each handler runs to completion once, whatever settles the delivery.
func TestDispatchAcrossTwoWorkersRunsEachHandlerOnce(t *testing.T) {
	r := newDispatchRig(t)
	holding, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	block := func(ctx context.Context, _ messaging.Event) error {
		once.Do(func() { close(holding) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	first := r.worker(r.handler(stlProbe, block), r.handler(cnxProbe, nil))
	second := r.worker(r.handler(stlProbe, nil), r.handler(cnxProbe, nil))
	e := r.event("evt-two-workers", orderActivated, "order.1", 1)
	enqueueAll(t, first.db, first.store, e)

	firstClock, secondClock := &testClock{now: deadAt}, &testClock{now: deadAt.Add(31 * time.Second)} // beyond the 30 s lease
	type result struct {
		claimed int
		err     error
	}
	done := make(chan result, 1)
	firstRelay := first.relay(r, first.dispatcher, firstClock, 5)
	firstRelay.Batch, firstRelay.PublishTimeout = 1, 20*time.Second // the handler holds its entry while the second worker runs
	go func() {
		n, err := firstRelay.RunOnce(context.Background())
		done <- result{n, err}
	}()
	select {
	case <-holding:
	case <-time.After(30 * time.Second):
		t.Fatal("the first worker never reached the STL handler")
	}
	secondRelay := second.relay(r, second.dispatcher, secondClock, 5)
	runOnce(t, secondRelay, 1) // re-claimed: STL is busy, CNX processes, the delivery is retried
	if s := readOutbox(t, first.db, e.ID); s.published || s.attempt != 2 || s.failures != 1 {
		close(release)
		t.Fatalf("after the second worker: %+v, want attempt 2 with one failure", s)
	}
	close(release)
	if res := <-done; res.err != nil || res.claimed != 1 {
		t.Fatalf("first worker: %d claimed, %v; want its lost lease taken as settled", res.claimed, res.err)
	}
	secondClock.now = secondClock.now.Add(5 * time.Second)
	runOnce(t, secondRelay, 1) // both duplicates
	if s := readOutbox(t, first.db, e.ID); !s.published {
		t.Fatalf("delivery not published: %+v", s)
	}
	stlEvents, _ := r.effects(stlProbe)
	cnxEvents, _ := r.effects(cnxProbe)
	if len(stlEvents) != 1 || len(cnxEvents) != 1 {
		t.Fatalf("effects STL %v, CNX %v; want one each", stlEvents, cnxEvents)
	}
	if got, want := r.runs(), []string{"stl.dispatch_probe evt-two-workers", "cnx.dispatch_probe evt-two-workers"}; !slices.Equal(got, want) {
		t.Fatalf("handler runs %v, want %v", got, want)
	}
}

// A consumer that keeps failing spends the budget and the delivery is dead-lettered with an alert,
// which unblocks its aggregate: the aggregate's next event is delivered. After the fix a human
// replays the dead letter through the dispatcher; it reaches that consumer once, after the later
// event (IG-06: out of order, so a handler that depends on order compares aggregate versions),
// while the consumer that processed it sees a duplicate. Concurrent replays reach it once too. A
// rejection is dead-lettered at once and its replay is refused without running the handler.
func TestDispatchDeadLetterReplayReachesTheConsumerOnce(t *testing.T) {
	r := newDispatchRig(t)
	var broken atomic.Bool
	broken.Store(true)
	var versions []int // aggregate versions of order.1 in the order STL processed them
	w := r.worker(r.handler(stlProbe, func(_ context.Context, e messaging.Event) error {
		switch {
		case e.Envelope.ID == "evt-o3-v2":
			return messaging.RejectEvent("stl.order_unknown", errors.New("no reservation for order.3"))
		case e.Envelope.Type == orderActivated && broken.Load():
			return errors.New("credit store misconfigured")
		}
		if e.Envelope.Subject == "order.1" {
			var p struct {
				Version int `json:"aggregate_version"`
			}
			if err := json.Unmarshal(e.Envelope.Payload, &p); err != nil {
				return err
			}
			versions = append(versions, p.Version)
		}
		return nil
	}), r.handler(cnxProbe, nil))
	failed, later := r.event("evt-o1-v1", orderActivated, "order.1", 1), r.event("evt-o1-v2", orderAmended, "order.1", 2)
	concurrent, rejected := r.event("evt-o2-v1", orderActivated, "order.2", 1), r.event("evt-o3-v2", orderAmended, "order.3", 2)
	enqueueAll(t, w.db, w.store, failed, later, concurrent, rejected)
	relay := w.relay(r, w.dispatcher, r.clock, 2)
	runOnce(t, relay, 3) // failure 1 of 2 for both activations; the amendment of order.3 is rejected and dead-lettered
	if state, code := r.letter(rejected.ID); state != "dead" || code != string(messaging.FailurePublishRejected) {
		t.Fatalf("rejected event: dead letter %q, %q", state, code)
	}
	r.clock.now = deadAt.Add(3 * time.Second)
	runOnce(t, relay, 2) // failure 2 of 2: both activations dead-lettered
	for _, id := range []eventing.EventID{failed.ID, concurrent.ID} {
		if state, code := r.letter(id); state != "dead" || code != string(messaging.FailurePublishFailed) {
			t.Fatalf("%s: dead letter %q, %q", id, state, code)
		}
	}
	runOnce(t, relay, 1) // order.1 v2, no longer behind its dead head
	if !readOutbox(t, w.db, later.ID).published {
		t.Fatal("the event after a dead letter was not delivered")
	}
	if alerts := r.alerts.all(); len(alerts) != 3 {
		t.Fatalf("alerts %+v, want one per dead letter", alerts)
	}

	broken.Store(false)
	replayer := r.replayer(w.dispatcher)
	if _, err := replayer.Replay(as(t, operator, r.scope), trdSource, string(failed.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := replayer.Replay(as(t, operator, r.scope), trdSource, string(failed.ID)); !errors.Is(err, messaging.ErrAlreadyReplayed) {
		t.Fatalf("second replay: %v", err)
	}
	if !slices.Equal(versions, []int{2, 1}) {
		t.Fatalf("STL processed order.1 versions %v, want 2 and then the replayed 1", versions)
	}
	if _, err := replayer.Replay(as(t, operator, r.scope), trdSource, string(rejected.ID)); !errors.Is(err, messaging.ErrReplayFailed) ||
		!errors.Is(err, eventing.ErrPublishRejected) {
		t.Fatalf("replay of a rejected event: %v", err)
	}
	if state, _ := r.letter(rejected.ID); state != "dead" {
		t.Fatalf("the rejected event's letter is %q", state)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, op := range []kernel.ActorRef{operator, operator2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = replayer.Replay(as(t, op, r.scope), trdSource, string(concurrent.ID))
		}()
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case !errors.Is(err, messaging.ErrAlreadyReplayed) && !errors.Is(err, messaging.ErrReplayFailed):
			t.Fatalf("concurrent replay: %v", err)
		}
	}
	if state, _ := r.letter(concurrent.ID); succeeded == 0 || state != "replayed" {
		t.Fatalf("%d concurrent replays succeeded, letter %q", succeeded, state)
	}

	stlEvents, _ := r.effects(stlProbe)
	cnxEvents, _ := r.effects(cnxProbe)
	if !slices.Equal(stlEvents, []string{"evt-o1-v2", "evt-o1-v1", "evt-o2-v1"}) || !slices.Equal(cnxEvents, []string{"evt-o1-v1", "evt-o2-v1"}) {
		t.Fatalf("effects STL %v, CNX %v; want each event once", stlEvents, cnxEvents)
	}
	rejectedRuns := 0
	for _, run := range r.runs() {
		if run == "stl.dispatch_probe evt-o3-v2" {
			rejectedRuns++
		}
	}
	if rejectedRuns != 1 {
		t.Fatalf("the rejecting handler ran %d times, want once", rejectedRuns)
	}
}

// Prepare reads an upstream context's transaction owner before the consumer's transaction
// begins; the same read inside the handler fails with ErrTransactionConflict (rule T3).
func TestDispatchPrepareReadsUpstreamOutsideTheConsumerTransaction(t *testing.T) {
	r := newDispatchRig(t)
	upstream, err := bentopg.NewDB(pgtest.AppPool(t), bentopg.WithSchema(r.db.Schema()))
	if err != nil {
		t.Fatal(err)
	}
	read := func(ctx context.Context) (int, error) {
		var n int
		err := upstream.Transactor().WithinTransaction(ctx, func(ctx context.Context) error {
			exec, err := upstream.RequireExecutor(ctx)
			if err != nil {
				return err
			}
			return exec.QueryRow(ctx, `SELECT 42`).Scan(&n)
		})
		return n, err
	}
	var inside error
	stl := r.handler(stlProbe, func(ctx context.Context, e messaging.Event) error {
		if e.Prepared != 42 {
			return fmt.Errorf("prepared %v", e.Prepared)
		}
		_, inside = read(ctx)
		return nil
	})
	db, err := bentopg.NewDB(pgtest.AppPool(t), bentopg.WithSchema(r.db.Schema()))
	if err != nil {
		t.Fatal(err)
	}
	d, err := messaging.NewDispatcher(messaging.Subscription{
		Consumer: stlProbe, Source: trdSource, EventTypes: []eventing.EventType{orderActivated}, DB: db, Handler: stl,
		Prepare: func(ctx context.Context, _ eventing.Envelope) (any, error) { return read(ctx) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Publish(context.Background(), r.event("evt-prepared", orderActivated, "order.1", 1)); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(inside, application.ErrTransactionConflict) {
		t.Fatalf("upstream read inside the consumer's transaction: %v; want ErrTransactionConflict", inside)
	}
	if events, _ := r.effects(stlProbe); !slices.Equal(events, []string{"evt-prepared"}) {
		t.Fatalf("effects %v", events)
	}
}
