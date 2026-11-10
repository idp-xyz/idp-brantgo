package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-bento-go/testkit"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// fakeInboxDB is an inbox and a table of handler effects with transactions: a transaction works
// on a copy that replaces the committed state only when its function returns nil.
type fakeInboxDB struct {
	committed fakeInboxState
	scopes    []kernel.Scope // the scope of each transaction
	commits   int
	rollbacks int
	busy      int   // Start fails with ErrInboxBusy this many times
	markErr   error // MarkProcessed records the entry, then fails with markErr
	codes     []eventing.FailureCode
}

type fakeInboxState struct {
	entries map[eventing.InboxKey]eventing.InboxStatus
	effects []eventing.EventID
}

type fakeInboxTxKey struct{}

func newFakeInboxDB() *fakeInboxDB {
	return &fakeInboxDB{committed: fakeInboxState{entries: map[eventing.InboxKey]eventing.InboxStatus{}}}
}

type transactorFunc func(ctx context.Context, fn application.TxFunc) error

func (f transactorFunc) WithinTransaction(ctx context.Context, fn application.TxFunc) error {
	return f(ctx, fn)
}

func (f *fakeInboxDB) transactor(scope kernel.Scope) application.Transactor {
	return transactorFunc(func(ctx context.Context, fn application.TxFunc) error {
		f.scopes = append(f.scopes, scope)
		work := &fakeInboxState{entries: maps.Clone(f.committed.entries), effects: slices.Clone(f.committed.effects)}
		if err := fn(context.WithValue(ctx, fakeInboxTxKey{}, work)); err != nil {
			f.rollbacks++
			return err
		}
		f.committed = *work
		f.commits++
		return nil
	})
}

func fakeTx(ctx context.Context) *fakeInboxState {
	tx, ok := ctx.Value(fakeInboxTxKey{}).(*fakeInboxState)
	if !ok {
		panic("fake inbox used outside a transaction")
	}
	return tx
}

func (f *fakeInboxDB) Start(ctx context.Context, key eventing.InboxKey, _ time.Time) (eventing.InboxStartResult, error) {
	tx := fakeTx(ctx)
	if f.busy > 0 {
		f.busy--
		return eventing.InboxStartResult{}, fmt.Errorf("inbox: %w", eventing.ErrInboxBusy)
	}
	if status, ok := tx.entries[key]; ok {
		return eventing.InboxStartResult{Status: status}, nil
	}
	tx.entries[key] = eventing.InboxProcessing
	return eventing.InboxStartResult{Receipt: eventing.InboxReceipt{Key: key}, Status: eventing.InboxProcessing, Acquired: true}, nil
}

func (f *fakeInboxDB) MarkProcessed(ctx context.Context, r eventing.InboxReceipt, _ time.Time) error {
	fakeTx(ctx).entries[r.Key] = eventing.InboxProcessed
	return f.markErr
}

func (f *fakeInboxDB) MarkRejected(ctx context.Context, r eventing.InboxReceipt, _ time.Time, code eventing.FailureCode) error {
	fakeTx(ctx).entries[r.Key] = eventing.InboxRejected
	f.codes = append(f.codes, code)
	return nil
}

// effect is what a handler writes: one row per run, in the delivery's transaction.
func effect(ctx context.Context, e eventing.Envelope) {
	tx := fakeTx(ctx)
	tx.effects = append(tx.effects, e.ID)
}

var inboxScope = kernel.Scope{Tenant: "tenant.inbox", Company: "company.inbox"}

const testConsumer eventing.InboxConsumer = "inq.test_projection"

func inboxEnvelope(id string) eventing.Envelope {
	e := testkit.ValidEnvelope()
	e.ID, e.Source, e.Scope, e.CorrelationID = eventing.EventID(id), "idp-brant.pty", inboxScope.String(), "corr.inbox-1"
	return e
}

func testConsumerOn(db *fakeInboxDB, handler EventHandlerFunc) Consumer {
	return Consumer{Name: testConsumer, Inbox: db, Transactor: db.transactor, Handler: handler,
		Clock: testkit.NewFixedClock(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))}
}

func (f *fakeInboxDB) status(id string) (eventing.InboxStatus, bool) {
	s, ok := f.committed.entries[eventing.InboxKey{Consumer: testConsumer, Source: "idp-brant.pty", EventID: eventing.EventID(id)}]
	return s, ok
}

