package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Report is the machine-readable result of one run (report.json).
type Report struct {
	Tool        string           `json:"tool"`
	Mode        string           `json:"mode"`
	Label       string           `json:"label,omitempty"`
	StartedAt   string           `json:"started_at"`
	FinishedAt  string           `json:"finished_at"`
	Tenant      string           `json:"tenant"`
	Company     string           `json:"company"`
	Environment Environment      `json:"environment"`
	Workload    string           `json:"workload"`
	Setup       SetupStats       `json:"setup"`
	Prime       PrimeStats       `json:"prime"`
	Workers     int              `json:"dispatch_workers"`
	Stages      []StageReport    `json:"stages,omitempty"`
	Writes      *WritesStats     `json:"writes,omitempty"`
	Drain       *DrainStats      `json:"outbox_drain,omitempty"`
	Thresholds  *ThresholdReport `json:"thresholds,omitempty"`
	NotExecuted []string         `json:"not_executed"`
}

// Environment is where the run took place.
type Environment struct {
	Go                 string            `json:"go"`
	OSArch             string            `json:"os_arch"`
	Kernel             string            `json:"kernel"`
	WSL                bool              `json:"wsl"`
	CPUs               int               `json:"nproc"`
	Host               HostState         `json:"host_at_start"`
	PostgreSQL         string            `json:"postgresql"`
	Settings           map[string]string `json:"postgresql_settings,omitempty"`
	APIPool            string            `json:"api_pool"`
	DatabaseBytesStart int64             `json:"database_bytes_start,omitempty"`
	DatabaseBytesEnd   int64             `json:"database_bytes_end,omitempty"`
	Transport          string            `json:"transport"`
	Identity           string            `json:"identity"`
}

// SetupStats is the world's construction (BRC-001's front).
type SetupStats struct {
	Seconds  float64 `json:"seconds"`
	Requests int     `json:"requests"`
}

// PrimeStats is the sequential cycles before the first stage (one user, no concurrency).
type PrimeStats struct {
	Cycles   int        `json:"cycles"`
	Seconds  float64    `json:"seconds"`
	Commands ClassStats `json:"commands"`
	Reads    ClassStats `json:"reads"`
}

// Window is a stage's measurement window: its length on the monotonic clock and the wall clock at
// its edges. ClockStepS is how far the wall clock stepped within it (wall span minus monotonic
// span); a step between an event's recorded_at and published_at adds to that event's relay lag.
type Window struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	Seconds    float64 `json:"seconds"`
	ClockStepS float64 `json:"wall_clock_step_s"`
}

// CycleStats is the order cycles that ended in a window.
type CycleStats struct {
	Completed   int            `json:"completed"`
	Failed      int            `json:"failed"`
	PerSecond   float64        `json:"completed_per_second"`
	Latency     Latency        `json:"latency_completed"`
	Failures    map[string]int `json:"failures,omitempty"`
	FirstErrors []string       `json:"first_errors,omitempty"`
}

// StageReport is one stage.
type StageReport struct {
	Stage        int        `json:"stage"`
	CommandUsers int        `json:"command_users"`
	ReadUsers    int        `json:"read_users"`
	PoolCycles   int        `json:"cycles_readable_at_start"`
	Window       Window     `json:"window"`
	HostBefore   HostState  `json:"host_before"`
	HostAfter    HostState  `json:"host_after"`
	Commands     ClassStats `json:"commands"`
	Reads        ClassStats `json:"reads"`
	Cycles       CycleStats `json:"cycles"`
	APICores     float64    `json:"api_cpu_cores"`
	WorkerCores  float64    `json:"worker_cpu_cores"`
	DB           DBStats    `json:"database"`
	TopWaits     []string   `json:"top_wait_events,omitempty"`
	// WorkerRelay is what the workers' own metrics counted in the window (S5R-D07).
	WorkerRelay *WorkerRelayStats `json:"worker_relay_metrics,omitempty"`
}

// DrainStats is the relay emptying the Outbox after the last stage, with no load.
type DrainStats struct {
	Seconds   float64 `json:"seconds"`
	Published int64   `json:"published"`
	PerSecond float64 `json:"published_per_second"`
	Left      int     `json:"pending_or_leased_left"`
	Complete  bool    `json:"complete"`
}

// WritesStats is a writes-mode run (drill writes).
type WritesStats struct {
	Users    int        `json:"users"`
	Seconds  float64    `json:"seconds"`
	AckLog   string     `json:"ack_log"`
	Commands ClassStats `json:"commands"`
	Cycles   CycleStats `json:"cycles"`
}

