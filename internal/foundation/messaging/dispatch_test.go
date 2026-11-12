package messaging

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-bento-go/testkit"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	trdSource          = "idp-brant.trd"
	orderActivated     = eventing.EventType("idp-brant.trd.order_activated.v1")
	orderAmended       = eventing.EventType("idp-brant.trd.order_amended.v1")
	stlConsumer        = eventing.InboxConsumer("stl.order_credit")
	cnxConsumer        = eventing.InboxConsumer("cnx.order_compliance")
	purConsumer        = eventing.InboxConsumer("pur.order_review")
	dispatchCorrelator = "corr.dispatch-1"
)

func trdEnvelope(id string, typ eventing.EventType) eventing.Envelope {
	e := testkit.ValidEnvelope()
	e.ID, e.Source, e.Type, e.Scope, e.CorrelationID = eventing.EventID(id), trdSource, typ, inboxScope.String(), dispatchCorrelator
	return e
}

// fakeDispatch is a dispatcher whose consumers each run on a fake inbox DB of their own.
type fakeDispatch struct {
	*Dispatcher
	dbs map[eventing.InboxConsumer]*fakeInboxDB
}

func newFakeDispatch(t *testing.T, subs ...Subscription) fakeDispatch {
	t.Helper()
	f := fakeDispatch{dbs: map[eventing.InboxConsumer]*fakeInboxDB{}}
	d, err := newDispatcher(subs, func(s Subscription) (route, error) {
		db, ok := f.dbs[s.Consumer]
		if !ok {
			db = newFakeInboxDB()
			f.dbs[s.Consumer] = db
		}
		c := Consumer{Name: s.Consumer, Inbox: db, Transactor: db.transactor,
			Clock: testkit.NewFixedClock(time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))}
		return route{sub: s, consumer: c, executor: func(ctx context.Context) (bentopg.Executor, error) {
			fakeTx(ctx) // panics outside the consumer's transaction
			return nil, nil
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Dispatcher = d
	return f
}

// status is the committed inbox entry of consumer for a TRD event.
func (f fakeDispatch) status(consumer eventing.InboxConsumer, id string) (eventing.InboxStatus, bool) {
	s, ok := f.dbs[consumer].committed.entries[eventing.InboxKey{Consumer: consumer, Source: trdSource, EventID: eventing.EventID(id)}]
	return s, ok
}

func (f fakeDispatch) effects(consumer eventing.InboxConsumer) []eventing.EventID {
	return f.dbs[consumer].committed.effects
}

// recorder is a handler that writes an effect in the consumer's transaction and keeps what it got.
type recorder struct {
	events []Event
	fail   func(Event) error
}

func (r *recorder) handle(ctx context.Context, e Event) error {
	r.events = append(r.events, e)
	effect(ctx, e.Envelope)
	if r.fail != nil {
		return r.fail(e)
	}
	return nil
}

func subscription(consumer eventing.InboxConsumer, h EventFunc, types ...eventing.EventType) Subscription {
	return Subscription{Consumer: consumer, Source: trdSource, EventTypes: types, Handler: h}
}

// Every subscription of the event's source and type gets it, in the consumer's own transaction,
// as the consumer's system actor in the event's scope; a redelivery runs no handler again, and
// an event nobody subscribes to is delivered to nobody.
func TestDispatcherDeliversToEverySubscriptionOfTheEvent(t *testing.T) {
	stl, cnx, pur := &recorder{}, &recorder{}, &recorder{}
	f := newFakeDispatch(t,
		subscription(stlConsumer, stl.handle, orderActivated, orderAmended),
		subscription(cnxConsumer, cnx.handle, orderActivated),
		subscription(purConsumer, pur.handle, orderAmended))
	if got := f.Consumers(trdSource, orderActivated); !slices.Equal(got, []eventing.InboxConsumer{stlConsumer, cnxConsumer}) {
		t.Fatalf("consumers of OrderActivated %v", got)
	}
	e := trdEnvelope("evt-activated", orderActivated)
	for i := range 2 {
		if err := f.Publish(context.Background(), e); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}
	if len(stl.events) != 1 || len(cnx.events) != 1 || len(pur.events) != 0 {
		t.Fatalf("handlers ran %d, %d and %d times; want 1, 1 and 0", len(stl.events), len(cnx.events), len(pur.events))
	}
	got := stl.events[0]
	want := identity.Principal{Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.stl.order_credit"}, Scope: inboxScope}
	if got.Consumer != stlConsumer || got.Principal != want || got.Envelope.ID != e.ID || got.Prepared != nil {
		t.Fatalf("event %+v", got)
	}
	for _, c := range []eventing.InboxConsumer{stlConsumer, cnxConsumer} {
		if s, ok := f.status(c, "evt-activated"); !ok || s != eventing.InboxProcessed || len(f.effects(c)) != 1 {
			t.Fatalf("%s: entry %d (%v), effects %v; want processed once", c, s, ok, f.effects(c))
		}
		if !slices.Equal(f.dbs[c].scopes, []kernel.Scope{inboxScope, inboxScope}) {
			t.Fatalf("%s: transaction scopes %v", c, f.dbs[c].scopes)
		}
	}
	if len(f.dbs[purConsumer].scopes) != 0 {
		t.Fatal("a consumer of another event type opened a transaction")
	}
	if err := f.Publish(context.Background(), trdEnvelope("evt-frozen", "idp-brant.trd.quote_frozen.v1")); err != nil {
		t.Fatalf("an event without subscriptions: %v", err)
	}
	other := trdEnvelope("evt-other-source", orderActivated)
	other.Source = "idp-brant.cst"
	if err := f.Publish(context.Background(), other); err != nil || len(stl.events) != 1 {
		t.Fatalf("an event of another source: %v, %d runs", err, len(stl.events))
	}
}

// A consumer that fails is retried by the relay: the others committed their effect and see a
// duplicate on the next attempt, and the failing one runs again until it succeeds.
func TestDispatcherRetriesAFailingConsumerWithoutDoubleEffect(t *testing.T) {
	failing := true
	stl := &recorder{fail: func(Event) error {
		if failing {
			return errors.New("credit store unavailable")
		}
		return nil
	}}
	cnx := &recorder{}
	f := newFakeDispatch(t, subscription(stlConsumer, stl.handle, orderActivated), subscription(cnxConsumer, cnx.handle, orderActivated))
	e := trdEnvelope("evt-retry", orderActivated)
	err := f.Publish(context.Background(), e)
	if err == nil || errors.Is(err, eventing.ErrPublishRejected) || !strings.Contains(err.Error(), "stl.order_credit") {
		t.Fatalf("first attempt: %v; want a retryable error naming the consumer", err)
	}
	if _, ok := f.status(stlConsumer, "evt-retry"); ok || len(f.effects(stlConsumer)) != 0 || len(f.effects(cnxConsumer)) != 1 {
		t.Fatalf("after the failure: stl effects %v, cnx effects %v", f.effects(stlConsumer), f.effects(cnxConsumer))
	}
	failing = false
	if err = f.Publish(context.Background(), e); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(stl.events) != 2 || len(cnx.events) != 1 || len(f.effects(stlConsumer)) != 1 || len(f.effects(cnxConsumer)) != 1 {
		t.Fatalf("stl ran %d times, cnx %d; effects %v and %v", len(stl.events), len(cnx.events), f.effects(stlConsumer), f.effects(cnxConsumer))
	}
}

// A rejection is permanent: the relay dead-letters the delivery at once, the other consumers keep
// their effect, and a redelivery or replay is refused again without running a handler. A
// consumer that must retry takes precedence, so it gets its retries before the dead letter.
func TestDispatcherDeadLettersARejection(t *testing.T) {
	stl := &recorder{fail: func(Event) error { return RejectEvent("stl.unknown_order", errors.New("order.9 has no reservation")) }}
	cnx := &recorder{}
	busy := true
	pur := &recorder{fail: func(Event) error {
		if busy {
			return errors.New("transient")
		}
		return nil
	}}
	f := newFakeDispatch(t,
		subscription(stlConsumer, stl.handle, orderActivated, orderAmended),
		subscription(cnxConsumer, cnx.handle, orderActivated),
		subscription(purConsumer, pur.handle, orderAmended))
	e := trdEnvelope("evt-rejected", orderActivated)
	for i := range 2 {
		err := f.Publish(context.Background(), e)
		if !errors.Is(err, eventing.ErrPublishRejected) || !errors.Is(err, ErrEventRejected) || !strings.Contains(err.Error(), "stl.order_credit") {
			t.Fatalf("delivery %d: %v; want a publish rejection naming the consumer", i+1, err)
		}
	}
	if s, _ := f.status(stlConsumer, "evt-rejected"); s != eventing.InboxRejected || len(f.effects(stlConsumer)) != 0 ||
		!slices.Equal(f.dbs[stlConsumer].codes, []eventing.FailureCode{"stl.unknown_order"}) {
		t.Fatalf("stl: entry %d, effects %v, codes %v", s, f.effects(stlConsumer), f.dbs[stlConsumer].codes)
	}
	if len(stl.events) != 1 || len(cnx.events) != 1 || len(f.effects(cnxConsumer)) != 1 {
		t.Fatalf("stl ran %d times, cnx %d; want once each", len(stl.events), len(cnx.events))
	}

	amended := trdEnvelope("evt-amended", orderAmended)
	if err := f.Publish(context.Background(), amended); err == nil || errors.Is(err, eventing.ErrPublishRejected) {
		t.Fatalf("rejection with a consumer to retry: %v; want a retry", err)
	}
	busy = false
	if err := f.Publish(context.Background(), amended); !errors.Is(err, eventing.ErrPublishRejected) {
		t.Fatalf("after the retry: %v; want the rejection", err)
	}
	if len(pur.events) != 2 || len(f.effects(purConsumer)) != 1 || len(stl.events) != 2 {
		t.Fatalf("pur ran %d times with effects %v, stl %d times", len(pur.events), f.effects(purConsumer), len(stl.events))
	}
}

// Prepare runs before the consumer's transaction, as the consumer in the event's scope, and its
// result reaches the handler; its error is the handler's error, unless the event was settled
// before.
func TestDispatcherRunsPrepareOutsideTheTransaction(t *testing.T) {
	var prepareErr error
	var seen identity.Principal
	var correlation string
	stl := &recorder{}
	sub := subscription(stlConsumer, stl.handle, orderActivated)
	sub.Prepare = func(ctx context.Context, e eventing.Envelope) (any, error) {
		if _, inTx := ctx.Value(fakeInboxTxKey{}).(*fakeInboxState); inTx {
			t.Error("Prepare ran inside a transaction")
		}
		seen, _ = identity.FromContext(ctx)
		correlation = action.Correlation(ctx)
		return "order.1@2", prepareErr
	}
	f := newFakeDispatch(t, sub)

	prepareErr = errors.New("trd lookup unavailable")
	if err := f.Publish(context.Background(), trdEnvelope("evt-1", orderActivated)); err == nil || errors.Is(err, eventing.ErrPublishRejected) {
		t.Fatalf("prepare failure: %v; want a retry", err)
	}
	if _, ok := f.status(stlConsumer, "evt-1"); ok || len(stl.events) != 0 {
		t.Fatal("a failed Prepare left an inbox entry or ran the handler")
	}
	prepareErr = nil
	if err := f.Publish(context.Background(), trdEnvelope("evt-1", orderActivated)); err != nil {
		t.Fatal(err)
	}
	if len(stl.events) != 1 || stl.events[0].Prepared != "order.1@2" {
		t.Fatalf("handler events %+v; want the prepared value once", stl.events)
	}
	if seen.Actor.ID != "inbox.stl.order_credit" || seen.Scope != inboxScope || correlation != dispatchCorrelator {
		t.Fatalf("Prepare ran as %+v with correlation %q", seen, correlation)
	}

	prepareErr = errors.New("trd lookup unavailable")
	if err := f.Publish(context.Background(), trdEnvelope("evt-1", orderActivated)); err != nil {
		t.Fatalf("a Prepare failure for a processed event: %v; want the duplicate", err)
	}
	prepareErr = RejectEvent("stl.order_unknown", nil)
	if err := f.Publish(context.Background(), trdEnvelope("evt-2", orderActivated)); !errors.Is(err, eventing.ErrPublishRejected) {
		t.Fatalf("Prepare rejection: %v", err)
	}
	if s, _ := f.status(stlConsumer, "evt-2"); s != eventing.InboxRejected || len(stl.events) != 1 {
		t.Fatalf("entry %d after a Prepare rejection, %d handler runs", s, len(stl.events))
	}
}

func TestDispatcherRefusesEnvelopesItCannotDeliver(t *testing.T) {
	stl := &recorder{}
	f := newFakeDispatch(t, subscription(stlConsumer, stl.handle, orderActivated))
	invalid := trdEnvelope("", orderActivated)
	if err := f.Publish(context.Background(), invalid); !errors.Is(err, eventing.ErrPublishRejected) {
		t.Fatalf("invalid envelope: %v", err)
	}
	unscoped := trdEnvelope("evt-unscoped", orderActivated)
	unscoped.Scope = "no-company"
	if err := f.Publish(context.Background(), unscoped); !errors.Is(err, eventing.ErrPublishRejected) || !errors.Is(err, kernel.ErrInvalidScope) {
		t.Fatalf("unscoped envelope: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.Publish(ctx, trdEnvelope("evt-late", orderActivated)); !errors.Is(err, context.Canceled) || errors.Is(err, eventing.ErrPublishRejected) {
		t.Fatalf("canceled publish: %v", err)
	}
	if len(stl.events) != 0 || len(f.dbs[stlConsumer].scopes) != 0 {
		t.Fatalf("%d handler runs, %d transactions", len(stl.events), len(f.dbs[stlConsumer].scopes))
	}
}

func TestNewDispatcherRefusesInvalidSubscriptions(t *testing.T) {
	ok := func(context.Context, Event) error { return nil }
	valid := subscription(stlConsumer, ok, orderActivated)
	with := func(change func(*Subscription)) Subscription {
		s := valid
		s.EventTypes = slices.Clone(valid.EventTypes)
		change(&s)
		return s
	}
	db1, db2 := &bentopg.DB{}, &bentopg.DB{}
	for name, subs := range map[string][]Subscription{
		"consumer without context": {with(func(s *Subscription) { s.Consumer = "order_credit" })},
		"consumer with upper case": {with(func(s *Subscription) { s.Consumer = "STL.order_credit" })},
		"consumer name too long":   {with(func(s *Subscription) { s.Consumer = eventing.InboxConsumer("stl." + strings.Repeat("a", 55)) })},
		"source of another form":   {with(func(s *Subscription) { s.Source = "trd" })},
		"own events": {with(func(s *Subscription) {
			s.Source = "idp-brant.stl"
			s.EventTypes = []eventing.EventType{"idp-brant.stl.x.v1"}
		})},
		"no event types":            {with(func(s *Subscription) { s.EventTypes = nil })},
		"type without version":      {with(func(s *Subscription) { s.EventTypes = []eventing.EventType{"idp-brant.trd.order_activated"} })},
		"type of another source":    {with(func(s *Subscription) { s.EventTypes = []eventing.EventType{"idp-brant.cst.cost_source_denied.v1"} })},
		"type listed twice":         {with(func(s *Subscription) { s.EventTypes = append(s.EventTypes, orderActivated) })},
		"no handler":                {with(func(s *Subscription) { s.Handler = nil })},
		"invalid permission":        {with(func(s *Subscription) { s.Permissions = []string{"credit consume"} })},
		"consumer subscribed twice": {valid, with(func(s *Subscription) { s.EventTypes = []eventing.EventType{orderAmended, orderActivated} })},
		"consumer on two DBs": {with(func(s *Subscription) { s.DB = db1 }),
			with(func(s *Subscription) { s.DB = db2; s.EventTypes = []eventing.EventType{orderAmended} })},
	} {
		if _, err := newDispatcher(subs, func(s Subscription) (route, error) { return route{sub: s}, nil }); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := NewDispatcher(valid); !errors.Is(err, errNoDB) {
		t.Errorf("subscription without a DB: %v", err)
	}
	if d, err := NewDispatcher(); err != nil || len(d.Subscriptions()) != 0 {
		t.Errorf("no subscriptions: %v", err)
	}
	if d, err := newDispatcher([]Subscription{valid, with(func(s *Subscription) {
		s.Source, s.EventTypes = "idp-brant.cst", []eventing.EventType{"idp-brant.cst.cost_source_denied.v1"}
	})}, func(s Subscription) (route, error) { return route{sub: s}, nil }); err != nil || len(d.Subscriptions()) != 2 {
		t.Errorf("one consumer on two sources: %v", err)
	}
}

func TestInboxGrantsUnitePermissionsPerConsumer(t *testing.T) {
	ok := func(context.Context, Event) error { return nil }
	consume := subscription(stlConsumer, ok, orderActivated)
	consume.Permissions = []string{"credit.consume", "stl.read"}
	release := subscription(stlConsumer, ok, orderAmended)
	release.Permissions = []string{"credit.release", "credit.consume"}
	check := subscription(cnxConsumer, ok, orderAmended)
	grants, err := InboxGrants([]Subscription{consume, check, release})
	if err != nil {
		t.Fatal(err)
	}
	want := []InboxGrant{
		{Consumer: stlConsumer, Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.stl.order_credit"},
			Permissions: []string{"credit.consume", "credit.release", "stl.read"}},
		{Consumer: cnxConsumer, Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.cnx.order_compliance"}, Permissions: []string{}},
	}
	if len(grants) != len(want) {
		t.Fatalf("grants %+v", grants)
	}
	for i := range want {
		if grants[i].Consumer != want[i].Consumer || grants[i].Actor != want[i].Actor || !slices.Equal(grants[i].Permissions, want[i].Permissions) {
			t.Errorf("grant %d: %+v, want %+v", i, grants[i], want[i])
		}
	}
	if _, err = InboxGrants([]Subscription{consume, consume}); err == nil {
		t.Error("a consumer subscribed twice was granted")
	}
}

func TestSourceAndConsumerContexts(t *testing.T) {
	for source, want := range map[string]string{"idp-brant.trd": "trd", "idp-brant.relay-probe": "", "idp-brant.": "", "trd": "", "idp-brant.trd.x": ""} {
		if got := SourceContext(source); got != want {
			t.Errorf("SourceContext(%q) = %q, want %q", source, got, want)
		}
	}
	for consumer, want := range map[eventing.InboxConsumer]string{
		"stl.order_credit": "stl", "inq.party.deactivated": "inq", "stl": "", "stl.": "", "st.order": "", "stl.Order": "", ".stl.x": "",
	} {
		if got := ConsumerContext(consumer); got != want {
			t.Errorf("ConsumerContext(%q) = %q, want %q", consumer, got, want)
		}
	}
}
