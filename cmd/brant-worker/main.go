// brant-worker relays committed Outbox events to an HTTP sink (-sink) or, with -dispatch, in
// process to the consumers the composed contexts subscribe, each through the Inbox in its context's
// own transaction. Delivery is at least once: consumers deduplicate by event ID.
//
//	brant-worker -sink https://...   POST each envelope with its event ID as Idempotency-Key
//	brant-worker -dispatch           deliver to the subscriptions of every context (messaging.Dispatcher)
//	brant-worker -agent ...          run the AI agent runner (S4: internal/ai/execution), alone or beside
//	                                 -sink or -dispatch
//
// With -dispatch, an event no context subscribes to is settled as delivered; a consumer added later
// never sees it. Each consumer's system actor system:inbox.<consumer> needs its grant in every
// company it serves (brant-admin inbox).
//
// -agent acts as the AI actors of -agent-id in the companies of -agent-scope (tenant/company,
// comma-separated) on the tasks they hold delegations for, every -agent-poll. The model is
// -model scripted (the default, the only one in CI: the turns of the -model-script JSON file) or
// -model openaicompat (an OpenAI-compatible service at -model-base-url, model -model-name; the
// optional bearer key is read from the environment variable -model-key-env, never from a flag, and
// never logged). The runner's system actor system:ai.runner needs its grant in every company
// (brant-admin agent), each agent its role template (brant-admin assign ... -roles ai_q01_agent).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
)

// agentFlags are the flags of -agent.
type agentFlags struct {
	on                                  *bool
	scopes, agents, accepts             *string
	poll, timeout                       *time.Duration
	steps                               *int
	kind, script, name, baseURL, keyEnv *string
}

func defineAgentFlags() agentFlags {
	return agentFlags{
		on:      flag.Bool("agent", false, "run the AI agent runner"),
		scopes:  flag.String("agent-scope", "", "tenant/company pairs the runner serves, comma-separated"),
		agents:  flag.String("agent-id", "", "AI actor IDs the runner acts as, comma-separated"),
		accepts: flag.String("agent-accept-bundles", "", "SHA-256s of earlier ExecutionBundles the runner still runs, comma-separated"),
		poll:    flag.Duration("agent-poll", 5*time.Second, "wait between passes over the runner's tasks"),
		steps:   flag.Int("agent-steps", 8, "model turns per task and pass"),
		kind:    flag.String("model", "scripted", "model of the runner: scripted or openaicompat"),
		script:  flag.String("model-script", "", "JSON script of the scripted model's turns"),
		name:    flag.String("model-name", "", "model name (default scripted-q01-v1 or qwen3.8-flash-next-iq3_xxs)"),
		baseURL: flag.String("model-base-url", "http://127.0.0.1:8080", "base URL of the OpenAI-compatible service"),
		keyEnv:  flag.String("model-key-env", "IDP_BRANT_MODEL_API_KEY", "environment variable holding the service's optional bearer key"),
		timeout: flag.Duration("model-timeout", 2*time.Minute, "timeout of one model call"),
	}
}

func (f agentFlags) config(logger *slog.Logger) (bootstrap.AgentConfig, error) {
	scopes, err := bootstrap.ParseScopes(*f.scopes)
	if err != nil {
		return bootstrap.AgentConfig{}, err
	}
	agents := split(*f.agents)
	if len(agents) == 0 {
		return bootstrap.AgentConfig{}, errors.New("-agent needs -agent-id")
	}
	mc := bootstrap.ModelConfig{Kind: *f.kind, Name: *f.name, BaseURL: *f.baseURL, APIKey: os.Getenv(*f.keyEnv), Timeout: *f.timeout}
	if mc.Kind == bootstrap.ModelScripted {
		if *f.script == "" {
			return bootstrap.AgentConfig{}, errors.New("-model scripted needs -model-script")
		}
		if mc.Script, err = os.ReadFile(*f.script); err != nil {
			return bootstrap.AgentConfig{}, err
		}
	}
	m, profile, err := bootstrap.NewModel(mc)
	if err != nil {
		return bootstrap.AgentConfig{}, err
	}
	return bootstrap.AgentConfig{Scopes: scopes, Agents: agents, Model: m, Profile: profile, Accepts: split(*f.accepts),
		MaxSteps: *f.steps, Logger: logger}, nil
}

func split(list string) []string {
	var out []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

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
	af := defineAgentFlags()
	flag.Parse()
	dsn := os.Getenv("IDP_BRANT_POSTGRES_DSN")
	relaying := *sink != "" || *dispatch
	if dsn == "" || (*sink != "" && *dispatch) || (!relaying && !*af.on) {
		return errors.New("IDP_BRANT_POSTGRES_DSN and one of -sink and -dispatch, or -agent, are required")
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
	var contexts *bootstrap.Contexts
	if *dispatch || *af.on {
		if contexts, err = platform.Contexts(); err != nil {
			return err
		}
	}
	done := make(chan error, 2)
	running := 0
	if *af.on {
		acfg, err := af.config(logger)
		if err != nil {
			return err
		}
		runner, err := contexts.Agent(acfg)
		if err != nil {
			return err
		}
		if err = runner.Init(ctx); err != nil {
			return err
		}
		ref := runner.Ref()
		logger.Info("running agents", "agents", acfg.Agents, "scopes", len(acfg.Scopes), "model", *af.kind, "profile", acfg.Profile.ProfileID,
			"bundle", ref.BundleID, "bundle_sha256", ref.SHA256, "poll", *af.poll)
		running++
		go func() { done <- runner.Run(ctx, *af.poll) }()
	}
	if !relaying {
		err = <-done
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		logger.Info("stopped")
		return nil
	}
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
		dispatcher, err := contexts.Dispatcher()
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
	running++
	go func() { done <- relay.Run(ctx) }()
	// The first of the relay and the runner to end ends the process.
	err = <-done
	stop()
	for range running - 1 {
		if e := <-done; err == nil || errors.Is(err, context.Canceled) {
			err = e
		}
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("stopped")
	return nil
}
