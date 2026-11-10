//go:build integration

package messaging_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-bento-go/postgres/inbox"
	"go.idp.xyz/idp-bento-go/testkit"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const inboxConsumer eventing.InboxConsumer = "inq.test_projection"

// inboxRig is a consuming context in miniature: a throwaway Bento schema with the inbox, an effect
// table under FORCE RLS in a schema of its own, and the context's DB.
type inboxRig struct {
	t      *testing.T
	db     *bentopg.DB
	store  *inbox.Store
	effect string // the effect table, schema-qualified
	scope  kernel.Scope
	calls  atomic.Int32
}

func newInboxRig(t *testing.T) *inboxRig {
	t.Helper()
	db := outboxDB(t)
	migrator, app := pgtest.MigratorPool(t), pgtest.AppPool(t)
	schema := pgtest.Schema(t, migrator, app, "brant_tinbox_", `CREATE TABLE {{schema}}.effect (
    tenant_id text NOT NULL, company_id text NOT NULL, event_id text NOT NULL, actor_id text NOT NULL);
ALTER TABLE {{schema}}.effect ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.effect FORCE ROW LEVEL SECURITY;
CREATE POLICY effect_scope ON {{schema}}.effect
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
GRANT SELECT, INSERT ON {{schema}}.effect TO {{app}}`)
	store, err := inbox.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return &inboxRig{t: t, db: db, store: store, effect: pgx.Identifier{schema, "effect"}.Sanitize(), scope: uniqueScope(t)}
}

// worker returns another transaction owner on its own pool and the same Bento schema: a second
// worker process of the consuming context.
func (r *inboxRig) worker() *bentopg.DB {
	r.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(r.t), bentopg.WithSchema(r.db.Schema()))
	if err != nil {
		r.t.Fatal(err)
	}
	return db
}

// handler writes one effect row per run on db, as the Principal the consumer put in ctx and under
// RLS for its scope, and then calls then.
func (r *inboxRig) handler(db *bentopg.DB, then func(context.Context, eventing.Envelope) error) messaging.EventHandler {
	return messaging.EventHandlerFunc(func(ctx context.Context, e eventing.Envelope) error {
		r.calls.Add(1)
		p, err := identity.FromContext(ctx)
		if err != nil {
			return err
		}
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		_, err = exec.Exec(ctx, `INSERT INTO `+r.effect+` VALUES ($1, $2, $3, $4)`,
			string(p.Scope.Tenant), string(p.Scope.Company), string(e.ID), p.Actor.ID)
		if err != nil || then == nil {
			return err
		}
		return then(ctx, e)
	})
}

