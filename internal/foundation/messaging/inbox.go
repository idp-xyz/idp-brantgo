package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-bento-go/postgres/inbox"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// InboxStore is the Bento inbox on the consuming context's own DB; *inbox.Store implements it.
// Its table is framework-owned: Brant reaches it through these methods only.
type InboxStore interface {
	eventing.InboxStarter
	eventing.InboxCompleter
}

// EventHandler applies one integration event for a consuming context. ctx is bound to the
// consumer's transaction on the context's own DB, to the envelope's scope (FORCE RLS) and to the
// consumer's system Principal (InboxActor), so the context's repositories, called with ctx, join
// that transaction and its effect commits together with the inbox record.
//
// A nil error commits the effect and marks the event processed. RejectEvent refuses the event for
// good: the effect rolls back and the event is marked rejected. Any other error rolls everything
// back and the sender delivers the event again, so a handler need not be idempotent itself.
type EventHandler interface {
	HandleEvent(ctx context.Context, envelope eventing.Envelope) error
}

// EventHandlerFunc adapts a function to EventHandler.
type EventHandlerFunc func(ctx context.Context, envelope eventing.Envelope) error

func (f EventHandlerFunc) HandleEvent(ctx context.Context, envelope eventing.Envelope) error {
	return f(ctx, envelope)
}

// ErrEventRejected marks an event a consumer refused for good; it is never redelivered with
// success.
var ErrEventRejected = errors.New("messaging: event rejected")

// FailureEventRejected is the failure code of a rejection whose handler gave no valid code.
const FailureEventRejected eventing.FailureCode = "event.rejected"

type eventRejection struct {
	code  eventing.FailureCode
	cause error
}

func (r *eventRejection) Error() string {
	if r.cause == nil {
		return fmt.Sprintf("%v (%s)", ErrEventRejected, r.code)
	}
	return fmt.Sprintf("%v (%s): %v", ErrEventRejected, r.code, r.cause)
}

func (r *eventRejection) Unwrap() []error {
	if r.cause == nil {
		return []error{ErrEventRejected}
	}
	return []error{ErrEventRejected, r.cause}
}

// RejectEvent returns a handler's permanent refusal of an event, recorded in the inbox with code
// (lower-case letters, digits, '.', '_' and '-', such as "inq.unknown_rfq"). An invalid code is
// recorded as FailureEventRejected.
//
// Only this error marks an event rejected, and a rejected event stays rejected: a later
// redelivery or dead-letter replay is refused too. Refusals that a grant, a configuration or a
// later event can cure, such as a *kernel.Rejection of PERMISSION_DENIED for the consumer's own
// actor, should be returned as they are, so the event is retried and dead-lettered instead.
func RejectEvent(code eventing.FailureCode, cause error) error {
	if !validFailureCode(code) {
		code = FailureEventRejected
	}
	return &eventRejection{code: code, cause: cause}
}

func validFailureCode(code eventing.FailureCode) bool {
	if len(code) == 0 || len(code) > eventing.MaxFailureCodeLength {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || (i > 0 && (c == '.' || c == '_' || c == '-'))
		if !ok {
			return false
		}
	}
	return true
}

// InboxOutcome is what a consumer made of one delivery.
type InboxOutcome uint8

const (
	// InboxRetry: nothing was settled and the sender delivers the event again later. It is the zero
	// value, so no unexpected path counts as delivered.
	InboxRetry InboxOutcome = iota
	// InboxProcessed: the handler ran; its effect and the processed record committed together.
	InboxProcessed
	// InboxDuplicate: the event was processed before; the handler did not run.
	InboxDuplicate
	// InboxRejected: the event was refused for good, now or before; none of a handler's effect
	// committed.
	InboxRejected
)

func (o InboxOutcome) String() string {
	switch o {
	case InboxProcessed:
		return "processed"
	case InboxDuplicate:
		return "duplicate"
	case InboxRejected:
		return "rejected"
	}
	return "retry"
}

// inboxActorPrefix starts the actor ID of every consumer: system:inbox.<consumer>.
const inboxActorPrefix = "inbox."

// InboxActor returns the actor a consumer processes events as: kind system, ID "inbox.<consumer>"
// (written system:inbox.<consumer>). A consumer name is an identifier of at most 58 characters,
// by convention "<bc>.<purpose>", such as "inq.party_deactivated". Events are never processed as
// the actor who caused them; the payload names that actor for the record only. The consuming
// context grants this actor what its handler needs, like any other actor (brant-admin grant
// -kind system).
func InboxActor(consumer eventing.InboxConsumer) (kernel.ActorRef, error) {
	if !kernel.ValidIdentifier(string(consumer)) {
		return kernel.ActorRef{}, fmt.Errorf("messaging: invalid inbox consumer name %q", consumer)
	}
	return kernel.NewActorRef(kernel.ActorSystem, inboxActorPrefix+string(consumer))
}

