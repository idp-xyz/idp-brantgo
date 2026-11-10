package messaging

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
)

// DeadLetterAlert reports one delivery the relay gave up on (ADR-BR-015).
type DeadLetterAlert struct {
	Source    string
	EventID   eventing.EventID
	EventType eventing.EventType
	Scope     string // as the envelope carries it
	Code      eventing.FailureCode
	Attempt   uint32
	Failures  uint32
	Error     string // the last error, if there was one
	DeadAt    time.Time
	// Critical marks a revocation, repudiation or risk event, and a delivery that was abandoned
	// without a dead letter.
	Critical bool
	// Stored is false when no dead letter could be stored, because the envelope's scope does not
	// parse. The Bento outbox row, abandoned, still holds the envelope.
	Stored bool
}

// Alerter is told about every delivery the relay abandons, after the abandonment commits.
type Alerter interface {
	Alert(ctx context.Context, alert DeadLetterAlert)
}

// SlogAlerter logs each alert at ERROR. A nil Logger means slog.Default().
type SlogAlerter struct {
	Logger *slog.Logger
}

var _ Alerter = SlogAlerter{}

func (a SlogAlerter) Alert(ctx context.Context, alert DeadLetterAlert) {
	logger := a.Logger
	if logger == nil {
		logger = slog.Default()
	}
	msg := "outbox delivery dead-lettered"
	if !alert.Stored {
		msg = "outbox delivery abandoned without a dead letter"
	}
	logger.ErrorContext(ctx, msg, "critical", alert.Critical, "stored", alert.Stored,
		"event_id", alert.EventID, "source", alert.Source, "event_type", alert.EventType, "scope", alert.Scope,
		"failure_code", alert.Code, "attempt", alert.Attempt, "failures", alert.Failures,
		"dead_at", alert.DeadAt, "error", alert.Error)
}

// criticalMarkers are fragments of the event types of revocation, repudiation and risk events.
// ADR-BR-015: such an event is never dropped quietly, and its dead letter alerts as critical.
var criticalMarkers = []string{"revoked", "repudiat", "risk", "withdrawn", "cancelled", "deactivated"}

// CriticalMarkers returns the fragments CriticalEventType looks for.
func CriticalMarkers() []string { return slices.Clone(criticalMarkers) }

// TypeContains returns a predicate that holds when an event type contains one of markers,
// ignoring case. Empty markers are ignored.
func TypeContains(markers ...string) func(eventing.EventType) bool {
	lower := make([]string, 0, len(markers))
	for _, m := range markers {
		if m != "" {
			lower = append(lower, strings.ToLower(m))
		}
	}
	return func(t eventing.EventType) bool {
		s := strings.ToLower(string(t))
		return slices.ContainsFunc(lower, func(m string) bool { return strings.Contains(s, m) })
	}
}

var defaultCritical = TypeContains(criticalMarkers...)

// CriticalEventType is the default predicate for revocation, repudiation and risk events: the
// type contains one of CriticalMarkers.
func CriticalEventType(t eventing.EventType) bool { return defaultCritical(t) }
