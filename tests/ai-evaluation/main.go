// Command ai-evaluation is Brant's Q01 evaluation suite (WBS S4-07a; product baseline 19.1 EvalRun,
// 19.2 CT01–CT16, 9.1 AN01–AN12; core design BC-AT27, BC-AT28). It is a black-box client: it imports
// no internal package (AC6) and states the contract values it uses itself.
//
// It starts the built processes as an operator runs them — brant-migrate -apply, brant-api behind a
// local OpenID Connect issuer (go-jose), one brant-worker -dispatch for the run and one brant-worker
// -agent per agent phase of a trial — against a PostgreSQL 16 database of its own, provisions one
// fresh tenant per trial with brant-admin and builds the synthetic BRC-001 world in it over HTTP. Each
// trial is one Q01 task of a scenario family, from the customer's inquiry file (synthetic text, XLSX
// or PDF generated here) to the family's end: the AI agent's steps run in the agent runner (scripted
// model in the scripted suite, an OpenAI-compatible model service in the live suite), the people's
// steps are a scripted human operator with fixed truth (every one synthetic, never an AI completion,
// decision E-7), and the trial is graded on the authoritative records read back over HTTP — TSK's
// task, actions and verifications, TRD's quote, order and goal readiness, INQ's RFQ and corrections,
// brant_ai's model-call ledger — never on what the model said.
//
// Every trial is kept in the EvalRun registry (report.json, summary.md) with its phases, human steps,
// corrections, cost and assertions; families report first success and repeat consistency.
//
// The task names the uploaded inquiry file as its source (kind evd_file) and the agent registers the
// extract itself, in both suites; which source it registered — and which response and order it
// created — the operator and the graders read from the task's records, so a live model may name them
// as it likes (learn.go). An agent phase that fails, or asks the model and commits nothing, records
// why and the last ledger calls with their codes. With -trace (on by default in the live suite, never
// under CI=true) the agent workers also write their local diagnostic trace of every turn under the
// trial's directory: what the model was told and what it sent, to classify a failed live trial. That
// trace is not evidence.
//
//	go run ./tests/ai-evaluation -bin bin -out bin/ai-eval/run1                      # scripted suite (make ai-eval)
//	go run ./tests/ai-evaluation -families normal,an09 -trials 2 -parallel 2 ...
//	go run -tags aievallive ./tests/ai-evaluation -suite live ...                    # live suite (make ai-eval-live)
//
// Exit status: 0 every trial met its expected outcome (scripted) or the live run completed; 1 a
// scripted trial missed its expected outcome; 2 the harness could not run; 3 the live suite was not
// executed (no model key in the environment).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	suiteScripted = "scripted"
	suiteLive     = "live"

	exitFailed      = 1
	exitHarness     = 2
	exitNotExecuted = 3
)

type config struct {
	suite              string
	bin, dsn, migrator string
	out                string
	families           []string
	trials, parallel   int
	phaseTimeout       time.Duration
	longWait           time.Duration
	archive            string
	live               liveConfig
	// trace: the agent workers write their local diagnostic trace under each trial's directory
	// (-trace on, or auto in the live suite; never under CI=true).
	trace bool
}

// Default phase timeouts (-phase-timeout 0). The scripted model answers at once. A local model
// answers in 6–30 s per call, and a task's budget is 40 model calls: at 30 s each that is 20
// minutes. 25 minutes let the task's own budget, not the harness, end a phase that keeps calling the
// model (the task pauses budget_exhausted), so a phase that reaches the timeout has stopped
// answering.
const (
	scriptedPhaseTimeout = 2 * time.Minute
	livePhaseTimeout     = 25 * time.Minute
)

// Values of -trace.
const (
	traceAuto = "auto"
	traceOn   = "on"
	traceOff  = "off"
)

// liveConfig is the model service of the live suite (decisions E-3/E-5: an OpenAI-compatible service,
// its key only in the environment).
type liveConfig struct {
	baseURL, model, keyEnv string
	timeout                time.Duration
}