// Consumer handles a context's integration events at least once without double effects
// (ADR-BR-015: the relay delivers at least once, consumers deduplicate by event ID).
//
// Each delivery runs in one transaction of the consuming context's own DB, bound to the
// envelope's scope: Start the inbox entry for the consumer, the source and the event ID; if the
// event was processed or rejected before, stop; otherwise run the handler and mark the entry
// processed. The handler's effect and the inbox record commit together or not at all, and a
// redelivery, the same event delivered by two workers, or a dead-letter replay (same event ID)
// runs the handler at most once to completion. A second worker that meets an entry another
// worker holds gets eventing.ErrInboxBusy and InboxRetry.
//
// A rejection (RejectEvent) rolls the handler's transaction back and records the rejection in a
// new transaction, so a refused event leaves no partial effect. An envelope that does not
// validate or whose scope does not parse is rejected without an inbox entry: no scope, no
// transaction under RLS.
type Consumer struct {
	Name  eventing.InboxConsumer
	Inbox InboxStore
	// Transactor opens a transaction bound to a scope on the DB the Inbox store uses; NewConsumer
	// sets bento.ScopeTransactor on that DB. On another DB the inbox calls fail instead of joining.
	Transactor func(kernel.Scope) application.Transactor
	Handler    EventHandler
	Clock      application.Clock
}

// NewConsumer returns the consumer called name of the context that owns db, with the Bento inbox
// and the scope transactor on db. A context builds it in its module with its own DB; bootstrap
// mounts its Receive behind the relay's sink.
func NewConsumer(db *bentopg.DB, name eventing.InboxConsumer, handler EventHandler) (Consumer, error) {
	if db == nil {
		return Consumer{}, errors.New("messaging: consumer needs its context's DB")
	}
	store, err := inbox.NewStore(db)
	if err != nil {
		return Consumer{}, err
	}
	c := Consumer{
		Name: name, Inbox: store, Handler: handler, Clock: bento.SystemClock{},
		Transactor: func(s kernel.Scope) application.Transactor { return bento.ScopeTransactor{DB: db, Scope: s} },
	}
	if _, err = c.validate(); err != nil {
		return Consumer{}, err
	}
	return c, nil
}

func (c Consumer) validate() (kernel.ActorRef, error) {
	if c.Inbox == nil || c.Transactor == nil || c.Handler == nil || c.Clock == nil {
		return kernel.ActorRef{}, errors.New("messaging: consumer misconfigured")
	}
	return InboxActor(c.Name)
}

// Consume handles one delivered envelope. The error is nil for InboxProcessed and InboxDuplicate;
// it wraps ErrEventRejected for InboxRejected and says why for InboxRetry.
func (c Consumer) Consume(ctx context.Context, envelope eventing.Envelope) (InboxOutcome, error) {
	actor, err := c.validate()
	if err != nil {
		return InboxRetry, err
	}
	if err = envelope.Validate(); err != nil {
		return InboxRejected, &eventRejection{code: FailureEventRejected, cause: err}
	}
	scope, err := kernel.ParseScope(envelope.Scope)
	if err != nil {
		return InboxRejected, &eventRejection{code: FailureEventRejected, cause: err}
	}
	// The handler acts as the consumer's system actor in the event's scope, whoever sent the event
	// or called Consume.
	ctx, err = identity.WithPrincipal(ctx, identity.Principal{Actor: actor, Scope: scope})
	if err != nil {
		return InboxRetry, err
	}
	if kernel.ValidIdentifier(envelope.CorrelationID) {
		ctx = action.WithCorrelation(ctx, envelope.CorrelationID)
	}
	key := eventing.InboxKey{Consumer: c.Name, Source: envelope.Source, EventID: envelope.ID}
	tx := c.Transactor(scope)

	var outcome InboxOutcome
	err = tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		received := c.Clock.Now()
		started, err := c.Inbox.Start(txCtx, key, received)
		if err != nil {
			return err
		}
		if !started.Acquired {
			outcome = settled(started.Status)
			return nil
		}
		if err = c.Handler.HandleEvent(txCtx, envelope.Clone()); err != nil {
			return err // rolls back the handler's effect and the inbox entry
		}
		outcome = InboxProcessed
		return c.Inbox.MarkProcessed(txCtx, started.Receipt, later(received, c.Clock.Now()))
	})
	var rejection *eventRejection
	switch {
	case err == nil && outcome == InboxRejected:
		return InboxRejected, fmt.Errorf("%w: %s/%s was rejected before", ErrEventRejected, envelope.Source, envelope.ID)
	case err == nil:
		return outcome, nil
	case errors.As(err, &rejection):
		return c.reject(ctx, tx, key, rejection)
	}
	return InboxRetry, fmt.Errorf("messaging: consume %s/%s: %w", envelope.Source, envelope.ID, err)
}

