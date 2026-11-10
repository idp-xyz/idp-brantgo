//go:build integration

package messaging_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	operator  = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "operator"}
	operator2 = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "operator.2"}
	opsAgent  = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.ops"}
	opsSystem = kernel.ActorRef{Kind: kernel.ActorSystem, ID: "system.ops"}
	revokedOp = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "operator.revoked"}
	deadAt    = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
)

func uniqueScope(t *testing.T) kernel.Scope {
	t.Helper()
	return kernel.Scope{Tenant: kernel.TenantID(pgtest.UniqueID(t, "tenant.")), Company: kernel.CompanyID(pgtest.UniqueID(t, "company."))}
}

type recordingAlerter struct {
	mu     sync.Mutex
	alerts []messaging.DeadLetterAlert
}

func (a *recordingAlerter) Alert(_ context.Context, alert messaging.DeadLetterAlert) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts = append(a.alerts, alert)
}

func (a *recordingAlerter) all() []messaging.DeadLetterAlert {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]messaging.DeadLetterAlert(nil), a.alerts...)
}

// dlq is a throwaway Bento outbox schema plus a fresh scope in brant_msg and brant_auth, in which
// the operators, the AI agent and the system actor all hold dead_letter.replay.
type dlq struct {
	t      *testing.T
	db     *bentopg.DB
	store  *bentooutbox.Store
	scope  kernel.Scope
	clock  *testClock
	alerts *recordingAlerter
}

func newDLQ(t *testing.T) *dlq {
	t.Helper()
	db := outboxDB(t)
	f := &dlq{t: t, db: db, store: newStore(t, db), scope: uniqueScope(t), clock: &testClock{now: deadAt}, alerts: &recordingAlerter{}}
	f.grant(f.scope, operator, operator2, opsAgent, opsSystem, revokedOp)
	f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := authorization.Grants{}.Revoke(ctx, exec, f.scope, revokedOp.ID, deadAt)
		return err
	})
	return f
}

func (f *dlq) inScope(scope kernel.Scope, fn func(context.Context, bentopg.Executor) error) {
	f.t.Helper()
	if err := f.tryInScope(scope, fn); err != nil {
		f.t.Fatal(err)
	}
}

func (f *dlq) tryInScope(scope kernel.Scope, fn func(context.Context, bentopg.Executor) error) error {
	return bento.ScopeTransactor{DB: f.db, Scope: scope}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := f.db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return fn(ctx, exec)
	})
}

func (f *dlq) grant(scope kernel.Scope, actors ...kernel.ActorRef) {
	f.t.Helper()
	f.inScope(scope, func(ctx context.Context, exec bentopg.Executor) error {
		g := authorization.Grants{}
		if err := g.Provision(ctx, exec, scope); err != nil {
			return err
		}
		for _, a := range actors {
			if _, err := g.Put(ctx, exec, scope, a, []string{messaging.ReplayPermission}, deadAt); err != nil {
				return err
			}
		}
		return nil
	})
}

func (f *dlq) epoch(scope kernel.Scope) int64 {
	f.t.Helper()
	var epoch int64
	f.inScope(scope, func(ctx context.Context, exec bentopg.Executor) error {
		return exec.QueryRow(ctx, `SELECT authority_epoch FROM brant_auth.authority_scope WHERE tenant_id = $1 AND company_id = $2`,
			string(scope.Tenant), string(scope.Company)).Scan(&epoch)
	})
	return epoch
}

func (f *dlq) probe(id, eventType string) eventing.Envelope {
	e := probe(id, "partition-"+id, deadAt.Add(-time.Hour))
	e.Scope = f.scope.String()
	if eventType != "" {
		e.Type = eventing.EventType(eventType)
	}
	return e
}

func (f *dlq) relay(publisher eventing.Publisher, maxFailures uint32) messaging.Relay {
	r := testRelay(f.db, f.store, publisher, f.clock, maxFailures)
	r.Alerter = f.alerts
	return r
}

