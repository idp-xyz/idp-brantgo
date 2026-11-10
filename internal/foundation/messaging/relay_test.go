package messaging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-bento-go/testkit"
	"go.idp.xyz/idp-brant/internal/kernel"
)

type fakeOutbox struct {
	deliveries  []eventing.Delivery
	claims      []eventing.OutboxClaim
	published   []eventing.EventID
	failures    []eventing.DeliveryFailure
	abandoned   []eventing.FailureCode
	finalizeErr error
}

func (f *fakeOutbox) Claim(_ context.Context, c eventing.OutboxClaim) ([]eventing.Delivery, error) {
	f.claims = append(f.claims, c)
	d := f.deliveries
	f.deliveries = nil
	return d, nil
}

func (f *fakeOutbox) MarkPublished(_ context.Context, ref eventing.DeliveryRef, _ time.Time) error {
	f.published = append(f.published, ref.EventID)
	return f.finalizeErr
}

func (f *fakeOutbox) RecordFailure(_ context.Context, _ eventing.DeliveryRef, failure eventing.DeliveryFailure) error {
	f.failures = append(f.failures, failure)
	return f.finalizeErr
}

func (f *fakeOutbox) Abandon(_ context.Context, _ eventing.DeliveryRef, _ time.Time, code eventing.FailureCode) error {
	f.abandoned = append(f.abandoned, code)
	return f.finalizeErr
}

// fakeDeadLetters runs settle like DeadLetterStore and records the letter when both succeed.
type fakeDeadLetters struct {
	letters []DeadLetter
	err     error // returned after settle, as a failed insert would
}

func (f *fakeDeadLetters) Bury(ctx context.Context, letter DeadLetter, settle func(context.Context) error) error {
	if err := settle(ctx); err != nil {
		return err
	}
	if f.err != nil {
		return f.err
	}
	f.letters = append(f.letters, letter)
	return nil
}

type fakeAlerter struct{ alerts []DeadLetterAlert }

func (f *fakeAlerter) Alert(_ context.Context, a DeadLetterAlert) { f.alerts = append(f.alerts, a) }

var (
	relayNow   = time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)
	relayScope = kernel.Scope{Tenant: "tenant.demo", Company: "company.demo"}
)

func delivery(id string, failures uint32) eventing.Delivery {
	envelope := testkit.ValidEnvelope()
	envelope.ID = eventing.EventID(id)
	envelope.Scope = relayScope.String()
	return eventing.Delivery{Ref: eventing.DeliveryRef{Source: envelope.Source, EventID: envelope.ID, LeaseToken: "lease"}, Envelope: envelope, Attempt: failures + 1, Failures: failures}
}

type relayFakes struct {
	outbox  *fakeOutbox
	dead    *fakeDeadLetters
	alerter *fakeAlerter
}

func testRelay(outbox *fakeOutbox, publisher eventing.PublisherFunc) Relay {
	relay, _ := testRelayWithFakes(outbox, publisher)
	return relay
}

func testRelayWithFakes(outbox *fakeOutbox, publisher eventing.PublisherFunc) (Relay, relayFakes) {
	fakes := relayFakes{outbox: outbox, dead: &fakeDeadLetters{}, alerter: &fakeAlerter{}}
	return Relay{
		Outbox: outbox, DeadLetters: fakes.dead, Alerter: fakes.alerter, Publisher: publisher, Clock: testkit.NewFixedClock(relayNow),
		Batch: 10, LeaseFor: 30 * time.Second, PublishTimeout: 2 * time.Second, MaxAttempts: 5, MaxFailures: 3,
		RetryBase: time.Second, RetryMax: time.Minute, IdleWait: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, fakes
}

func failing(context.Context, eventing.Envelope) error { return errors.New("sink down") }

func rejecting(context.Context, eventing.Envelope) error {
	return fmt.Errorf("%w: sink answered 422", eventing.ErrPublishRejected)
}

func TestRelayPublishesInClaimOrder(t *testing.T) {
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{delivery("a", 0), delivery("b", 0)}}
	var sent []eventing.EventID
	relay := testRelay(outbox, func(_ context.Context, e eventing.Envelope) error { sent = append(sent, e.ID); return nil })
	claimed, err := relay.RunOnce(context.Background())
	if err != nil || claimed != 2 {
		t.Fatalf("RunOnce = %d, %v", claimed, err)
	}
	if fmt.Sprint(sent) != "[a b]" || fmt.Sprint(outbox.published) != "[a b]" {
		t.Fatalf("sent %v, published %v", sent, outbox.published)
	}
	// Bento abandons silently at Claim's attempt limit, so the relay keeps it out of reach and
	// enforces MaxAttempts itself.
	if c := outbox.claims[0]; !c.Now.Equal(relayNow) || c.Limit != 10 || c.LeaseFor != 30*time.Second || c.MaxAttempts != math.MaxUint32 {
		t.Fatalf("claim = %+v", c)
	}
}