func TestConsumerRunsHandlerOnceAndDeduplicatesByEventID(t *testing.T) {
	db := newFakeInboxDB()
	calls := 0
	c := testConsumerOn(db, func(ctx context.Context, e eventing.Envelope) error {
		calls++
		effect(ctx, e)
		return nil
	})
	for i, want := range []InboxOutcome{InboxProcessed, InboxDuplicate, InboxDuplicate} {
		if got, err := c.Consume(context.Background(), inboxEnvelope("evt-1")); got != want || err != nil {
			t.Fatalf("delivery %d: %s, %v; want %s", i+1, got, err, want)
		}
	}
	if calls != 1 || len(db.committed.effects) != 1 {
		t.Fatalf("handler ran %d times, %d effects; want once", calls, len(db.committed.effects))
	}
	if s, _ := db.status("evt-1"); s != eventing.InboxProcessed {
		t.Fatalf("inbox status %d", s)
	}
	if !slices.Equal(db.scopes, []kernel.Scope{inboxScope, inboxScope, inboxScope}) {
		t.Fatalf("transaction scopes %v", db.scopes)
	}
}

func TestConsumerProcessesAsItsSystemActorInTheEventScope(t *testing.T) {
	db := newFakeInboxDB()
	var seen identity.Principal
	var correlation string
	c := testConsumerOn(db, func(ctx context.Context, _ eventing.Envelope) error {
		var err error
		seen, err = identity.FromContext(ctx)
		correlation = action.Correlation(ctx)
		return err
	})
	human := identity.Principal{Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "human.1"}, Scope: kernel.Scope{Tenant: "tenant.other", Company: "company.other"}}
	ctx, err := identity.WithPrincipal(context.Background(), human)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := c.Consume(ctx, inboxEnvelope("evt-1")); out != InboxProcessed || err != nil {
		t.Fatalf("%s, %v", out, err)
	}
	want := identity.Principal{Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.inq.test_projection"}, Scope: inboxScope}
	if seen != want || seen.Actor.String() != "system:inbox.inq.test_projection" {
		t.Fatalf("handler principal %+v, want %+v", seen, want)
	}
	if correlation != "corr.inbox-1" {
		t.Fatalf("correlation %q, want the event's", correlation)
	}
}

func TestConsumerRollsBackHandlerEffectAndRetriesOnError(t *testing.T) {
	db := newFakeInboxDB()
	fail := true
	c := testConsumerOn(db, func(ctx context.Context, e eventing.Envelope) error {
		effect(ctx, e)
		if fail {
			return errors.New("upstream unavailable")
		}
		return nil
	})
	out, err := c.Consume(context.Background(), inboxEnvelope("evt-1"))
	if out != InboxRetry || err == nil || errors.Is(err, ErrEventRejected) {
		t.Fatalf("%s, %v; want a retry", out, err)
	}
	if _, ok := db.status("evt-1"); ok || len(db.committed.effects) != 0 {
		t.Fatalf("a failed delivery left an inbox entry or an effect: %+v", db.committed)
	}
	fail = false
	if out, err = c.Consume(context.Background(), inboxEnvelope("evt-1")); out != InboxProcessed || err != nil {
		t.Fatalf("redelivery: %s, %v", out, err)
	}
}

func TestConsumerRollsBackHandlerEffectWhenMarkingFails(t *testing.T) {
	db := newFakeInboxDB()
	db.markErr = errors.New("connection lost")
	c := testConsumerOn(db, func(ctx context.Context, e eventing.Envelope) error { effect(ctx, e); return nil })
	if out, err := c.Consume(context.Background(), inboxEnvelope("evt-1")); out != InboxRetry || err == nil {
		t.Fatalf("%s, %v; want a retry", out, err)
	}
	if _, ok := db.status("evt-1"); ok || len(db.committed.effects) != 0 || db.commits != 0 {
		t.Fatalf("the effect or the inbox record committed alone: %+v", db.committed)
	}
}

