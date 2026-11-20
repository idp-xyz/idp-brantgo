package bootstrap

import (
	"errors"
	"log/slog"
	"time"

	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
)

// NewRelayMetrics returns the metrics of one worker process (S5R-D07): give them to
// RelayConfig.Metrics, to the Dispatcher's SetMetrics and to MonitorConfig.Metrics, and serve their
// Handler where the worker exposes metrics (brant-worker -metrics-listen).
func NewRelayMetrics() *messaging.RelayMetrics { return messaging.NewRelayMetrics() }

// MonitorConfig configures the Outbox backlog monitor of a worker (S5R-D07).
type MonitorConfig struct {
	Metrics  *messaging.RelayMetrics // receives the backlog gauges; nil logs the alerts only
	Interval time.Duration           // between two reads of the backlog
	Alert    time.Duration           // age of the oldest undelivered event that alerts; 0 means messaging.OldestUndeliveredAlert
	Incident time.Duration           // age that is an incident; 0 means messaging.OldestUndeliveredIncident
	Logger   *slog.Logger
}

// RelayMonitor assembles the Outbox backlog monitor on a transaction owner of its own. It reads the
// Outbox of the Bento schema the platform was opened with, as the relay does.
func (p *Platform) RelayMonitor(cfg MonitorConfig) (messaging.RelayMonitor, error) {
	if cfg.Interval <= 0 {
		return messaging.RelayMonitor{}, errors.New("bootstrap: the relay monitor needs an interval")
	}
	if cfg.Alert == 0 {
		cfg.Alert = messaging.OldestUndeliveredAlert
	}
	if cfg.Incident == 0 {
		cfg.Incident = messaging.OldestUndeliveredIncident
	}
	if cfg.Alert < 0 || cfg.Incident <= cfg.Alert {
		return messaging.RelayMonitor{}, errors.New("bootstrap: the relay monitor needs 0 < alert < incident")
	}
	db, err := p.newDB()
	if err != nil {
		return messaging.RelayMonitor{}, err
	}
	return messaging.RelayMonitor{
		Probe:   messaging.OutboxProbe{DB: db, Clock: bento.SystemClock{}},
		Metrics: cfg.Metrics, Interval: cfg.Interval, Alert: cfg.Alert, Incident: cfg.Incident, Logger: cfg.Logger,
	}, nil
}