func TestRelayRecordsFailureWithBackoff(t *testing.T) {
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{delivery("a", 1)}}
	relay, fakes := testRelayWithFakes(outbox, failing)
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(outbox.failures) != 1 || len(outbox.published) != 0 {
		t.Fatalf("failures %v, published %v", outbox.failures, outbox.published)
	}
	f := outbox.failures[0]
	if !f.RetryAt.Equal(relayNow.Add(2*time.Second)) || f.Code != FailurePublishFailed || f.MaxFailures != 3 {
		t.Fatalf("failure = %+v, want retry after 2s with budget 3", f)
	}
	if len(fakes.dead.letters)+len(fakes.alerter.alerts) != 0 {
		t.Fatalf("a failure within budget is no dead letter: %v, %v", fakes.dead.letters, fakes.alerter.alerts)
	}
}

func TestRelayRecordsFailureWhenPublishTimesOut(t *testing.T) {
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{delivery("a", 0)}}
	relay := testRelay(outbox, func(ctx context.Context, _ eventing.Envelope) error { <-ctx.Done(); return ctx.Err() })
	relay.PublishTimeout = 20 * time.Millisecond
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatalf("a timed-out publish must not stop the relay: %v", err)
	}
	if len(outbox.failures) != 1 || outbox.failures[0].Code != FailurePublishFailed {
		t.Fatalf("failures %v, want one %s", outbox.failures, FailurePublishFailed)
	}
}

// requireOneDeadLetter checks that exactly one dead letter and one alert describe the delivery.
func requireOneDeadLetter(t *testing.T, fakes relayFakes, id eventing.EventID, code eventing.FailureCode, failures uint32) (DeadLetter, DeadLetterAlert) {
	t.Helper()
	if len(fakes.dead.letters) != 1 || len(fakes.alerter.alerts) != 1 {
		t.Fatalf("dead letters %+v, alerts %+v; want one of each", fakes.dead.letters, fakes.alerter.alerts)
	}
	l, a := fakes.dead.letters[0], fakes.alerter.alerts[0]
	if l.EventID != id || l.Code != code || l.Failures != failures || l.Scope != relayScope || l.Envelope.ID != id || !l.DeadAt.Equal(relayNow) {
		t.Fatalf("dead letter = %+v", l)
	}
	if a.EventID != id || a.Code != code || a.Failures != failures || !a.Stored || a.Critical != l.Critical || a.Error != l.LastError {
		t.Fatalf("alert = %+v for letter %+v", a, l)
	}
	return l, a
}

func TestRelayDeadLettersRejectedDelivery(t *testing.T) {
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{delivery("rejected", 0)}}
	relay, fakes := testRelayWithFakes(outbox, rejecting)
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(outbox.abandoned) != fmt.Sprint([]eventing.FailureCode{FailurePublishRejected}) || len(outbox.failures) != 0 {
		t.Fatalf("abandoned %v, failures %v", outbox.abandoned, outbox.failures)
	}
	l, a := requireOneDeadLetter(t, fakes, "rejected", FailurePublishRejected, 0)
	if l.Critical || !strings.Contains(l.LastError, "422") || l.Attempt != 1 {
		t.Fatalf("dead letter = %+v, alert = %+v", l, a)
	}
}

func TestRelayDeadLettersInvalidEnvelopeWithoutPublishing(t *testing.T) {
	invalid := delivery("invalid", 0)
	invalid.Envelope.Payload = nil
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{invalid}}
	var sent []eventing.EventID
	relay, fakes := testRelayWithFakes(outbox, func(_ context.Context, e eventing.Envelope) error { sent = append(sent, e.ID); return nil })
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 0 {
		t.Fatalf("an invalid envelope must not reach the publisher; sent %v", sent)
	}
	if fmt.Sprint(outbox.abandoned) != fmt.Sprint([]eventing.FailureCode{eventing.FailureEnvelopeInvalidOnPublish}) {
		t.Fatalf("abandoned %v", outbox.abandoned)
	}
	l, _ := requireOneDeadLetter(t, fakes, "invalid", eventing.FailureEnvelopeInvalidOnPublish, 0)
	if !strings.Contains(l.LastError, "payload") {
		t.Fatalf("last error = %q", l.LastError)
	}
}