func TestConsumerRecordsPermanentRejectionWithoutEffect(t *testing.T) {
	db := newFakeInboxDB()
	calls := 0
	c := testConsumerOn(db, func(ctx context.Context, e eventing.Envelope) error {
		calls++
		effect(ctx, e)
		return RejectEvent("inq.unknown_rfq", errors.New("rfq.9 does not exist"))
	})
	out, err := c.Consume(context.Background(), inboxEnvelope("evt-1"))
	if out != InboxRejected || !errors.Is(err, ErrEventRejected) || !strings.Contains(err.Error(), "rfq.9") {
		t.Fatalf("%s, %v; want a rejection with its cause", out, err)
	}
	if s, _ := db.status("evt-1"); s != eventing.InboxRejected || len(db.committed.effects) != 0 {
		t.Fatalf("status %d, effects %v; want rejected without effect", s, db.committed.effects)
	}
	if !slices.Equal(db.codes, []eventing.FailureCode{"inq.unknown_rfq"}) {
		t.Fatalf("failure codes %v", db.codes)
	}
	out, err = c.Consume(context.Background(), inboxEnvelope("evt-1"))
	if out != InboxRejected || !errors.Is(err, ErrEventRejected) || calls != 1 {
		t.Fatalf("redelivery: %s, %v, handler ran %d times; want rejected without running", out, err, calls)
	}
}

func TestRejectEventWithInvalidCodeUsesDefault(t *testing.T) {
	db := newFakeInboxDB()
	c := testConsumerOn(db, func(context.Context, eventing.Envelope) error { return RejectEvent("Not A Code", nil) })
	if out, _ := c.Consume(context.Background(), inboxEnvelope("evt-1")); out != InboxRejected {
		t.Fatalf("outcome %s", out)
	}
	if !slices.Equal(db.codes, []eventing.FailureCode{FailureEventRejected}) {
		t.Fatalf("failure codes %v", db.codes)
	}
	for _, code := range []eventing.FailureCode{"", ".x", "a b", "UPPER", eventing.FailureCode(strings.Repeat("a", 129))} {
		if validFailureCode(code) {
			t.Errorf("%q accepted", code)
		}
	}
}

// A worker that processed the event between the handler's rollback and the rejection wins.
func TestRejectionYieldsToAWorkerThatProcessedMeanwhile(t *testing.T) {
	db := newFakeInboxDB()
	c := testConsumerOn(db, func(ctx context.Context, e eventing.Envelope) error {
		db.committed.entries[eventing.InboxKey{Consumer: testConsumer, Source: e.Source, EventID: e.ID}] = eventing.InboxProcessed
		return RejectEvent("inq.stale", nil)
	})
	if out, err := c.Consume(context.Background(), inboxEnvelope("evt-1")); out != InboxDuplicate || err != nil {
		t.Fatalf("%s, %v; want the other worker's outcome", out, err)
	}
	if len(db.codes) != 0 {
		t.Fatalf("rejection recorded over a processed entry: %v", db.codes)
	}
}

func TestConsumerRetriesWhenAnotherWorkerHoldsTheEntry(t *testing.T) {
	db := newFakeInboxDB()
	db.busy = 1
	calls := 0
	c := testConsumerOn(db, func(context.Context, eventing.Envelope) error { calls++; return nil })
	out, err := c.Consume(context.Background(), inboxEnvelope("evt-1"))
	if out != InboxRetry || !errors.Is(err, eventing.ErrInboxBusy) || calls != 0 {
		t.Fatalf("%s, %v, %d calls; want a busy retry without running the handler", out, err, calls)
	}
	if out, err = c.Consume(context.Background(), inboxEnvelope("evt-1")); out != InboxProcessed || err != nil {
		t.Fatalf("after the other worker: %s, %v", out, err)
	}
}

func TestConsumerRejectsEnvelopesWithoutTransaction(t *testing.T) {
	db := newFakeInboxDB()
	calls := 0
	c := testConsumerOn(db, func(context.Context, eventing.Envelope) error { calls++; return nil })
	unscoped := inboxEnvelope("evt-1")
	unscoped.Scope = "tenant-without-company"
	invalid := inboxEnvelope("evt-2")
	invalid.ID = ""
	for _, e := range []eventing.Envelope{unscoped, invalid} {
		if out, err := c.Consume(context.Background(), e); out != InboxRejected || !errors.Is(err, ErrEventRejected) {
			t.Fatalf("%q: %s, %v; want a permanent rejection", e.Scope, out, err)
		}
	}
	if calls != 0 || len(db.scopes) != 0 {
		t.Fatalf("handler ran %d times, %d transactions", calls, len(db.scopes))
	}
}

