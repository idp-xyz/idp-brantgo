package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
)

// OutboxBacklog is the undelivered part of the Outbox at one instant: every tenant's events, since a
// relay serves them all. Abandoned deliveries (dead letters) are not part of it (S5R-D07).
type OutboxBacklog struct {
	At           time.Time // when it was read, on the probe's clock
	Pending      int64     // waiting to be claimed and never failed: the relay has not reached them yet
	Retrying     int64     // waiting for the retry after a failed publish
	Leased       int64     // claimed by a worker that holds the lease
	LeaseExpired int64     // claimed, but the lease ran out: the worker stopped or stalled; another re-claims it
	Oldest       time.Time // recorded_at of the oldest undelivered event; zero when there is none
}

// Events is the number of undelivered events.
func (b OutboxBacklog) Events() int64 { return b.Pending + b.Retrying + b.Leased + b.LeaseExpired }

// OldestAge is how long the oldest undelivered event has waited at At, 0 when there is none.
func (b OutboxBacklog) OldestAge() time.Duration {
	if b.Oldest.IsZero() || b.At.Before(b.Oldest) {
		return 0
	}
	return b.At.Sub(b.Oldest)
}

// OutboxProbe reads the OutboxBacklog of the Bento Outbox in DB's schema. The Outbox has no RLS;
// the application role reads it as the relay claims from it. The rows are those of Bento's index on
// undelivered events (outbox_active_recorded_idx), so the cost follows the backlog, not the table.
type OutboxProbe struct {
	DB    *bentopg.DB
	Clock application.Clock
}

// Read returns the backlog now. It runs outside any transaction.
func (p OutboxProbe) Read(ctx context.Context) (OutboxBacklog, error) {
	if p.DB == nil || p.Clock == nil {
		return OutboxBacklog{}, errors.New("messaging: outbox probe misconfigured")
	}
	q, err := p.DB.ReadExecutor(ctx)
	if err != nil {
		return OutboxBacklog{}, err
	}
	b := OutboxBacklog{At: p.Clock.Now()}
	var oldest *time.Time
	err = q.QueryRow(ctx, fmt.Sprintf(`SELECT
		count(*) FILTER (WHERE status = 'PENDING' AND failures = 0),
		count(*) FILTER (WHERE status = 'PENDING' AND failures > 0),
		count(*) FILTER (WHERE status = 'LEASED' AND lease_expires_at > $1),
		count(*) FILTER (WHERE status = 'LEASED' AND lease_expires_at <= $1),
		min(recorded_at)
		FROM %s.outbox WHERE status IN ('PENDING', 'LEASED')`, pgx.Identifier{p.DB.Schema()}.Sanitize()), b.At).
		Scan(&b.Pending, &b.Retrying, &b.Leased, &b.LeaseExpired, &oldest)
	if err != nil {
		return OutboxBacklog{}, fmt.Errorf("messaging: read the outbox backlog: %w", err)
	}
	if oldest != nil {
		b.Oldest = oldest.UTC()
	}
	return b, nil
}

// BacklogReader reads the Outbox backlog; OutboxProbe is the PostgreSQL one.
type BacklogReader interface {
	Read(ctx context.Context) (OutboxBacklog, error)
}

// RelayMonitor reads the Outbox backlog every Interval into Metrics and logs when the age of the
// oldest undelivered event reaches Alert (WARN) or Incident (ERROR), and when it falls below Alert
// again (INFO): once per change of level, so a lasting lag is not logged every Interval (S5R-D07).
// A probe that fails is logged at WARN and counted. Every worker runs one; they read the same
// Outbox, so their gauges agree and each of them alerts.
type RelayMonitor struct {
	Probe    BacklogReader
	Metrics  *RelayMetrics // nil keeps no gauges; the alerts are still logged
	Interval time.Duration
	Alert    time.Duration
	Incident time.Duration
	Logger   *slog.Logger
}

// lagLevel is where the oldest undelivered event's age stands against the thresholds.
type lagLevel uint8

const (
	lagNormal lagLevel = iota
	lagAlert
	lagIncident
)

func (l lagLevel) String() string {
	switch l {
	case lagAlert:
		return "alert"
	case lagIncident:
		return "incident"
	}
	return "normal"
}

// Run probes until ctx is canceled and then returns ctx.Err().
func (m RelayMonitor) Run(ctx context.Context) error {
	if err := m.validate(); err != nil {
		return err
	}
	level := lagNormal
	for {
		level = m.check(ctx, level)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.Interval):
		}
	}
}

func (m RelayMonitor) validate() error {
	if m.Probe == nil || m.Interval <= 0 || m.Alert <= 0 || m.Incident <= m.Alert {
		return errors.New("messaging: relay monitor misconfigured (probe, interval, 0 < alert < incident)")
	}
	return nil
}

// check probes once and returns the level after it; a failed probe keeps the level.
func (m RelayMonitor) check(ctx context.Context, prev lagLevel) lagLevel {
	pctx, cancel := context.WithTimeout(ctx, m.Interval)
	b, err := m.Probe.Read(pctx)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			m.Metrics.probeFailed()
			m.logger().Warn("outbox probe failed", "error", err)
		}
		return prev
	}
	m.Metrics.observeBacklog(b)
	age := b.OldestAge()
	level := lagNormal
	switch {
	case age >= m.Incident:
		level = lagIncident
	case age >= m.Alert:
		level = lagAlert
	}
	if level == prev {
		return level
	}
	args := []any{"lag_level", level.String(), "oldest_undelivered_age_s", age.Seconds(), "oldest_recorded_at", b.Oldest,
		"backlog", b.Events(), "pending", b.Pending, "retrying", b.Retrying, "leased", b.Leased, "lease_expired", b.LeaseExpired,
		"alert_s", m.Alert.Seconds(), "incident_s", m.Incident.Seconds()}
	switch level {
	case lagIncident:
		m.logger().Error("outbox relay lag incident: the oldest undelivered event is older than the incident threshold", args...)
	case lagAlert:
		m.logger().Warn("outbox relay lag alert: the oldest undelivered event is older than the alert threshold", args...)
	default:
		m.logger().Info("outbox relay lag recovered: the oldest undelivered event is younger than the alert threshold", args...)
	}
	return level
}

func (m RelayMonitor) logger() *slog.Logger {
	if m.Logger == nil {
		return slog.Default()
	}
	return m.Logger
}