// The failure that spends the budget abandons through RecordFailure, so it is counted; Bento
// abandons when failures + 1 reaches the budget the relay passes.
func TestRelayDeadLettersWhenFailureBudgetIsSpent(t *testing.T) {
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{delivery("a", 2)}}
	relay, fakes := testRelayWithFakes(outbox, failing)
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(outbox.failures) != 1 || len(outbox.abandoned) != 0 {
		t.Fatalf("failures %v, abandoned %v", outbox.failures, outbox.abandoned)
	}
	if f := outbox.failures[0]; f.MaxFailures != 3 || f.Code != FailurePublishFailed || !f.FailedAt.Equal(relayNow) {
		t.Fatalf("failure = %+v, want the budget of 3 reached", f)
	}
	l, _ := requireOneDeadLetter(t, fakes, "a", FailurePublishFailed, 3)
	if l.LastError != "sink down" || l.Attempt != 3 {
		t.Fatalf("dead letter = %+v", l)
	}
}

func TestRelayDeadLettersWhenLastAttemptFails(t *testing.T) {
	d := delivery("a", 0)
	d.Attempt = 5 // MaxAttempts; earlier attempts lost their lease
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{d}}
	relay, fakes := testRelayWithFakes(outbox, failing)
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(outbox.failures) != 1 || outbox.failures[0].MaxFailures != 1 {
		t.Fatalf("failures %+v, want one that abandons (budget 1)", outbox.failures)
	}
	requireOneDeadLetter(t, fakes, "a", FailurePublishFailed, 1)
}

func TestRelayDeadLettersDeliveryClaimedBeyondAttemptBudget(t *testing.T) {
	d := delivery("a", 1)
	d.Attempt = 6 // the fifth attempt never settled
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{d}}
	var sent int
	relay, fakes := testRelayWithFakes(outbox, func(context.Context, eventing.Envelope) error { sent++; return nil })
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sent != 0 || fmt.Sprint(outbox.abandoned) != fmt.Sprint([]eventing.FailureCode{eventing.FailureClaimLimitExceeded}) {
		t.Fatalf("sent %d, abandoned %v; want abandoned unpublished", sent, outbox.abandoned)
	}
	l, _ := requireOneDeadLetter(t, fakes, "a", eventing.FailureClaimLimitExceeded, 1)
	if l.Attempt != 6 || !strings.Contains(l.LastError, "attempt 6 of 5") {
		t.Fatalf("dead letter = %+v", l)
	}
}

func TestRelayMarksRevocationAndRiskDeadLettersCritical(t *testing.T) {
	revoked := delivery("revoked", 0)
	revoked.Envelope.Type = "brant.pty.qualification_revoked"
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{revoked}}
	relay, fakes := testRelayWithFakes(outbox, rejecting)
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	l, a := requireOneDeadLetter(t, fakes, "revoked", FailurePublishRejected, 0)
	if !l.Critical || !a.Critical || a.EventType != revoked.Envelope.Type {
		t.Fatalf("dead letter %+v, alert %+v; want critical", l, a)
	}

	custom, customFakes := testRelayWithFakes(&fakeOutbox{deliveries: []eventing.Delivery{delivery("plain", 0)}}, rejecting)
	custom.Critical = func(eventing.EventType) bool { return true }
	if _, err := custom.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := customFakes.alerter.alerts; len(a) != 1 || !a[0].Critical {
		t.Fatalf("a configured predicate decides: %+v", a)
	}
}

