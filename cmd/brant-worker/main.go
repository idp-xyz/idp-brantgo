// brant-worker relays committed Outbox events to an HTTP sink (-sink) or, with -dispatch, in
// process to the consumers the composed contexts subscribe, each through the Inbox in its context's
// own transaction. Delivery is at least once: consumers deduplicate by event ID.
//
//	brant-worker -sink https://...   POST each envelope with its event ID as Idempotency-Key
//	brant-worker -dispatch           deliver to the subscriptions of every context (messaging.Dispatcher)
//
// With -dispatch, an event no context subscribes to is settled as delivered; a consumer added later
// never sees it. Each consumer's system actor system:inbox.<consumer> needs its grant in every
// company it serves (brant-admin inbox).
//
// Every -probe-interval the worker reads the Outbox backlog and the age of the oldest undelivered
// event, and logs a WARN when that age reaches -lag-alert and an ERROR at -lag-incident (S5R-D07:
// 60 s and 300 s, provisional until a real owner confirms them). With -metrics-listen it serves
// GET /metrics in the Prometheus text format: relay latency, backlog, oldest undelivered age,
// failures, dead letters and, with -dispatch, each consumer's outcomes. The reference deployment
// runs at least two -dispatch workers (ops/runbook-outbox-relay.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "brant-worker:", err)
		os.Exit(1)
	}
}

func run() error {
	schema := flag.String("schema", bentopg.DefaultSchema, "Bento schema that holds the outbox")
	sink := flag.String("sink", "", "HTTP endpoint that receives each event envelope as a JSON POST")
	dispatch := flag.Bool("dispatch", false, "deliver each event in process to the consumers the contexts subscribe, instead of -sink")
	batch := flag.Int("batch", 10, "deliveries claimed per round")
	lease := flag.Duration("lease", time.Minute, "lease on a claimed batch; an unsettled delivery is re-claimed after it expires")
	poll := flag.Duration("poll", time.Second, "wait between claims while the outbox is idle")
	maxAttempts := flag.Uint("max-attempts", 50, "claims of one delivery before it is abandoned; keep well above -max-failures (Bento suggests 5x)")
	maxFailures := flag.Uint("max-failures", 10, "recorded publish failures before a delivery is abandoned")
	retryBase := flag.Duration("retry-base", time.Second, "delay after the first publish failure, doubled per failure")
	retryMax := flag.Duration("retry-max", 5*time.Minute, "upper bound for the retry delay")
	sinkTimeout := flag.Duration("sink-timeout", 5*time.Second, "timeout for one sink request, or with -dispatch for one event's consumers together")
	metricsListen := flag.String("metrics-listen", "", "address that serves GET /metrics in the Prometheus text format, such as 127.0.0.1:9464; empty serves none")
	probeInterval := flag.Duration("probe-interval", 15*time.Second, "how often the outbox backlog and the oldest undelivered event's age are read; 0 reads neither and raises no lag alert")
	lagAlert := flag.Duration("lag-alert", 60*time.Second, "age of the oldest undelivered event that logs a lag alert (S5R-D07, provisional)")
	lagIncident := flag.Duration("lag-incident", 300*time.Second, "age of the oldest undelivered event that logs a lag incident (S5R-D07, provisional)")
	flag.Parse()
	dsn := os.Getenv("IDP_BRANT_POSTGRES_DSN")
	if dsn == "" || (*sink != "") == *dispatch {
		return errors.New("IDP_BRANT_POSTGRES_DSN and one of -sink and -dispatch are required")
	}
	// A batch is sent one delivery at a time under one lease. If the lease can end before the last
	// delivery is sent, another worker re-claims deliveries that are still queued here.
	if time.Duration(*batch)*(*sinkTimeout) >= *lease {
		return errors.New("-lease must be longer than -batch × -sink-timeout")
	}
	if *probeInterval < 0 || (*probeInterval > 0 && (*lagAlert <= 0 || *lagIncident <= *lagAlert)) {
		return errors.New("-probe-interval must not be negative, and 0 < -lag-alert < -lag-incident")
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	platform, err := bootstrap.Open(ctx, bootstrap.Config{PostgresDSN: dsn, BentoSchema: *schema})
	if err != nil {
		return err
	}
	defer platform.Close()
	metrics := bootstrap.NewRelayMetrics()
	cfg := bootstrap.RelayConfig{
		SinkURL:        *sink,
		Batch:          *batch,
		LeaseFor:       *lease,
		PublishTimeout: *sinkTimeout,
		MaxAttempts:    uint32(*maxAttempts),
		MaxFailures:    uint32(*maxFailures),
		RetryBase:      *retryBase,
		RetryMax:       *retryMax,
		IdleWait:       *poll,
		Logger:         logger,
		Metrics:        metrics,
	}
	subscriptions := 0
	if *dispatch {
		dispatcher, err := platform.Dispatcher()
		if err != nil {
			return err
		}
		dispatcher.SetMetrics(metrics)
		cfg.Publisher, subscriptions = dispatcher, len(dispatcher.Subscriptions())
	}
	relay, err := platform.Relay(cfg)
	if err != nil {
		return err
	}
	var background sync.WaitGroup
	defer background.Wait()
	bgCtx, stopBackground := context.WithCancel(ctx)
	defer stopBackground()
	if *probeInterval > 0 {
		monitor, err := platform.RelayMonitor(bootstrap.MonitorConfig{
			Metrics: metrics, Interval: *probeInterval, Alert: *lagAlert, Incident: *lagIncident, Logger: logger,
		})
		if err != nil {
			return err
		}
		background.Go(func() { _ = monitor.Run(bgCtx) })
	}
	if *metricsListen != "" {
		ln, err := net.Listen("tcp", *metricsListen)
		if err != nil {
			return fmt.Errorf("-metrics-listen: %w", err)
		}
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", metrics.Handler())
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		background.Go(func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("metrics server stopped", "error", err)
			}
		})
		background.Go(func() {
			<-bgCtx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		})
		logger.Info("serving metrics", "address", ln.Addr().String(), "path", "/metrics")
	}
	logger.Info("relaying outbox", "schema", *schema, "sink", *sink, "dispatch", *dispatch, "subscriptions", subscriptions, "lease", *lease,
		"probe_interval", *probeInterval, "lag_alert", *lagAlert, "lag_incident", *lagIncident)
	if err = relay.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("stopped")
	return nil
}