// workerArgs are brant-worker's model flags for the live suite; the key is read by the worker from
// the environment variable keyEnv names, which it inherits.
func (l liveConfig) workerArgs() []string {
	return []string{"-model", "openaicompat", "-model-base-url", l.baseURL, "-model-name", l.model, "-model-key-env", l.keyEnv,
		"-model-timeout", l.timeout.String()}
}

func (l liveConfig) workerEnv() []string { return nil }

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "ai-evaluation:", err)
		os.Exit(exitHarness)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, cfg))
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("ai-evaluation", flag.ContinueOnError)
	cfg := config{}
	var families, trace string
	fs.StringVar(&cfg.suite, "suite", suiteScripted, "scripted (the scripted model; CI and make ai-eval) or live (an OpenAI-compatible model service; build tag aievallive)")
	fs.StringVar(&cfg.bin, "bin", "bin", "directory holding brant-api, brant-worker, brant-admin and brant-migrate (make build)")
	fs.StringVar(&cfg.dsn, "dsn", os.Getenv("IDP_BRANT_POSTGRES_DSN"), "application role DSN of the evaluation's own database")
	fs.StringVar(&cfg.migrator, "migrator-dsn", os.Getenv("IDP_BRANT_MIGRATOR_DSN"), "migrator role DSN of the same database (brant-migrate -apply)")
	fs.StringVar(&cfg.out, "out", filepath.Join("bin", "ai-eval", time.Now().UTC().Format("20060102T150405Z")), "directory for report.json, summary.md, the trials' files and the processes' logs")
	fs.StringVar(&families, "families", "all", "families to run, comma-separated ("+strings.Join(familyNames(), ",")+"), or all")
	fs.IntVar(&cfg.trials, "trials", 5, "trials per family (decision E-4: the fixed set is 11 families × 5 trials)")
	fs.IntVar(&cfg.parallel, "parallel", 4, "trials run at the same time (each in its own tenant)")
	fs.DurationVar(&cfg.phaseTimeout, "phase-timeout", 0, "longest an agent phase may take before the trial fails; 0 is the suite's default: "+
		scriptedPhaseTimeout.String()+" scripted, "+livePhaseTimeout.String()+" live (a local model answers in 6–30 s per call)")
	fs.StringVar(&trace, "trace", traceAuto, "the agent workers' local diagnostic trace of every turn under trials/<trial>/trace/ (facts, model replies, "+
		"tool results in clear; not evidence): on, off, or auto (on in the live suite, off in the scripted one); never under CI=true")
	fs.DurationVar(&cfg.longWait, "long-wait", 5*time.Second, "AN06: how long the processes stay down while the task waits")
	fs.StringVar(&cfg.archive, "archive", "docs", "the master archive (role templates are read from it)")
	fs.StringVar(&cfg.live.baseURL, "model-base-url", "http://127.0.0.1:8080", "live suite: base URL of the OpenAI-compatible model service")
	fs.StringVar(&cfg.live.model, "model-name", "qwen3.8-flash-next-iq3_xxs", "live suite: model name")
	fs.StringVar(&cfg.live.keyEnv, "model-key-env", "IDP_BRANT_MODEL_API_KEY", "live suite: environment variable holding the service's key")
	fs.DurationVar(&cfg.live.timeout, "model-timeout", 2*time.Minute, "live suite: timeout of one model call")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	switch {
	case cfg.suite != suiteScripted && cfg.suite != suiteLive:
		return cfg, fmt.Errorf("-suite must be scripted or live, got %q", cfg.suite)
	case cfg.suite == suiteLive && !liveBuilt:
		return cfg, errors.New("the live suite is built with -tags aievallive (make ai-eval-live)")
	case cfg.dsn == "" || cfg.migrator == "":
		return cfg, errors.New("-dsn and -migrator-dsn (IDP_BRANT_POSTGRES_DSN, IDP_BRANT_MIGRATOR_DSN) are required")
	case cfg.trials < 1 || cfg.parallel < 1:
		return cfg, errors.New("-trials and -parallel must be at least 1")
	case cfg.phaseTimeout < 0:
		return cfg, errors.New("-phase-timeout must not be negative")
	}
	if cfg.phaseTimeout == 0 {
		cfg.phaseTimeout = scriptedPhaseTimeout
		if cfg.suite == suiteLive {
			cfg.phaseTimeout = livePhaseTimeout
		}
	}
	ci := os.Getenv("CI") == "true"
	switch trace {
	case traceOn:
		if ci {
			return cfg, errors.New("-trace on never runs under CI=true: the agent trace is a local diagnostic")
		}
		cfg.trace = true
	case traceAuto:
		cfg.trace = cfg.suite == suiteLive && !ci
	case traceOff:
	default:
		return cfg, fmt.Errorf("-trace must be on, off or auto, got %q", trace)
	}
	var err error
	if cfg.families, err = selectFamilies(families); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// run runs the suite and returns the exit status; the registry is written whatever happened.