// reject records the handler's rejection in a transaction of its own, after the handler's
// transaction rolled back. Another worker may have settled the event in between; its outcome
// stands.
func (c Consumer) reject(ctx context.Context, tx application.Transactor, key eventing.InboxKey, rejection *eventRejection) (InboxOutcome, error) {
	var outcome InboxOutcome
	err := tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		received := c.Clock.Now()
		started, err := c.Inbox.Start(txCtx, key, received)
		if err != nil {
			return err
		}
		if !started.Acquired {
			outcome = settled(started.Status)
			return nil
		}
		outcome = InboxRejected
		return c.Inbox.MarkRejected(txCtx, started.Receipt, later(received, c.Clock.Now()), rejection.code)
	})
	switch {
	case err != nil:
		return InboxRetry, fmt.Errorf("messaging: record rejection of %s/%s: %w", key.Source, key.EventID, errors.Join(err, rejection))
	case outcome == InboxRejected:
		return InboxRejected, rejection
	}
	return outcome, nil
}

// settled is the outcome of a delivery that meets an entry an earlier delivery settled.
func settled(status eventing.InboxStatus) InboxOutcome {
	if status == eventing.InboxRejected {
		return InboxRejected
	}
	return InboxDuplicate
}

func later(a, b time.Time) time.Time {
	if b.Before(a) {
		return a
	}
	return b
}

// MaxEnvelopeBytes bounds the body Receive decodes: Bento's largest payload and the envelope
// fields around it.
const MaxEnvelopeBytes = eventing.MaxPayloadBytes + 64<<10

// Verdict tells the sender of a relay POST what became of the delivery.
type Verdict uint8

const (
	// VerdictRetry: nothing was settled; deliver again later (5xx).
	VerdictRetry Verdict = iota
	// VerdictDelivered: processed, now or before (2xx).
	VerdictDelivered
	// VerdictRejected: refused for good; never deliver again (4xx other than 408 and 429).
	VerdictRejected
)

// Reception is the answer to one relay POST. Status is the HTTP status that messaging.Webhook
// reads as Verdict: 200 delivered; 400, 413 or 422 rejected; 503 retry.
type Reception struct {
	Verdict Verdict
	Status  int
	EventID eventing.EventID // empty when the body is not an envelope
	Outcome InboxOutcome
	// Err says why a delivery was rejected or must be retried. It is for the receiver's log, never
	// for the sender.
	Err error
}

// Receive handles one relay POST: body is the envelope as Bento encodes it (messaging.Webhook)
// and idempotencyKey the request's Idempotency-Key header, which must be the envelope's event ID.
// It is the HTTP-agnostic part of an inbound event endpoint; the transport authenticates the
// sender, limits the body to MaxEnvelopeBytes and writes Status.
func (c Consumer) Receive(ctx context.Context, idempotencyKey string, body []byte) Reception {
	if len(body) > MaxEnvelopeBytes {
		return Reception{Verdict: VerdictRejected, Status: http.StatusRequestEntityTooLarge,
			Err: fmt.Errorf("%w: body of %d bytes exceeds %d", ErrEventRejected, len(body), MaxEnvelopeBytes)}
	}
	var envelope eventing.Envelope
	if err := json.Unmarshal(body, &envelope); err != nil { // Bento's decoder validates the envelope
		return Reception{Verdict: VerdictRejected, Status: http.StatusBadRequest, Err: fmt.Errorf("%w: %w", ErrEventRejected, err)}
	}
	if idempotencyKey == "" || idempotencyKey != string(envelope.ID) {
		return Reception{Verdict: VerdictRejected, Status: http.StatusBadRequest, EventID: envelope.ID,
			Err: fmt.Errorf("%w: Idempotency-Key %q is not the event ID", ErrEventRejected, idempotencyKey)}
	}
	outcome, err := c.Consume(ctx, envelope)
	r := Reception{EventID: envelope.ID, Outcome: outcome, Err: err}
	switch outcome {
	case InboxProcessed, InboxDuplicate:
		r.Verdict, r.Status = VerdictDelivered, http.StatusOK
	case InboxRejected:
		r.Verdict, r.Status = VerdictRejected, http.StatusUnprocessableEntity
	default:
		r.Verdict, r.Status = VerdictRetry, http.StatusServiceUnavailable
	}
	return r
}