const workloadNote = "BRC-001 on one tenant and company (company.A), one customer (customer.1), one RFQ (rfq.1, 4 lines, 120 PCS) and one " +
	"cost snapshot (660.00 USD). A command user repeats the order cycle: evd file.register (the customer's PO, ~3.7 KiB text), " +
	"trd quote.save_draft, quote.freeze, GET quote, quote.submit_review, quote.review.decide, quote.activate (formal document and QT number), " +
	"evd source.register (the customer's mail), quote.response.record, quote.response.verify, order.prepare, GET order, order.review.request, " +
	"order.review.decide, cnx trade_compliance.evaluate, GET check, trade_compliance.decide, stl credit.reserve (672.00 of the shared " +
	"customer credit), GET order check, trd order.activate, pur purchase.draft.prepare (600.00): 17 commands and 4 reads, as six humans " +
	"(sales, quote.reviewer, acceptance.verifier, order.reviewer, compliance, purchaser). A read user issues one of 14 GET routes " +
	"uniformly on the cycles completed so far. Closed loop, no think time; brant-worker -dispatch processes (2 by default, S5R-D07's " +
	"reference deployment) relay the Outbox to the contexts' consumers (STL consumes the reservation on OrderActivated)."

var notExecuted = []string{
	"a front end: latencies are client-side over loopback HTTP (server plus transport), not browser end to end",
	"a real identity provider: tokens come from the load generator's local issuer (RS256, at+jwt); each actor's token is reused for 7.5 minutes",
	"several tenants or customers: every user works in one tenant on one customer, so the shared rows (POL quote number sequence, " +
		"STL customer credit, authority guard) are contended as hard as possible",
	"AI tasks, large reports, files above 4 KiB and external services: baseline 11.1 sets their limits apart",
	"failures (process restart, database interruption, redelivery) during the stages: the recovery drill covers the database",
	"a production-like host: the API, the worker, PostgreSQL (Docker) and the load generator share one WSL2 host with other work",
	"a host whose wall clock is slewed, not stepped: WSL2 steps it forward by seconds now and then; each window reports the step it saw, " +
		"and a step between an event's recorded_at and published_at adds to that event's relay lag (both stamps are wall-clock readings)",
}

func environment(ctx context.Context, obs *observer, cfg config) Environment {
	env := Environment{Go: runtime.Version(), OSArch: runtime.GOOS + "/" + runtime.GOARCH, CPUs: runtime.NumCPU(), Host: hostState(),
		Transport: "brant-api process on 127.0.0.1, HTTP/1.1 keep-alive, JSON",
		Identity:  "local OIDC issuer of the load generator (RS256, typ at+jwt, JWKS by discovery)"}
	if raw, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		env.Kernel = strings.TrimSpace(string(raw))
		env.WSL = strings.Contains(strings.ToLower(env.Kernel), "microsoft")
	}
	env.APIPool = fmt.Sprintf("pgxpool pool_max_conns=%d", max(4, runtime.NumCPU()))
	if cfg.poolMax > 0 {
		env.APIPool = fmt.Sprintf("pgxpool pool_max_conns=%d", cfg.poolMax)
	}
	if obs == nil {
		return env
	}
	_ = obs.pool.QueryRow(ctx, `SELECT version()`).Scan(&env.PostgreSQL)
	env.DatabaseBytesStart = 0
	_ = obs.pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&env.DatabaseBytesStart)
	env.Settings = map[string]string{}
	rows, err := obs.pool.Query(ctx, `SELECT name, setting || coalesce(unit, '') FROM pg_settings WHERE name IN ('max_connections',
		'shared_buffers', 'work_mem', 'synchronous_commit', 'fsync', 'wal_level', 'archive_mode', 'archive_timeout', 'max_wal_size',
		'checkpoint_timeout', 'effective_cache_size', 'default_transaction_isolation')`)
	if err == nil {
		for rows.Next() {
			var k, v string
			if rows.Scan(&k, &v) == nil {
				env.Settings[k] = v
			}
		}
		rows.Close()
	}
	return env
}