func run(ctx context.Context, cfg config) int {
	if err := os.MkdirAll(cfg.out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "ai-evaluation:", err)
		return exitHarness
	}
	r := newEvalRun(cfg)
	families := familiesOf(cfg.families)
	finish := func(code int) int {
		r.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		r.Environment.Load1End = load1()
		r.summarize(families)
		if err := r.write(cfg.out); err != nil {
			fmt.Fprintln(os.Stderr, "ai-evaluation: write the registry:", err)
			return exitHarness
		}
		fmt.Print(r.markdown())
		return code
	}
	if cfg.suite == suiteLive && os.Getenv(cfg.live.keyEnv) == "" {
		r.NotExecuted = append([]string{"the live suite: " + cfg.live.keyEnv + " is not set, so no model call was made and no trial ran " +
			"(the service at " + cfg.live.baseURL + " requires a key)"}, r.NotExecuted...)
		fmt.Fprintln(os.Stderr, "ai-evaluation: NOT EXECUTED:", r.NotExecuted[0])
		return finish(exitNotExecuted)
	}
	started := time.Now()
	r.Environment.Load1Start = load1()
	e, err := startEnv(ctx, cfg, r)
	r.StartupSeconds = round(time.Since(started).Seconds(), 2)
	if e != nil {
		defer e.close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ai-evaluation:", err)
		r.Environment.Error = err.Error()
		return finish(exitHarness)
	}

	type job struct {
		f *family
		n int
	}
	var jobs []job
	for _, f := range families {
		for n := 1; n <= cfg.trials; n++ {
			jobs = append(jobs, job{f, n})
		}
	}
	r.Trials = make([]*TrialRecord, len(jobs))
	queue := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < cfg.parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				j := jobs[i]
				r.Trials[i] = runTrial(ctx, e, j.f, j.n)
				fmt.Fprintf(os.Stderr, "trial %s: %s pass=%v %.1f s %s\n", r.Trials[i].TrialID, r.Trials[i].Outcome, r.Trials[i].Pass,
					r.Trials[i].Seconds, r.Trials[i].Error)
			}
		}()
	}
	for i := range jobs {
		if ctx.Err() != nil {
			break
		}
		queue <- i
	}
	close(queue)
	wg.Wait()
	var kept []*TrialRecord
	for _, t := range r.Trials {
		if t != nil {
			kept = append(kept, t)
		}
	}
	r.Trials = kept
	r.Probes = append(r.Probes, e.probes()...)
	if ctx.Err() != nil {
		r.Environment.Error = "interrupted: " + ctx.Err().Error()
		return finish(exitHarness)
	}
	failed := 0
	for _, t := range r.Trials {
		if !t.Pass {
			failed++
		}
	}
	if failed > 0 && cfg.suite == suiteScripted {
		fmt.Fprintf(os.Stderr, "ai-evaluation: %d of %d trials missed their expected outcome\n", failed, len(r.Trials))
		return finish(exitFailed)
	}
	return finish(0)
}