func (r *inboxRig) consumer(db *bentopg.DB, h messaging.EventHandler) messaging.Consumer {
	r.t.Helper()
	c, err := messaging.NewConsumer(db, inboxConsumer, h)
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

func (r *inboxRig) envelope(id string) eventing.Envelope {
	at := time.Now().UTC().Add(-time.Minute)
	return eventing.Envelope{
		SpecVersion: eventing.SpecVersion, ID: eventing.EventID(id), Source: "idp-brant.pty", Type: "idp-brant.pty.party_confirmed.v1",
		Version: 1, Scope: r.scope.String(), Subject: "party.1", PartitionKey: r.scope.String() + "/Party:party.1",
		OccurredAt: at, RecordedAt: at, CorrelationID: "corr.inbox", ContentType: eventing.JSONContentType,
		Payload: json.RawMessage(`{"party_id":"party.1","row_version":2}`),
	}
}

// effects returns the actor of each effect row of id, read under RLS in the rig's scope.
func (r *inboxRig) effects(id eventing.EventID) []string {
	r.t.Helper()
	var actors []string
	err := bento.ScopeTransactor{DB: r.db, Scope: r.scope}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := r.db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		rows, err := exec.Query(ctx, `SELECT actor_id FROM `+r.effect+` WHERE event_id = $1`, string(id))
		if err != nil {
			return err
		}
		actors, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return actors
}

var errPeek = errors.New("peek only")

// status reads the inbox entry of id through the Bento store, never by SQL: Start in a transaction
// that rolls back. ok is false when there is no settled entry.
func (r *inboxRig) status(id eventing.EventID) (status eventing.InboxStatus, ok bool) {
	r.t.Helper()
	var res eventing.InboxStartResult
	err := r.db.Transactor().WithinTransaction(context.Background(), func(ctx context.Context) error {
		var err error
		if res, err = r.store.Start(ctx, eventing.InboxKey{Consumer: inboxConsumer, Source: "idp-brant.pty", EventID: id}, time.Now()); err != nil {
			return err
		}
		return errPeek
	})
	if !errors.Is(err, errPeek) {
		r.t.Fatal(err)
	}
	return res.Status, !res.Acquired
}

func mustEncode(t *testing.T, e eventing.Envelope) []byte {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A redelivered event, by Consume or by a relay POST with the same Idempotency-Key, runs the
// handler once; the handler acts as system:inbox.<consumer> in the event's scope.
func TestInboxRunsHandlerOnceForRedeliveredEvent(t *testing.T) {
	r := newInboxRig(t)
	c := r.consumer(r.db, r.handler(r.db, nil))
	e := r.envelope("evt-dup")
	for i, want := range []messaging.InboxOutcome{messaging.InboxProcessed, messaging.InboxDuplicate} {
		if got, err := c.Consume(context.Background(), e); got != want || err != nil {
			t.Fatalf("delivery %d: %s, %v; want %s", i+1, got, err, want)
		}
	}
	for range 2 {
		rec := c.Receive(context.Background(), string(e.ID), mustEncode(t, e))
		if rec.Verdict != messaging.VerdictDelivered || rec.Status != http.StatusOK || rec.Outcome != messaging.InboxDuplicate || rec.Err != nil {
			t.Fatalf("relay POST of a processed event: %+v", rec)
		}
	}
	if actors := r.effects(e.ID); r.calls.Load() != 1 || !slices.Equal(actors, []string{"inbox.inq.test_projection"}) {
		t.Fatalf("handler ran %d times, effects by %v; want once by inbox.inq.test_projection", r.calls.Load(), actors)
	}
	if s, ok := r.status(e.ID); !ok || s != eventing.InboxProcessed {
		t.Fatalf("inbox entry %d, %v; want processed", s, ok)
	}
}

// failAfterMark marks the entry processed in the transaction and then fails, as a failure after
// the handler would.
type failAfterMark struct{ messaging.InboxStore }

var errAfterMark = errors.New("injected failure after MarkProcessed")

func (f failAfterMark) MarkProcessed(ctx context.Context, r eventing.InboxReceipt, at time.Time) error {
	if err := f.InboxStore.MarkProcessed(ctx, r, at); err != nil {
		return err
	}
	return errAfterMark
}

// The handler's effect and the inbox record commit together: a failure after the handler, or in
// it after its write, rolls back both, and the redelivery runs the handler again.
func TestInboxCommitsHandlerEffectAndRecordTogether(t *testing.T) {
	r := newInboxRig(t)
	failing := true
	c := r.consumer(r.db, r.handler(r.db, func(context.Context, eventing.Envelope) error {
		if failing {
			return errors.New("injected handler failure after its write")
		}
		return nil
	}))
	e := r.envelope("evt-atomic")
	if out, err := c.Consume(context.Background(), e); out != messaging.InboxRetry || err == nil {
		t.Fatalf("handler failure: %s, %v; want a retry", out, err)
	}
	failing = false
	working := c.Inbox
	c.Inbox = failAfterMark{working}
	if out, err := c.Consume(context.Background(), e); out != messaging.InboxRetry || !errors.Is(err, errAfterMark) {
		t.Fatalf("failure after MarkProcessed: %s, %v; want a retry", out, err)
	}
	if actors := r.effects(e.ID); len(actors) != 0 {
		t.Fatalf("effects %v survived a rolled-back delivery", actors)
	}
	if s, ok := r.status(e.ID); ok {
		t.Fatalf("inbox entry %d survived a rolled-back delivery", s)
	}
	c.Inbox = working
	if out, err := c.Consume(context.Background(), e); out != messaging.InboxProcessed || err != nil {
		t.Fatalf("redelivery: %s, %v", out, err)
	}
	if n := len(r.effects(e.ID)); n != 1 || r.calls.Load() != 3 {
		t.Fatalf("%d effects after %d runs; want 1 after 3", n, r.calls.Load())
	}
}

// recordingInbox records the failure code of each rejection it passes on.
type recordingInbox struct {
	messaging.InboxStore
	mu    sync.Mutex
	codes []eventing.FailureCode
}

func (r *recordingInbox) MarkRejected(ctx context.Context, rc eventing.InboxReceipt, at time.Time, code eventing.FailureCode) error {
	r.mu.Lock()
	r.codes = append(r.codes, code)
	r.mu.Unlock()
	return r.InboxStore.MarkRejected(ctx, rc, at, code)
}

// A permanent rejection is recorded with its failure code, leaves no effect and is never retried:
// a redelivery or a relay POST is refused without running the handler.
func TestInboxRecordsPermanentRejectionAndNeverRetries(t *testing.T) {
	r := newInboxRig(t)
	c := r.consumer(r.db, r.handler(r.db, func(context.Context, eventing.Envelope) error {
		return messaging.RejectEvent("pty.unknown_party", errors.New("party.1 is not a customer here"))
	}))
	recording := &recordingInbox{InboxStore: c.Inbox}
	c.Inbox = recording
	e := r.envelope("evt-reject")
	if out, err := c.Consume(context.Background(), e); out != messaging.InboxRejected || !errors.Is(err, messaging.ErrEventRejected) {
		t.Fatalf("%s, %v; want a permanent rejection", out, err)
	}
	if s, ok := r.status(e.ID); !ok || s != eventing.InboxRejected {
		t.Fatalf("inbox entry %d, %v; want rejected", s, ok)
	}
	if actors := r.effects(e.ID); len(actors) != 0 || !slices.Equal(recording.codes, []eventing.FailureCode{"pty.unknown_party"}) {
		t.Fatalf("effects %v, codes %v; want no effect and pty.unknown_party", actors, recording.codes)
	}
	if out, err := c.Consume(context.Background(), e); out != messaging.InboxRejected || !errors.Is(err, messaging.ErrEventRejected) {
		t.Fatalf("redelivery: %s, %v", out, err)
	}
	if rec := c.Receive(context.Background(), string(e.ID), mustEncode(t, e)); rec.Verdict != messaging.VerdictRejected ||
		rec.Status != http.StatusUnprocessableEntity {
		t.Fatalf("relay POST of a rejected event: %+v", rec)
	}
	if r.calls.Load() != 1 || len(recording.codes) != 1 {
		t.Fatalf("handler ran %d times, %d rejections recorded; want 1 and 1", r.calls.Load(), len(recording.codes))
	}
}

// Two workers on their own pools deliver the same event: while the first runs the handler, the
// second gets ErrInboxBusy and runs nothing; afterwards it sees the event processed.
func TestInboxConcurrentWorkersRunHandlerOnce(t *testing.T) {
	r := newInboxRig(t)
	other := r.worker()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	block := func(ctx context.Context, _ eventing.Envelope) error {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	first := r.consumer(r.db, r.handler(r.db, block))
	second := r.consumer(other, r.handler(other, block))
	e := r.envelope("evt-two-workers")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var firstOut messaging.InboxOutcome
	var firstErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		firstOut, firstErr = first.Consume(ctx, e)
	}()
	<-started
	out, err := second.Consume(ctx, e)
	if out != messaging.InboxRetry || !errors.Is(err, eventing.ErrInboxBusy) {
		close(release)
		<-done
		t.Fatalf("second worker while the first holds the entry: %s, %v; want ErrInboxBusy", out, err)
	}
	close(release)
	<-done
	if firstOut != messaging.InboxProcessed || firstErr != nil {
		t.Fatalf("first worker: %s, %v", firstOut, firstErr)
	}
	if out, err = second.Consume(ctx, e); out != messaging.InboxDuplicate || err != nil {
		t.Fatalf("second worker afterwards: %s, %v; want a duplicate", out, err)
	}
	if n := len(r.effects(e.ID)); n != 1 || r.calls.Load() != 1 {
		t.Fatalf("handler ran %d times with %d effects; want once", r.calls.Load(), n)
	}
}

// Eight deliveries of one event race on two workers, each retrying while the entry is busy:
// exactly one runs the handler.
func TestInboxRacingDeliveriesRunHandlerOnce(t *testing.T) {
	r := newInboxRig(t)
	dbs := []*bentopg.DB{r.db, r.worker()}
	e := r.envelope("evt-race")
	const deliveries = 8
	outcomes := make([]messaging.InboxOutcome, deliveries)
	errs := make([]error, deliveries)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := range deliveries {
		db := dbs[i%len(dbs)]
		c := r.consumer(db, r.handler(db, func(context.Context, eventing.Envelope) error {
			time.Sleep(20 * time.Millisecond)
			return nil
		}))
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			for attempt := 0; attempt < 500; attempt++ {
				outcomes[i], errs[i] = c.Consume(context.Background(), e)
				if outcomes[i] != messaging.InboxRetry || !errors.Is(errs[i], eventing.ErrInboxBusy) {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	close(gate)
	wg.Wait()
	processed := 0
	for i := range outcomes {
		switch {
		case errs[i] != nil:
			t.Fatalf("delivery %d: %s, %v", i, outcomes[i], errs[i])
		case outcomes[i] == messaging.InboxProcessed:
			processed++
		case outcomes[i] != messaging.InboxDuplicate:
			t.Fatalf("delivery %d: %s", i, outcomes[i])
		}
	}
	if n := len(r.effects(e.ID)); processed != 1 || n != 1 || r.calls.Load() != 1 {
		t.Fatalf("%d processed, %d effects, %d handler runs; want 1, 1, 1", processed, n, r.calls.Load())
	}
}

// An envelope whose scope does not parse cannot run under RLS: it is rejected permanently, with
// no inbox entry and without running the handler.
func TestInboxRejectsEnvelopeWhoseScopeDoesNotParse(t *testing.T) {
	r := newInboxRig(t)
	c := r.consumer(r.db, r.handler(r.db, nil))
	e := r.envelope("evt-unscoped")
	e.Scope = "tenant-without-company"
	if out, err := c.Consume(context.Background(), e); out != messaging.InboxRejected || !errors.Is(err, messaging.ErrEventRejected) ||
		!errors.Is(err, kernel.ErrInvalidScope) {
		t.Fatalf("%s, %v; want a permanent rejection for the scope", out, err)
	}
	if rec := c.Receive(context.Background(), string(e.ID), mustEncode(t, e)); rec.Verdict != messaging.VerdictRejected ||
		rec.Status != http.StatusUnprocessableEntity {
		t.Fatalf("relay POST: %+v", rec)
	}
	if s, ok := r.status(e.ID); ok || r.calls.Load() != 0 {
		t.Fatalf("inbox entry %d (%v), %d handler runs; want none", s, ok, r.calls.Load())
	}
}

// Bento's inbox keeps its portable contract under Brant's scope transactor.
func TestInboxStoreKeepsBentoContractUnderScopeTransactor(t *testing.T) {
	testkit.RunInboxContract(t, func(t *testing.T) testkit.InboxContractScenario {
		db := outboxDB(t)
		store, err := inbox.NewStore(db)
		if err != nil {
			t.Fatal(err)
		}
		return testkit.InboxContractScenario{
			Transactor: bento.ScopeTransactor{DB: db, Scope: uniqueScope(t)}, Starter: store, Completer: store,
			Key: eventing.InboxKey{Consumer: inboxConsumer, Source: "idp-brant.pty", EventID: "evt-contract"},
			Now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC),
		}
	})
}

// The relay's webhook reaches the consumer through Receive: a delivery is processed once even when
// it is sent again, a transient handler failure is retried by the sender, and a rejection is
// permanent for the sender too.
func TestRelayDeliversToInboxThroughWebhook(t *testing.T) {
	r := newInboxRig(t)
	var transient atomic.Bool
	c := r.consumer(r.db, r.handler(r.db, func(_ context.Context, e eventing.Envelope) error {
		switch {
		case e.ID == "evt-refused":
			return messaging.RejectEvent("pty.unknown_party", nil)
		case transient.Load():
			return errors.New("upstream unavailable")
		}
		return nil
	}))
	var statuses []int
	var mu sync.Mutex
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(io.LimitReader(req.Body, messaging.MaxEnvelopeBytes+1))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rec := c.Receive(req.Context(), req.Header.Get("Idempotency-Key"), body)
		mu.Lock()
		statuses = append(statuses, rec.Status)
		mu.Unlock()
		w.WriteHeader(rec.Status)
	}))
	t.Cleanup(sink.Close)
	webhook := messaging.Webhook{URL: sink.URL}

	producer := outboxDB(t)
	store := newStore(t, producer)
	e := r.envelope("evt-relayed")
	enqueueAll(t, producer, store, e)
	runOnce(t, testRelay(producer, store, webhook, bento.SystemClock{}, 3), 1)
	if !readOutbox(t, producer, e.ID).published {
		t.Fatal("the relay did not settle the delivery as published")
	}
	// A crash between publish and settle re-delivers the same envelope.
	if err := webhook.Publish(context.Background(), e); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if n := len(r.effects(e.ID)); n != 1 || r.calls.Load() != 1 {
		t.Fatalf("%d effects after %d runs; want one", n, r.calls.Load())
	}

	transient.Store(true)
	retried := r.envelope("evt-transient")
	if err := webhook.Publish(context.Background(), retried); err == nil || errors.Is(err, eventing.ErrPublishRejected) {
		t.Fatalf("transient failure: %v; want an error the relay retries", err)
	}
	transient.Store(false)
	if err := webhook.Publish(context.Background(), retried); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := webhook.Publish(context.Background(), r.envelope("evt-refused")); !errors.Is(err, eventing.ErrPublishRejected) {
		t.Fatalf("refused event: %v; want ErrPublishRejected", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []int{200, 200, 503, 200, 422}; !slices.Equal(statuses, want) {
		t.Fatalf("sink answered %v, want %v", statuses, want)
	}
}