// bury enqueues e and lets a sink that rejects it permanently turn it into a dead letter.
func (f *dlq) bury(e eventing.Envelope) {
	f.t.Helper()
	enqueueAll(f.t, f.db, f.store, e)
	reject := eventing.PublisherFunc(func(context.Context, eventing.Envelope) error {
		return fmt.Errorf("%w: sink answered 422", eventing.ErrPublishRejected)
	})
	runOnce(f.t, f.relay(reject, 5), 1)
	if _, ok := f.deadLetter(f.scope, e.ID); !ok {
		f.t.Fatalf("%s was not dead-lettered", e.ID)
	}
}

type deadLetterRow struct {
	EventType, FailureCode, State     string
	Envelope                          []byte
	Attempt, Failures, ReplayAttempts int64
	LastError, LastReplayError        *string
	Critical                          bool
	DeadAt                            time.Time
	ReplayedAt                        *time.Time
	ReplayedByKind, ReplayedByID      *string
	ReplayEpoch                       *int64
}

func (f *dlq) deadLetter(scope kernel.Scope, id eventing.EventID) (deadLetterRow, bool) {
	f.t.Helper()
	var r deadLetterRow
	found := true
	f.inScope(scope, func(ctx context.Context, exec bentopg.Executor) error {
		err := exec.QueryRow(ctx, `SELECT event_type, failure_code, state, envelope, attempt, failures, replay_attempts,
			last_error, last_replay_error, critical, dead_at, replayed_at, replayed_by_kind, replayed_by_id, replay_epoch
			FROM brant_msg.dead_letter WHERE source = $1 AND event_id = $2`, "idp-brant.relay-probe", string(id)).
			Scan(&r.EventType, &r.FailureCode, &r.State, &r.Envelope, &r.Attempt, &r.Failures, &r.ReplayAttempts,
				&r.LastError, &r.LastReplayError, &r.Critical, &r.DeadAt, &r.ReplayedAt, &r.ReplayedByKind, &r.ReplayedByID, &r.ReplayEpoch)
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
			return nil
		}
		return err
	})
	return r, found
}

func (f *dlq) replayer(publisher eventing.Publisher) messaging.Replayer {
	db, err := bentopg.NewDB(pgtest.AppPool(f.t)) // a transaction owner of its own
	if err != nil {
		f.t.Fatal(err)
	}
	return messaging.Replayer{DB: db, Publisher: publisher, Clock: f.clock, PublishTimeout: 5 * time.Second}
}

func as(t *testing.T, actor kernel.ActorRef, scope kernel.Scope) context.Context {
	t.Helper()
	ctx, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: actor, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}

// recordingSink answers each envelope POST with the status next returns and records the
// Idempotency-Key and the envelope.
type recordingSink struct {
	*httptest.Server
	mu        sync.Mutex
	keys      []string
	envelopes []eventing.Envelope
}

func newSink(t *testing.T, next func(n int) int) *recordingSink {
	t.Helper()
	s := &recordingSink{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e eventing.Envelope
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.keys = append(s.keys, r.Header.Get("Idempotency-Key"))
		s.envelopes = append(s.envelopes, e)
		n := len(s.keys)
		s.mu.Unlock()
		w.WriteHeader(next(n))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *recordingSink) received() ([]string, []eventing.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.keys...), append([]eventing.Envelope(nil), s.envelopes...)
}