// summary is the human-readable table of the run (summary.md).
func (r *Report) summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Load run %s (%s)\n\n", r.Label, r.Mode)
	fmt.Fprintf(&b, "- started %s, finished %s; tenant %s\n", r.StartedAt, r.FinishedAt, r.Tenant)
	e := r.Environment
	fmt.Fprintf(&b, "- %s, %s, kernel %s (WSL %v), nproc %d; %s\n", e.Go, e.OSArch, e.Kernel, e.WSL, e.CPUs, e.PostgreSQL)
	fmt.Fprintf(&b, "- host at start: %s; MemAvailable %d MiB of %d MiB\n", e.Host.Uptime, e.Host.MemAvailMiB, e.Host.MemTotalMiB)
	fmt.Fprintf(&b, "- API pool: %s; settings %v\n", e.APIPool, e.Settings)
	fmt.Fprintf(&b, "- setup %.1f s (%d requests); prime %d cycles in %.1f s (commands P95 %.1f ms, reads P95 %.1f ms); %d brant-worker -dispatch\n\n",
		r.Setup.Seconds, r.Setup.Requests, r.Prime.Cycles, r.Prime.Seconds, r.Prime.Commands.Latency.P95, r.Prime.Reads.Latency.P95, r.Workers)
	if len(r.Stages) > 0 {
		b.WriteString("| stage | users cmd/read | cmd ok/s | cmd P50/P95/P99 ms | cmd err/timeouts | read ok/s | read P50/P95/P99 ms | read err/timeouts | " +
			"cycles/s | cycle P95 s | API cores | conns max / active mean | lock waiters max (samples) | longest lock wait s | " +
			"outbox backlog max / relay lag P95 s | load1 before→after | MemAvailable MiB |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, s := range r.Stages {
			fmt.Fprintf(&b, "| %d | %d/%d | %.1f | %.1f / %.1f / %.1f | %d/%d | %.1f | %.1f / %.1f / %.1f | %d/%d | %.2f | %.2f | %.2f | %d / %.1f | %d (%d/%d) | %.3f | %d / %.3f | %.2f→%.2f | %d |\n",
				s.Stage, s.CommandUsers, s.ReadUsers, s.Commands.RPS, s.Commands.Latency.P50, s.Commands.Latency.P95, s.Commands.Latency.P99,
				s.Commands.Errors+s.Commands.Transport, s.Commands.Timeouts, s.Reads.RPS, s.Reads.Latency.P50, s.Reads.Latency.P95, s.Reads.Latency.P99,
				s.Reads.Errors+s.Reads.Transport, s.Reads.Timeouts, s.Cycles.PerSecond, s.Cycles.Latency.P95/1000, s.APICores,
				s.DB.APIConnsMax, s.DB.APIActiveMean, s.DB.LockWaitersMax, s.DB.LockWaitSamples, s.DB.Samples, s.DB.LongestLockWaitS,
				s.DB.OutboxBacklogMax, s.DB.RelayLag.P95, s.HostBefore.Load1, s.HostAfter.Load1, s.HostAfter.MemAvailMiB)
		}
		b.WriteString("\n")
		for _, s := range r.Stages {
			fmt.Fprintf(&b, "- stage %d: commits %d, rollbacks %d, deadlocks %d, cache hit %.4f, top waits %v", s.Stage, s.DB.Commits,
				s.DB.Rollbacks, s.DB.Deadlocks, s.DB.CacheHitRatio, s.TopWaits)
			fmt.Fprintf(&b, "; relay lag P50/P95/P99/max %.3f/%.3f/%.3f/%.3f s over %d events, oldest undelivered max %.3f s, wall clock step %.3f s",
				s.DB.RelayLag.P50, s.DB.RelayLag.P95, s.DB.RelayLag.P99, s.DB.RelayLag.Max, s.DB.RelayLag.Published, s.DB.OldestUndeliveredMaxS,
				s.Window.ClockStepS)
			if w := s.WorkerRelay; w != nil {
				fmt.Fprintf(&b, "; workers' metrics: %d workers published %.0f, %.2f %% within the P95 target, %.2f %% within the P99 target, "+
					"%.0f failures, %.0f dead letters, settlement mean %.4f s", w.Workers, w.Published, w.ShareP95*100, w.ShareP99*100, w.Failures,
					w.DeadLetters, w.SettleMeanS)
				if w.ScrapeErrors > 0 {
					fmt.Fprintf(&b, " (%d scrapes failed)", w.ScrapeErrors)
				}
			}
			if len(s.Commands.ErrorCodes) > 0 || len(s.Reads.ErrorCodes) > 0 || s.Cycles.Failed > 0 {
				fmt.Fprintf(&b, "; command errors %v, read errors %v, cycle failures %v", s.Commands.ErrorCodes, s.Reads.ErrorCodes, s.Cycles.Failures)
			}
			b.WriteString("\n")
		}
	}
	if t := r.Thresholds; t != nil && len(t.Checks) > 0 {
		fmt.Fprintf(&b, "\nThresholds (asserted %v; %d dispatch workers; relay targets up to %d command users, API targets up to %d): verdict %s, %d failed\n\n",
			t.Asserted, t.Workers, t.ReferenceUsers, t.APIUsers, t.Verdict, t.Failed)
		b.WriteString("| stage | command users | target | basis | limit s | observed s | status | note |\n|---|---|---|---|---|---|---|---|\n")
		for _, c := range t.Checks {
			observed := "–"
			if c.ObservedS != nil {
				observed = strconv.FormatFloat(*c.ObservedS, 'f', 3, 64)
			}
			fmt.Fprintf(&b, "| %d | %d | %s | %s | %g | %s | %s | %s |\n", c.Stage, c.CommandUsers, c.Target, c.Basis, c.LimitS, observed, c.Status, c.Note)
		}
		b.WriteString("\n")
	}
	if d := r.Drain; d != nil {
		fmt.Fprintf(&b, "Outbox drain after the last stage: %.1f s, %d events published (%.1f/s), %d left, complete %v\n",
			d.Seconds, d.Published, d.PerSecond, d.Left, d.Complete)
	}
	if w := r.Writes; w != nil {
		fmt.Fprintf(&b, "Writes: %d users for %.1f s; %d commands acknowledged (P95 %.1f ms), %d errors, %d timeouts, %d transport errors; "+
			"cycles %d completed, %d failed; log %s\n", w.Users, w.Seconds, w.Commands.OK, w.Commands.Latency.P95, w.Commands.Errors,
			w.Commands.Timeouts, w.Commands.Transport, w.Cycles.Completed, w.Cycles.Failed, w.AckLog)
	}
	return b.String()
}