func TestConsumerRefusesMisconfiguration(t *testing.T) {
	for _, name := range []eventing.InboxConsumer{"", "inq test", "-inq", eventing.InboxConsumer(strings.Repeat("a", 59))} {
		if _, err := InboxActor(name); err == nil {
			t.Errorf("consumer name %q accepted", name)
		}
	}
	if a, err := InboxActor(eventing.InboxConsumer(strings.Repeat("a", 58))); err != nil || len(a.ID) != 64 {
		t.Errorf("58-character consumer name: %+v, %v", a, err)
	}
	if _, err := NewConsumer(nil, testConsumer, EventHandlerFunc(func(context.Context, eventing.Envelope) error { return nil })); err == nil {
		t.Error("consumer without a DB accepted")
	}
	out, err := Consumer{Name: testConsumer}.Consume(context.Background(), inboxEnvelope("evt-1"))
	if out != InboxRetry || err == nil {
		t.Errorf("misconfigured consumer: %s, %v", out, err)
	}
}

func TestReceiveAnswersTheRelayByOutcome(t *testing.T) {
	db := newFakeInboxDB()
	c := testConsumerOn(db, func(ctx context.Context, e eventing.Envelope) error {
		switch e.ID {
		case "evt-reject":
			return RejectEvent("inq.unknown_rfq", nil)
		case "evt-retry":
			return errors.New("upstream unavailable")
		}
		effect(ctx, e)
		return nil
	})
	body := func(e eventing.Envelope) []byte {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	unscoped := inboxEnvelope("evt-unscoped")
	unscoped.Scope = "no-company"
	for _, tc := range []struct {
		name    string
		key     string
		body    []byte
		verdict Verdict
		status  int
		outcome InboxOutcome
	}{
		{"delivered", "evt-1", body(inboxEnvelope("evt-1")), VerdictDelivered, http.StatusOK, InboxProcessed},
		{"redelivered", "evt-1", body(inboxEnvelope("evt-1")), VerdictDelivered, http.StatusOK, InboxDuplicate},
		{"rejected by handler", "evt-reject", body(inboxEnvelope("evt-reject")), VerdictRejected, http.StatusUnprocessableEntity, InboxRejected},
		{"rejected before", "evt-reject", body(inboxEnvelope("evt-reject")), VerdictRejected, http.StatusUnprocessableEntity, InboxRejected},
		{"transient failure", "evt-retry", body(inboxEnvelope("evt-retry")), VerdictRetry, http.StatusServiceUnavailable, InboxRetry},
		{"scope does not parse", "evt-unscoped", body(unscoped), VerdictRejected, http.StatusUnprocessableEntity, InboxRejected},
		{"key is not the event ID", "evt-other", body(inboxEnvelope("evt-2")), VerdictRejected, http.StatusBadRequest, InboxRetry},
		{"no key", "", body(inboxEnvelope("evt-2")), VerdictRejected, http.StatusBadRequest, InboxRetry},
		{"not an envelope", "evt-3", []byte(`{"id":"evt-3"}`), VerdictRejected, http.StatusBadRequest, InboxRetry},
		{"not JSON", "evt-3", []byte(`evt-3`), VerdictRejected, http.StatusBadRequest, InboxRetry},
		{"too large", "evt-3", make([]byte, MaxEnvelopeBytes+1), VerdictRejected, http.StatusRequestEntityTooLarge, InboxRetry},
	} {
		r := c.Receive(context.Background(), tc.key, tc.body)
		if r.Verdict != tc.verdict || r.Status != tc.status || r.Outcome != tc.outcome {
			t.Errorf("%s: verdict %d, status %d, outcome %s, err %v; want %d, %d, %s", tc.name, r.Verdict, r.Status, r.Outcome, r.Err,
				tc.verdict, tc.status, tc.outcome)
		}
		if (r.Verdict == VerdictDelivered) != (r.Err == nil) {
			t.Errorf("%s: err %v with verdict %d", tc.name, r.Err, r.Verdict)
		}
	}
	if len(db.committed.effects) != 1 {
		t.Fatalf("effects %v; want evt-1 once", db.committed.effects)
	}
}