// (a) The failure that spends the budget abandons the delivery and stores its dead letter, with
// the original envelope, attempts, failures and the failure code, and raises one alert.
func TestBudgetExhaustionStoresDeadLetter(t *testing.T) {
	f := newDLQ(t)
	event := f.probe("dlq-budget", "")
	enqueueAll(t, f.db, f.store, event)
	relay := f.relay(eventing.PublisherFunc(func(context.Context, eventing.Envelope) error {
		return errors.New("sink unavailable")
	}), 2)
	runOnce(t, relay, 1) // failure 1 of 2
	if _, found := f.deadLetter(f.scope, event.ID); found || len(f.alerts.all()) != 0 {
		t.Fatal("a failure within budget must not dead-letter")
	}
	f.clock.now = f.clock.now.Add(time.Minute)
	runOnce(t, relay, 1) // failure 2 spends the budget

	if s := readOutbox(t, f.db, event.ID); !s.abandoned || s.failures != 2 || s.attempt != 2 {
		t.Fatalf("outbox state %+v, want abandoned after 2 failures", s)
	}
	row, found := f.deadLetter(f.scope, event.ID)
	if !found || row.State != "dead" || row.Attempt != 2 || row.Failures != 2 || row.FailureCode != string(messaging.FailurePublishFailed) ||
		row.LastError == nil || !strings.Contains(*row.LastError, "sink unavailable") || row.Critical ||
		row.EventType != string(event.Type) || !row.DeadAt.Equal(kernel.Instant(f.clock.now)) || row.ReplayAttempts != 0 {
		t.Fatalf("dead letter %+v (found %v)", row, found)
	}
	want, _ := json.Marshal(event)
	if !sameJSON(t, row.Envelope, want) {
		t.Fatalf("stored envelope %s, want the original %s", row.Envelope, want)
	}
	var stored eventing.Envelope
	if err := json.Unmarshal(row.Envelope, &stored); err != nil || stored.ID != event.ID || !sameJSON(t, stored.Payload, event.Payload) {
		t.Fatalf("stored envelope does not decode to the original: %+v, %v", stored, err)
	}
	alerts := f.alerts.all()
	if len(alerts) != 1 || alerts[0].EventID != event.ID || !alerts[0].Stored || alerts[0].Critical || alerts[0].Failures != 2 {
		t.Fatalf("alerts %+v, want one stored, non-critical alert", alerts)
	}
}

// (a) The abandonment and the dead letter commit together: Bento's Abandon joins the dead-letter
// transaction, so a failure before that transaction commits leaves the delivery leased, and it is
// dead-lettered when it comes back.
func TestAbandonmentCommitsWithItsDeadLetter(t *testing.T) {
	f := newDLQ(t)
	event := f.probe("dlq-atomic", "")
	enqueueAll(t, f.db, f.store, event)
	reject := eventing.PublisherFunc(func(context.Context, eventing.Envelope) error {
		return fmt.Errorf("%w: sink answered 400", eventing.ErrPublishRejected)
	})
	relay := f.relay(reject, 5)
	injected := errors.New("injected failure after abandon")
	relay.DeadLetters = buryThenFail{inner: messaging.DeadLetterStore{DB: f.db}, err: injected}
	if _, err := relay.RunOnce(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("RunOnce = %v, want the injected failure", err)
	}
	if s := readOutbox(t, f.db, event.ID); s.abandoned || s.attempt != 1 {
		t.Fatalf("outbox state %+v: the abandonment must roll back with its dead letter", s)
	}
	if _, found := f.deadLetter(f.scope, event.ID); found || len(f.alerts.all()) != 0 {
		t.Fatal("nothing may be stored or alerted for a rolled-back abandonment")
	}

	f.clock.now = f.clock.now.Add(time.Minute) // the lease has expired
	runOnce(t, f.relay(reject, 5), 1)
	if s := readOutbox(t, f.db, event.ID); !s.abandoned || s.attempt != 2 {
		t.Fatalf("outbox state %+v, want abandoned on attempt 2", s)
	}
	if row, found := f.deadLetter(f.scope, event.ID); !found || row.Attempt != 2 || row.FailureCode != string(messaging.FailurePublishRejected) {
		t.Fatalf("dead letter %+v (found %v)", row, found)
	}
}

// buryThenFail runs the real Bury with a settle that abandons and then fails, so the transaction
// that holds the abandonment rolls back.
type buryThenFail struct {
	inner messaging.DeadLetterWriter
	err   error
}

