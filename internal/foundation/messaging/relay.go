package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Failure codes recorded by Relay.
const (
	FailurePublishFailed   eventing.FailureCode = "publish.failed"
	FailurePublishRejected eventing.FailureCode = "publish.rejected"
)

// claimAttemptCeiling is the attempt limit the relay hands to Claim. Bento abandons a delivery
// that reaches Claim's limit inside Claim and tells no one, which would skip the dead letter, so
// the relay enforces MaxAttempts itself on the deliveries it claims.
const claimAttemptCeiling = math.MaxUint32

// Relay moves committed Outbox deliveries to a Publisher in claim order. Delivery is at least once:
// a crash between Publish and MarkPublished re-delivers after the lease expires, so consumers must
// deduplicate by event ID.
//
// A batch shares one lease and is published one delivery at a time, so Batch × PublishTimeout must
// be shorter than LeaseFor. Leases compare Clock readings, so workers need synchronized clocks.
//
// The relay abandons a delivery that the sink rejects permanently, whose envelope is invalid, whose
// MaxFailures-th publish fails, or that is claimed after MaxAttempts attempts (ADR-BR-015). Each
// abandonment is stored as a dead letter in the same transaction and then reported to Alerter; an
// envelope whose scope does not parse cannot be stored under RLS, so it is abandoned with a
// critical alert. No delivery is dropped without one or the other.
type Relay struct {
	Outbox interface {
		eventing.OutboxClaimer
		eventing.OutboxFinalizer
	}
	DeadLetters    DeadLetterWriter              // shares the Outbox store's transaction owner
	Alerter        Alerter                       // nil means SlogAlerter on Logger
	Critical       func(eventing.EventType) bool // revocation, repudiation and risk events; nil means CriticalEventType
	Publisher      eventing.Publisher
	Clock          application.Clock
	Batch          int
	LeaseFor       time.Duration
	PublishTimeout time.Duration // bound on one Publish; a publish cut off by it counts as a failure
	MaxAttempts    uint32        // claims of one delivery; a claim beyond it is abandoned unpublished
	MaxFailures    uint32
	RetryBase      time.Duration // delay after the first failure, doubled per recorded failure up to RetryMax
	RetryMax       time.Duration
	IdleWait       time.Duration
	Logger         *slog.Logger
}

// Run relays until ctx is canceled and then returns ctx.Err().
func (r Relay) Run(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	for {
		claimed, err := r.RunOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			r.logger().Error("outbox relay round failed", "error", err)
		}
		if claimed > 0 && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.IdleWait):
		}
	}
}

// RunOnce claims one batch and settles each delivery. It returns how many deliveries it claimed.
func (r Relay) RunOnce(ctx context.Context) (int, error) {
	if err := r.validate(); err != nil {
		return 0, err
	}
	deliveries, err := r.Outbox.Claim(ctx, eventing.OutboxClaim{
		Now: r.Clock.Now(), Limit: r.Batch, LeaseFor: r.LeaseFor, MaxAttempts: claimAttemptCeiling,
	})
	if err != nil {
		return 0, err
	}
	for _, d := range deliveries {
		if err := r.settle(ctx, d); err != nil {
			return len(deliveries), err
		}
	}
	return len(deliveries), nil
}

func (r Relay) settle(ctx context.Context, d eventing.Delivery) error {
	if err := d.Envelope.Validate(); err != nil {
		return r.bury(ctx, d, eventing.FailureEnvelopeInvalidOnPublish, d.Failures, err)
	}
	if d.Attempt > r.MaxAttempts {
		// Earlier attempts ended unsettled (crash or lost lease) and the attempt budget is spent.
		return r.bury(ctx, d, eventing.FailureClaimLimitExceeded, d.Failures,
			fmt.Errorf("claimed for attempt %d of %d; earlier attempts ended unsettled", d.Attempt, r.MaxAttempts))
	}
	publishCtx, cancel := context.WithTimeout(ctx, r.PublishTimeout)
	err := r.Publisher.Publish(publishCtx, d.Envelope)
	cancel()
	switch {
	case err == nil:
		return r.leaseLostIsSettled(d, r.Outbox.MarkPublished(ctx, d.Ref, r.Clock.Now()))
	case ctx.Err() != nil:
		// Stopping: the lease expires and another worker re-delivers.
		return ctx.Err()
	case errors.Is(err, eventing.ErrPublishRejected):
		r.logger().Warn("outbox delivery rejected by sink", "event_id", d.Envelope.ID, "error", err)
		return r.bury(ctx, d, FailurePublishRejected, d.Failures, err)
	case d.Failures+1 >= r.MaxFailures || d.Attempt >= r.MaxAttempts:
		r.logger().Warn("outbox publish failed; budget spent", "event_id", d.Envelope.ID, "failures", d.Failures+1, "attempt", d.Attempt, "error", err)
		return r.bury(ctx, d, FailurePublishFailed, d.Failures+1, err)
	default:
		r.logger().Warn("outbox publish failed", "event_id", d.Envelope.ID, "failures", d.Failures+1, "error", err)
		failedAt := r.Clock.Now()
		return r.leaseLostIsSettled(d, r.Outbox.RecordFailure(ctx, d.Ref, eventing.DeliveryFailure{
			FailedAt: failedAt, RetryAt: failedAt.Add(r.backoff(d.Failures)), Code: FailurePublishFailed, MaxFailures: r.MaxFailures,
		}))
	}
}