// newEvalRun is the registry entry of the run with everything fixed before the first trial.
func newEvalRun(cfg config) *EvalRun {
	r := &EvalRun{RunID: filepath.Base(cfg.out), RunTag: strconv.FormatInt(time.Now().UnixMilli(), 36), Tool: "tests/ai-evaluation",
		Suite: cfg.suite, DatasetVersion: datasetVersion,
		Synthetic: true, DataNote: "synthetic BRC-001-derived inquiries (text, XLSX, PDF generated by the harness), configurations and parties; " +
			"decision E-6: no real customer data", Grouping: "one tenant per trial: each trial is its own customer session, nothing is shared between trials",
		InputProfile: "Q01.upload.v0.1 (baseline 14.2): one inquiry file per trial, ≤ 10 MiB, text/plain, PDF with a text layer or XLSX values",
		Truth: "fixed per family before the run (families.go): BRC-001's four lines ST01 red/blue × S/M, 24+36+12+48 = 120 PCS at 8.00 USD = 960.00 USD, " +
			"cost snapshot 660.00 USD; each family's expected outcome",
		Reviewer: "scripted human operator (decision E-7 option a for the fixed set): the owner sales_clerk, quote_reviewer, order_reviewer, " +
			"compliance and credit steps with the fixed truth; every human step and correction is synthetic and never an AI completion",
		TrialsPerFamily: cfg.trials, Budget: Budget{MaxModelCalls: 40, MaxRetries: 3, MaxCost: "3.00", Currency: "USD"},
		StopCondition: "a trial ends when its family's last step is done, when an agent phase exceeds -phase-timeout (" + cfg.phaseTimeout.String() +
			"), or at the first refusal the family does not expect; every trial is kept",
		Graders: []string{"authority assertions on TSK, TRD, INQ and brant_ai records read back over HTTP (never model text)",
			"expected outcome of the family (lifecycle, completion actor, refusals) fixed before the run",
			"repeat consistency: the trials of a family give the same signature (outcome, assertions, phases' model/tool calls, corrections)"},
		FailureTaxonomy: []string{failSetup, failAgentPhase, failHumanStep, failAssertion, failTimeout, failHarness},
		StartedAt:       time.Now().UTC().Format(time.RFC3339),
		NotExecuted: []string{
			"AN12 (real de-identified inquiries with a human baseline): not executable before BC-D01; decision E-6 allows synthetic data only",
			"AN02 (context versions): not in the plan's fixed set (S4-07)",
			"owner-operated trial over HTTP (decision E-7 option c): needs the owner; E1",
		}}
	if cfg.suite == suiteScripted {
		r.Model = ModelInfo{Kind: "scripted", Requested: "scripted-q01-v1", TokensNote: "token counts are the scripted model's synthetic usage",
			CostNote: "cost from the model-call ledger; the scripted profile is free (0.00)"}
		r.NotExecuted = append(r.NotExecuted, "the live suite (make ai-eval-live): a separate run against the model service")
	} else {
		r.Model = ModelInfo{Kind: "openaicompat", Requested: cfg.live.model, BaseURL: cfg.live.baseURL, KeyFromEnvVar: cfg.live.keyEnv,
			TokensNote: "token counts as the service returned them", CostNote: "cost from the model-call ledger at the profile's price list"}
	}
	host, _ := os.Hostname()
	r.Environment = Environment{Host: host, GoVersion: runtime.Version(), OS: runtime.GOOS + "/" + runtime.GOARCH, CPUs: runtime.NumCPU(),
		CI: os.Getenv("CI") == "true", Parallel: cfg.parallel, PhaseTimeout: cfg.phaseTimeout.String(), GitHead: gitHead(),
		AgentTrace: "off",
		Processes:  "brant-migrate -apply; brant-api (local OIDC issuer); brant-worker -dispatch (one); brant-worker -agent (one per agent phase)"}
	if cfg.trace {
		r.Environment.AgentTrace = "on: each agent phase's turns under trials/<trial>/trace/<nn>-<phase>/<tenant>/<company>/<task>/turn-*.jsonl, " +
			"the tools on offer in trials/<trial>/trace/<nn>-<phase>/tools-*.json (brant-worker -agent-trace-dir). A local diagnostic of synthetic " +
			"trials — the prompt, the facts, the model's replies and the tool results in clear, never the model key — to classify failed trials; " +
			"it is not evidence and not a result of this run"
	}
	return r
}

func gitHead() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// datasetVersion names the synthetic dataset: the generators in fixtures.go and families.go.
const datasetVersion = "q01-synthetic-brc001/1"

// load1 is the host's one-minute load average (other work on the host is not controlled).
func load1() float64 {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(raw))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}