func (b buryThenFail) Bury(ctx context.Context, letter messaging.DeadLetter, settle func(context.Context) error) error {
	return b.inner.Bury(ctx, letter, func(ctx context.Context) error {
		if err := settle(ctx); err != nil {
			return err
		}
		return b.err
	})
}

// Bento abandons a delivery at Claim's attempt limit without telling the caller. The relay keeps
// that limit out of reach and dead-letters a delivery claimed beyond its own budget instead.
func TestDeliveryBeyondAttemptBudgetIsDeadLettered(t *testing.T) {
	f := newDLQ(t)
	event := f.probe("dlq-attempts", "brant.pty.qualification_revoked")
	enqueueAll(t, f.db, f.store, event)
	// A worker claims the delivery and dies: the lease expires unsettled.
	claimed, err := f.store.Claim(context.Background(), eventing.OutboxClaim{Now: f.clock.now, Limit: 1, LeaseFor: time.Second, MaxAttempts: 1})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim = %v, %v", claimed, err)
	}
	f.clock.now = f.clock.now.Add(time.Minute)
	var sent int
	relay := f.relay(eventing.PublisherFunc(func(context.Context, eventing.Envelope) error { sent++; return nil }), 5)
	relay.MaxAttempts = 1
	runOnce(t, relay, 1)
	if sent != 0 {
		t.Fatal("a delivery beyond its attempt budget must not be published")
	}
	if s := readOutbox(t, f.db, event.ID); !s.abandoned || s.attempt != 2 {
		t.Fatalf("outbox state %+v, want abandoned on attempt 2", s)
	}
	row, found := f.deadLetter(f.scope, event.ID)
	if !found || row.FailureCode != string(eventing.FailureClaimLimitExceeded) || row.Attempt != 2 || !row.Critical {
		t.Fatalf("dead letter %+v (found %v), want a critical claim-limit dead letter", row, found)
	}
	if a := f.alerts.all(); len(a) != 1 || !a[0].Critical || !a[0].Stored {
		t.Fatalf("alerts %+v, want one critical alert", a)
	}
}

// (b) FORCE RLS keeps a dead letter inside its tenant and company: another scope neither reads,
// writes nor replays it, and the application role cannot delete it or change its envelope.
func TestDeadLettersAreIsolatedByScope(t *testing.T) {
	f := newDLQ(t)
	event := f.probe("dlq-rls", "")
	f.bury(event)
	otherTenant := kernel.Scope{Tenant: kernel.TenantID(pgtest.UniqueID(t, "tenant.")), Company: f.scope.Company}
	otherCompany := kernel.Scope{Tenant: f.scope.Tenant, Company: kernel.CompanyID(pgtest.UniqueID(t, "company."))}
	replayer := f.replayer(eventing.PublisherFunc(func(context.Context, eventing.Envelope) error {
		t.Error("a replay from another scope must not publish")
		return nil
	}))
	for _, other := range []kernel.Scope{otherTenant, otherCompany} {
		f.grant(other, operator)
		if _, found := f.deadLetter(other, event.ID); found {
			t.Fatalf("%s reads the dead letter of %s", other, f.scope)
		}
		f.inScope(other, func(ctx context.Context, exec bentopg.Executor) error {
			tag, err := exec.Exec(ctx, `UPDATE brant_msg.dead_letter SET last_replay_error = 'x' WHERE event_id = $1`, string(event.ID))
			if err != nil || tag.RowsAffected() != 0 {
				t.Errorf("%s updates a foreign dead letter: %v, %d rows", other, err, tag.RowsAffected())
			}
			return nil
		})
		err := f.tryInScope(other, func(ctx context.Context, exec bentopg.Executor) error {
			_, err := exec.Exec(ctx, `INSERT INTO brant_msg.dead_letter (tenant_id, company_id, source, event_id, event_type, envelope,
				attempt, failures, failure_code, critical, dead_at) VALUES ($1, $2, 's', 'forged', 't', '{}', 1, 0, 'x', false, now())`,
				string(f.scope.Tenant), string(f.scope.Company))
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s writes into %s: %v, want SQLSTATE 42501", other, f.scope, err)
		}
		if _, err := replayer.Replay(as(t, operator, other), "idp-brant.relay-probe", string(event.ID)); !errors.Is(err, messaging.ErrDeadLetterNotFound) {
			t.Errorf("replay from %s: %v, want ErrDeadLetterNotFound", other, err)
		}
	}
	q, err := f.db.ReadExecutor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var visible int
	if err = q.QueryRow(context.Background(), `SELECT count(*) FROM brant_msg.dead_letter`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("without a scope %d dead letters are visible (%v), want 0", visible, err)
	}
	for _, sql := range []string{
		`DELETE FROM brant_msg.dead_letter WHERE event_id = $1`,
		`UPDATE brant_msg.dead_letter SET envelope = '{}'::jsonb WHERE event_id = $1`,
		`UPDATE brant_msg.dead_letter SET failure_code = 'changed' WHERE event_id = $1`,
	} {
		err := f.tryInScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
			_, err := exec.Exec(ctx, sql, string(event.ID))
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s: %v, want SQLSTATE 42501", sql, err)
		}
	}
	if row, found := f.deadLetter(f.scope, event.ID); !found || row.State != "dead" || row.LastReplayError != nil {
		t.Fatalf("the dead letter changed: %+v (found %v)", row, found)
	}
}