// bury abandons d and stores its dead letter in one transaction, then alerts. failures is the
// failure count after the abandonment.
//
// A publish failure abandons through RecordFailure with the budget set to this failure, so the
// failure is counted, and Bento abandons in the same statement (failures + 1 >= MaxFailures).
// RecordFailure does not report that it abandoned; it does not need to, because only the lease
// holder changes failures, so the outcome is known before the call. Every other cause abandons
// through Abandon.
func (r Relay) bury(ctx context.Context, d eventing.Delivery, code eventing.FailureCode, failures uint32, cause error) error {
	at := r.Clock.Now()
	settle := func(ctx context.Context) error { return r.Outbox.Abandon(ctx, d.Ref, at, code) }
	if failures > d.Failures {
		settle = func(ctx context.Context) error {
			return r.Outbox.RecordFailure(ctx, d.Ref, eventing.DeliveryFailure{
				FailedAt: at, RetryAt: at.Add(r.backoff(d.Failures)), Code: code, MaxFailures: failures,
			})
		}
	}
	alert := DeadLetterAlert{
		Source: d.Ref.Source, EventID: d.Ref.EventID, EventType: d.Envelope.Type, Scope: d.Envelope.Scope,
		Code: code, Attempt: d.Attempt, Failures: failures, DeadAt: kernel.Instant(at), Critical: r.critical(d.Envelope.Type),
	}
	if cause != nil {
		alert.Error = cutText(cause.Error(), maxErrorBytes)
	}
	scope, scopeErr := kernel.ParseScope(d.Envelope.Scope)
	var err error
	if scopeErr == nil {
		alert.Stored = true
		err = r.DeadLetters.Bury(ctx, DeadLetter{
			Scope: scope, Source: d.Ref.Source, EventID: d.Ref.EventID, Envelope: d.Envelope,
			Attempt: d.Attempt, Failures: failures, Code: code, LastError: alert.Error, Critical: alert.Critical, DeadAt: at,
		}, settle)
	} else {
		// RLS cannot hold a letter without a scope. Abandon anyway: retrying cannot fix the scope.
		alert.Critical = true
		alert.Error = cutText(strings.TrimPrefix(alert.Error+"; no dead letter: "+scopeErr.Error(), "; "), maxErrorBytes)
		err = settle(ctx)
	}
	if err != nil {
		return r.leaseLostIsSettled(d, fmt.Errorf("messaging: abandon %s: %w", d.Ref.EventID, err))
	}
	r.alerter().Alert(context.WithoutCancel(ctx), alert)
	return nil
}

// leaseLostIsSettled treats a lost lease as settled: another worker owns the delivery now.
func (r Relay) leaseLostIsSettled(d eventing.Delivery, err error) error {
	if errors.Is(err, eventing.ErrLeaseLost) {
		r.logger().Warn("outbox lease lost", "event_id", d.Envelope.ID)
		return nil
	}
	return err
}

func (r Relay) backoff(failures uint32) time.Duration {
	delay := r.RetryBase
	for i := uint32(0); i < failures && delay < r.RetryMax; i++ {
		delay *= 2
	}
	return min(delay, r.RetryMax)
}

func (r Relay) validate() error {
	if r.Outbox == nil || r.DeadLetters == nil || r.Publisher == nil || r.Clock == nil || r.Batch < 1 || r.Batch > eventing.MaxOutboxClaimLimit ||
		r.LeaseFor <= 0 || r.PublishTimeout <= 0 || r.MaxAttempts == 0 || r.MaxFailures == 0 || r.IdleWait <= 0 {
		return errors.New("messaging: relay misconfigured")
	}
	if time.Duration(r.Batch)*r.PublishTimeout >= r.LeaseFor {
		return errors.New("messaging: relay lease must outlast Batch × PublishTimeout")
	}
	return nil
}

func (r Relay) critical(t eventing.EventType) bool {
	if r.Critical == nil {
		return CriticalEventType(t)
	}
	return r.Critical(t)
}

func (r Relay) alerter() Alerter {
	if r.Alerter == nil {
		return SlogAlerter{Logger: r.logger()}
	}
	return r.Alerter
}

func (r Relay) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.Default()
	}
	return r.Logger
}
