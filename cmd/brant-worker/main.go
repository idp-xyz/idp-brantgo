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
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
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
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	platform, err := bootstrap.Open(ctx, bootstrap.Config{PostgresDSN: dsn, BentoSchema: *schema})
	if err != nil {
		return err
	}
	defer platform.Close()
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
	}
	subscriptions := 0
	if *dispatch {
		dispatcher, err := platform.Dispatcher()
		if err != nil {
			return err
		}
		cfg.Publisher, subscriptions = dispatcher, len(dispatcher.Subscriptions())
	}
	relay, err := platform.Relay(cfg)
	if err != nil {
		return err
	}
	logger.Info("relaying outbox", "schema", *schema, "sink", *sink, "dispatch", *dispatch, "subscriptions", subscriptions, "lease", *lease)
	if err = relay.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("stopped")
	return nil
}