// (c) A human with the grant replays a dead letter with its original event ID; a failed publish
// keeps it dead and counts the attempt; AI, system, revoked and unauthenticated callers are refused.
func TestHumanReplayRepublishesWithSameEventID(t *testing.T) {
	f := newDLQ(t)
	event := f.probe("dlq-replay", "brant.pty.qualification_revoked")
	f.bury(event)
	if row, _ := f.deadLetter(f.scope, event.ID); !row.Critical {
		t.Fatal("a revocation dead letter must be critical")
	}
	sink := newSink(t, func(n int) int {
		if n == 1 {
			return http.StatusServiceUnavailable
		}
		return http.StatusNoContent
	})
	replayer := f.replayer(messaging.Webhook{URL: sink.URL})
	replay := func(ctx context.Context, id string) (messaging.ReplayResult, error) {
		return replayer.Replay(ctx, "idp-brant.relay-probe", id)
	}

	for _, refused := range []struct {
		ctx  context.Context
		want error
	}{
		{as(t, opsAgent, f.scope), authorization.ErrActorKindDenied},
		{as(t, opsSystem, f.scope), authorization.ErrActorKindDenied},
		{as(t, revokedOp, f.scope), authorization.ErrForbidden},
		{as(t, kernel.ActorRef{Kind: kernel.ActorHuman, ID: "no.grant"}, f.scope), authorization.ErrForbidden},
		{context.Background(), identity.ErrUnauthenticated},
	} {
		if _, err := replay(refused.ctx, string(event.ID)); !errors.Is(err, refused.want) {
			t.Errorf("replay = %v, want %v", err, refused.want)
		}
	}
	if keys, _ := sink.received(); len(keys) != 0 {
		t.Fatalf("a refused replay reached the sink: %v", keys)
	}

	f.clock.now = deadAt.Add(time.Hour)
	if _, err := replay(as(t, operator, f.scope), string(event.ID)); !errors.Is(err, messaging.ErrReplayFailed) || !strings.Contains(err.Error(), "503") {
		t.Fatalf("replay against a failing sink = %v, want ErrReplayFailed", err)
	}
	row, _ := f.deadLetter(f.scope, event.ID)
	if row.State != "dead" || row.ReplayAttempts != 1 || row.LastReplayError == nil || !strings.Contains(*row.LastReplayError, "503") || row.ReplayedAt != nil {
		t.Fatalf("after a failed replay: %+v, want dead with one attempt and the error", row)
	}

	f.clock.now = deadAt.Add(2 * time.Hour)
	result, err := replay(as(t, operator, f.scope), string(event.ID))
	if err != nil {
		t.Fatal(err)
	}
	epoch := f.epoch(f.scope)
	if result.EventID != event.ID || result.ReplayedBy != operator || result.AuthorityEpoch != epoch || result.Attempts != 2 ||
		!result.ReplayedAt.Equal(kernel.Instant(f.clock.now)) {
		t.Fatalf("result %+v, want operator at epoch %d on attempt 2", result, epoch)
	}
	row, _ = f.deadLetter(f.scope, event.ID)
	if row.State != "replayed" || row.ReplayAttempts != 2 || row.ReplayedAt == nil || !row.ReplayedAt.Equal(result.ReplayedAt) ||
		*row.ReplayedByKind != "human" || *row.ReplayedByID != operator.ID || row.ReplayEpoch == nil || *row.ReplayEpoch != epoch {
		t.Fatalf("after the replay: %+v", row)
	}
	keys, envelopes := sink.received()
	if len(keys) != 2 || keys[0] != string(event.ID) || keys[1] != string(event.ID) {
		t.Fatalf("Idempotency-Keys %v, want the event ID twice", keys)
	}
	if got := envelopes[1]; got.ID != event.ID || got.Type != event.Type || got.Scope != event.Scope || !sameJSON(t, got.Payload, event.Payload) {
		t.Fatalf("replayed envelope %+v, want the original", got)
	}

	if _, err := replay(as(t, operator, f.scope), string(event.ID)); !errors.Is(err, messaging.ErrAlreadyReplayed) {
		t.Fatalf("second replay = %v, want ErrAlreadyReplayed", err)
	}
	if _, err := replay(as(t, operator, f.scope), "no-such-event"); !errors.Is(err, messaging.ErrDeadLetterNotFound) {
		t.Fatalf("unknown event = %v, want ErrDeadLetterNotFound", err)
	}
	if keys, _ := sink.received(); len(keys) != 2 {
		t.Fatalf("refused replays reached the sink: %v", keys)
	}
}