// An envelope whose scope does not parse cannot be stored under RLS: it is still abandoned, never
// retried forever, and the alert is critical.
func TestRelayAbandonsUnscopedEnvelopeWithCriticalAlert(t *testing.T) {
	unscoped := delivery("unscoped", 0)
	unscoped.Envelope.Scope = "test-scope"
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{unscoped}}
	relay, fakes := testRelayWithFakes(outbox, rejecting)
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(outbox.abandoned) != fmt.Sprint([]eventing.FailureCode{FailurePublishRejected}) || len(fakes.dead.letters) != 0 {
		t.Fatalf("abandoned %v, dead letters %v", outbox.abandoned, fakes.dead.letters)
	}
	if a := fakes.alerter.alerts; len(a) != 1 || !a[0].Critical || a[0].Stored || a[0].EventID != "unscoped" ||
		a[0].Scope != "test-scope" || !strings.Contains(a[0].Error, "no dead letter") || !strings.Contains(a[0].Error, "422") {
		t.Fatalf("alerts %+v, want one critical unstored alert", a)
	}

	exhausted := delivery("unscoped-exhausted", 2)
	exhausted.Envelope.Scope = "tenant.demo"
	outbox = &fakeOutbox{deliveries: []eventing.Delivery{exhausted}}
	relay, fakes = testRelayWithFakes(outbox, failing)
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(outbox.failures) != 1 || outbox.failures[0].MaxFailures != 3 || len(fakes.alerter.alerts) != 1 || !fakes.alerter.alerts[0].Critical {
		t.Fatalf("failures %+v, alerts %+v", outbox.failures, fakes.alerter.alerts)
	}
}

func TestRelayDoesNotAlertWhenAnotherWorkerOwnsTheDelivery(t *testing.T) {
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{delivery("a", 0)}, finalizeErr: eventing.ErrLeaseLost}
	relay, fakes := testRelayWithFakes(outbox, rejecting)
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatalf("lost lease must not fail the round: %v", err)
	}
	if len(fakes.dead.letters)+len(fakes.alerter.alerts) != 0 {
		t.Fatalf("dead letters %v, alerts %v", fakes.dead.letters, fakes.alerter.alerts)
	}
}

// A dead letter that cannot be stored fails the round: the abandonment rolls back with it, and
// the delivery comes back after its lease.
func TestRelayFailsRoundWhenDeadLetterCannotBeStored(t *testing.T) {
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{delivery("a", 0)}}
	relay, fakes := testRelayWithFakes(outbox, rejecting)
	fakes.dead.err = errors.New("insert failed")
	if _, err := relay.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "insert failed") {
		t.Fatalf("RunOnce = %v, want the insert error", err)
	}
	if len(fakes.alerter.alerts) != 0 {
		t.Fatalf("no alert before the dead letter commits: %v", fakes.alerter.alerts)
	}
}

func TestRelayTreatsLostLeaseAsSettled(t *testing.T) {
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{delivery("a", 0)}, finalizeErr: eventing.ErrLeaseLost}
	relay := testRelay(outbox, func(context.Context, eventing.Envelope) error { return nil })
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatalf("lost lease must not fail the round: %v", err)
	}
}

func TestRelayLeavesDeliveryLeasedWhenStopping(t *testing.T) {
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{delivery("a", 0)}}
	ctx, cancel := context.WithCancel(context.Background())
	relay, fakes := testRelayWithFakes(outbox, func(ctx context.Context, _ eventing.Envelope) error { cancel(); return ctx.Err() })
	if _, err := relay.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(outbox.published)+len(outbox.failures)+len(outbox.abandoned)+len(fakes.dead.letters) != 0 {
		t.Fatal("a delivery interrupted by shutdown must stay leased")
	}
}

func TestRelayBackoffDoublesUpToMax(t *testing.T) {
	relay := Relay{RetryBase: time.Second, RetryMax: 10 * time.Second}
	for failures, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second} {
		if got := relay.backoff(uint32(failures)); got != want {
			t.Errorf("backoff(%d) = %v, want %v", failures, got, want)
		}
	}
}

func TestRelayRejectsMisconfiguration(t *testing.T) {
	if _, err := (Relay{}).RunOnce(context.Background()); err == nil {
		t.Fatal("zero Relay must not run")
	}
	relay := testRelay(&fakeOutbox{}, func(context.Context, eventing.Envelope) error { return nil })
	relay.PublishTimeout = relay.LeaseFor / time.Duration(relay.Batch)
	if _, err := relay.RunOnce(context.Background()); err == nil {
		t.Fatal("a batch whose publishes can outlast the lease must not run")
	}
	relay = testRelay(&fakeOutbox{}, func(context.Context, eventing.Envelope) error { return nil })
	relay.DeadLetters = nil
	if _, err := relay.RunOnce(context.Background()); err == nil {
		t.Fatal("a relay without dead letters must not run")
	}
}