// (d) Two operators replay the same dead letter at once. Both publish, with the same
// Idempotency-Key, so the consumer drops one; exactly one replay is recorded.
func TestConcurrentReplaysAreHarmless(t *testing.T) {
	f := newDLQ(t)
	event := f.probe("dlq-concurrent", "brant.pol.risk_flagged")
	f.bury(event)
	both := make(chan struct{})
	var once sync.Once
	sink := newSink(t, func(n int) int {
		if n == 2 {
			once.Do(func() { close(both) })
		}
		select { // hold each publish until both replays are publishing
		case <-both:
		case <-time.After(10 * time.Second):
		}
		return http.StatusNoContent
	})
	replayer := f.replayer(messaging.Webhook{URL: sink.URL})
	type outcome struct {
		result messaging.ReplayResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, ctx := range []context.Context{as(t, operator, f.scope), as(t, operator2, f.scope)} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := replayer.Replay(ctx, "idp-brant.relay-probe", string(event.ID))
			outcomes <- outcome{r, err}
		}()
	}
	wg.Wait()
	close(outcomes)
	var won []messaging.ReplayResult
	var lost int
	for o := range outcomes {
		switch {
		case o.err == nil:
			won = append(won, o.result)
		case errors.Is(o.err, messaging.ErrAlreadyReplayed):
			lost++
		default:
			t.Fatalf("replay failed: %v", o.err)
		}
	}
	if len(won) != 1 || lost != 1 {
		t.Fatalf("%d replays recorded and %d refused, want one of each", len(won), lost)
	}
	keys, _ := sink.received()
	if len(keys) != 2 || keys[0] != string(event.ID) || keys[1] != string(event.ID) {
		t.Fatalf("Idempotency-Keys %v, want the event ID from both replays", keys)
	}
	row, _ := f.deadLetter(f.scope, event.ID)
	if row.State != "replayed" || row.ReplayAttempts != 1 || *row.ReplayedByID != won[0].ReplayedBy.ID {
		t.Fatalf("dead letter %+v, want one replay by %s", row, won[0].ReplayedBy)
	}
}
