// Command load is Brant's staged load generator and drill writer (WBS S5-11, baseline 11.1). It is
// a black-box client: it imports no internal package (AC6). It starts the built brant-api and
// brant-worker -dispatch as processes behind a local OIDC issuer, provisions a fresh tenant with
// brant-admin, builds BRC-001's front through the HTTP API, and then
//
//   - in stages mode, runs closed-loop virtual users stage by stage from small load to saturation:
//     command users repeat BRC-001's order cycle (quote to purchase draft, 17 commands and 4 reads),
//     read users issue ordinary reads of what the cycles wrote. Each stage reports P50/P95/P99,
//     error and timeout rates of reads and commands apart, cycles per second, and what PostgreSQL
//     showed: the API's connections, lock waiters, ungranted locks, transaction counters, the
//     Outbox backlog and the relay's lag;
//   - in writes mode, runs command users for a fixed time and appends every acknowledged command
//     to a log, which the recovery drill (scripts/backup-restore.sh) compares with a restored
//     database in acks mode to measure what a restore lost.
//
// Usage (make load and make drill run it):
//
//	go run ./tests/load -bin bin -dsn "$IDP_BRANT_POSTGRES_DSN" -observer-dsn postgres://postgres:...@127.0.0.1:55850/brant \
//	    -stages 1,2,4,8,16,32 -warmup 10s -measure 50s -out bin/load/run1
//	go run ./tests/load -mode writes -vus 4 -duration 3m -tenant drill.1 -ack-log bin/drill/acks.jsonl ...
//	go run ./tests/load -mode acks -dsn <restored database> -tenant drill.1 -ack-log bin/drill/acks.jsonl
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
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

type config struct {
	mode               string
	bin, dsn, observer string
	out, label         string
	stages             []int
	readRatio          float64
	warmup, measure    time.Duration
	pause, timeout     time.Duration
	prime              int
	drain              time.Duration
	worker             bool
	poolMax            int
	vus                int
	duration           time.Duration
	ackLog             string
	tenant             string
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.mode == "acks" {
		res, err := checkAcks(ctx, cfg.dsn, cfg.tenant, cfg.ackLog)
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		if err != nil {
			fmt.Fprintln(os.Stderr, "load:", err)
			os.Exit(1)
		}
		return
	}
	rep, err := run(ctx, cfg)
	if rep != nil {
		if werr := writeReport(cfg, rep); werr != nil {
			fmt.Fprintln(os.Stderr, "load: write report:", werr)
		}
		fmt.Print(rep.summary())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	cfg := config{}
	var stages string
	fs.StringVar(&cfg.mode, "mode", "stages", "stages (staged load), writes (drill writes with an acknowledgement log) or acks "+
		"(which acknowledged commands of -ack-log the database of -dsn holds for -tenant)")
	fs.StringVar(&cfg.bin, "bin", "bin", "directory holding brant-api, brant-worker and brant-admin (make build)")
	fs.StringVar(&cfg.dsn, "dsn", os.Getenv("IDP_BRANT_POSTGRES_DSN"), "application role DSN (postgres:// URL) for the API, the worker and brant-admin")
	fs.StringVar(&cfg.observer, "observer-dsn", os.Getenv("BRANT_LOAD_OBSERVER_DSN"), "DSN the observer samples PostgreSQL with (superuser or pg_read_all_stats); empty skips the observation")
	fs.StringVar(&cfg.out, "out", "bin/load", "directory for report.json, summary.md and the processes' logs")
	fs.StringVar(&cfg.label, "label", "", "label recorded in the report")
	fs.StringVar(&stages, "stages", "1,2,4,8,16,32", "command users per stage, comma-separated")
	fs.Float64Var(&cfg.readRatio, "read-ratio", 1, "read users per command user in each stage")
	fs.DurationVar(&cfg.warmup, "warmup", 10*time.Second, "time a stage runs before its measurement window")
	fs.DurationVar(&cfg.measure, "measure", 50*time.Second, "measurement window of a stage")
	fs.DurationVar(&cfg.pause, "pause", 5*time.Second, "idle time between stages (the relay drains the Outbox)")
	fs.DurationVar(&cfg.timeout, "timeout", 15*time.Second, "client timeout of one request")
	fs.IntVar(&cfg.prime, "prime", 3, "order cycles run one after another before the first stage")
	fs.DurationVar(&cfg.drain, "drain", 10*time.Minute, "after the last stage, how long the relay may take to empty the Outbox before the "+
		"processes stop (needs the worker and the observer); 0 skips the drain")
	fs.BoolVar(&cfg.worker, "worker", true, "run brant-worker -dispatch beside the API")
	fs.IntVar(&cfg.poolMax, "api-pool-max", 0, "pool_max_conns of the API's pool; 0 keeps pgxpool's default, max(4, CPUs)")
	fs.IntVar(&cfg.vus, "vus", 4, "writes mode: command users")
	fs.DurationVar(&cfg.duration, "duration", 3*time.Minute, "writes mode: how long the users write")
	fs.StringVar(&cfg.ackLog, "ack-log", "", "writes mode: JSON-lines log of every acknowledged command")
	fs.StringVar(&cfg.tenant, "tenant", "", "tenant ID; empty draws a fresh one")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	for _, s := range strings.Split(stages, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 {
			return cfg, fmt.Errorf("-stages: %q is not a positive number", s)
		}
		cfg.stages = append(cfg.stages, n)
	}
	switch {
	case cfg.mode != "stages" && cfg.mode != "writes" && cfg.mode != "acks":
		return cfg, fmt.Errorf("-mode must be stages, writes or acks, got %q", cfg.mode)
	case cfg.dsn == "":
		return cfg, errors.New("-dsn or IDP_BRANT_POSTGRES_DSN is required")
	case cfg.mode != "stages" && cfg.ackLog == "":
		return cfg, fmt.Errorf("%s mode needs -ack-log", cfg.mode)
	case cfg.mode == "acks" && cfg.tenant == "":
		return cfg, errors.New("acks mode needs the -tenant of the writes run")
	}
	if cfg.tenant == "" {
		cfg.tenant = fmt.Sprintf("load.%s.%04x", time.Now().UTC().Format("20060102T150405"), rand.IntN(1<<16))
	}
	return cfg, nil
}

// env is a running system under test.
type env struct {
	cfg    config
	is     *issuer
	api    *proc
	worker *proc
	c      *client
	obs    *observer
	w      *world
	pool   *pool
	rec    *recorder
}

func (e *env) close() {
	_ = e.worker.stop() // SIGTERM ends both; how they exit is in their logs
	_ = e.api.stop()
	if e.obs != nil {
		e.obs.close()
	}
	if e.is != nil {
		e.is.close()
	}
}

// run starts the system, builds the world and runs the mode; the report holds what was measured
// even when a later step fails.
func run(ctx context.Context, cfg config) (*Report, error) {
	if err := os.MkdirAll(cfg.out, 0o755); err != nil {
		return nil, err
	}
	rep := &Report{Tool: "tests/load", Mode: cfg.mode, Label: cfg.label, Tenant: cfg.tenant, Company: company,
		StartedAt: time.Now().UTC().Format(time.RFC3339), Workload: workloadNote, NotExecuted: notExecuted}
	e := &env{cfg: cfg, pool: &pool{}, rec: &recorder{}}
	defer e.close()
	if err := e.start(ctx, rep); err != nil {
		return rep, err
	}
	setupStart := time.Now()
	w, err := setup(ctx, e.c)
	rep.Setup = SetupStats{Seconds: round(time.Since(setupStart).Seconds(), 2), Requests: len(e.rec.take())}
	if err != nil {
		return rep, err
	}
	e.w = w
	if cfg.mode == "writes" {
		err = e.writes(ctx, rep)
	} else {
		err = e.staged(ctx, rep)
		if err == nil {
			e.drain(ctx, rep)
		}
	}
	rep.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	if e.obs != nil {
		rep.Environment.DatabaseBytesEnd = e.dbSize(ctx)
	}
	return rep, err
}

func (e *env) start(ctx context.Context, rep *Report) error {
	cfg := e.cfg
	var err error
	if e.is, err = newIssuer(); err != nil {
		return err
	}
	if cfg.observer != "" {
		if e.obs, err = newObserver(ctx, cfg.observer); err != nil {
			return fmt.Errorf("observer: %w", err)
		}
	}
	rep.Environment = environment(ctx, e.obs, cfg)
	apiDSN, err := withParams(cfg.dsn, map[string]string{"application_name": appAPI, "pool_max_conns": poolParam(cfg.poolMax)})
	if err != nil {
		return err
	}
	if err = provision(ctx, cfg.bin, cfg.dsn, cfg.tenant); err != nil {
		return err
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	e.api, err = startProc(cfg.bin, "brant-api", filepath.Join(cfg.out, "brant-api.log"), []string{"IDP_BRANT_POSTGRES_DSN=" + apiDSN},
		"-listen", "127.0.0.1:"+strconv.Itoa(port), "-oidc-issuer", e.is.url, "-oidc-audience", audience, "-oidc-insecure-http")
	if err != nil {
		return err
	}
	if err = waitHealthy(ctx, base, e.api); err != nil {
		return err
	}
	if cfg.worker {
		workerDSN, err := withParams(cfg.dsn, map[string]string{"application_name": appWorker})
		if err != nil {
			return err
		}
		e.worker, err = startProc(cfg.bin, "brant-worker", filepath.Join(cfg.out, "brant-worker.log"),
			[]string{"IDP_BRANT_POSTGRES_DSN=" + workerDSN}, "-dispatch")
		if err != nil {
			return err
		}
	}
	transport := &http.Transport{MaxIdleConns: 4096, MaxIdleConnsPerHost: 4096, IdleConnTimeout: 90 * time.Second, DisableCompression: true}
	e.c = &client{base: base, tenant: cfg.tenant, company: company, is: e.is, rec: e.rec, timeout: cfg.timeout,
		hc: &http.Client{Transport: transport}}
	return nil
}

func poolParam(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// cycleResult is one order cycle a command user finished or abandoned on a refusal.
type cycleResult struct {
	end  time.Time
	dur  time.Duration
	step string // the refused step, "" on success
	code string
}

type cycleLog struct {
	mu      sync.Mutex
	results []cycleResult
	errs    []string
}

func (l *cycleLog) add(r cycleResult, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.results = append(l.results, r)
	if err != nil && len(l.errs) < 20 {
		l.errs = append(l.errs, r.end.UTC().Format(time.RFC3339Nano)+" "+err.Error())
	}
}

// commandUser repeats order cycles until ctx ends.
func (e *env) commandUser(ctx context.Context, vu int, log *cycleLog) {
	for n := 1; ctx.Err() == nil; n++ {
		start := time.Now()
		cy, err := orderCycle(ctx, e.c, e.w, vu, n)
		if ctx.Err() != nil {
			return // the stage ended inside the cycle: it is neither a success nor a failure
		}
		r := cycleResult{end: time.Now(), dur: time.Since(start)}
		if err != nil {
			var se *stepError
			if errors.As(err, &se) {
				r.step = se.step
			}
			var rf *refusal
			if errors.As(err, &rf) {
				r.code = strconv.Itoa(rf.Status) + " " + rf.Code
			} else {
				r.code = "transport/timeout"
			}
		} else {
			e.pool.add(cy)
		}
		log.add(r, err)
		if err != nil {
			select { // a refused or failed cycle backs off instead of spinning
			case <-ctx.Done():
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
}

// readUser issues reads until ctx ends.
func (e *env) readUser(ctx context.Context, seed uint64) {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	for ctx.Err() == nil {
		if err := readOnce(ctx, e.c, e.w, e.pool, r); err != nil && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

func (e *env) staged(ctx context.Context, rep *Report) error {
	prime := &cycleLog{}
	start := time.Now()
	for n := 1; n <= e.cfg.prime; n++ {
		cy, err := orderCycle(ctx, e.c, e.w, 0, n)
		if err != nil {
			return fmt.Errorf("prime cycle %d: %w", n, err)
		}
		e.pool.add(cy)
		prime.add(cycleResult{end: time.Now()}, nil)
	}
	samples := e.rec.take()
	rep.Prime = PrimeStats{Cycles: e.cfg.prime, Seconds: round(time.Since(start).Seconds(), 2),
		Commands: aggregate(samples, classCommand, start, time.Now().Add(time.Second)),
		Reads:    aggregate(samples, classRead, start, time.Now().Add(time.Second))}
	for i, users := range e.cfg.stages {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		st, err := e.stage(ctx, i+1, users)
		rep.Stages = append(rep.Stages, st)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "stage %d (%d command + %d read users): commands P95 %.1f ms, reads P95 %.1f ms, %d cycles, load1 %.2f\n",
			st.Stage, st.CommandUsers, st.ReadUsers, st.Commands.Latency.P95, st.Reads.Latency.P95, st.Cycles.Completed, st.HostAfter.Load1)
		if i < len(e.cfg.stages)-1 {
			select {
			case <-ctx.Done():
			case <-time.After(e.cfg.pause):
			}
		}
	}
	return nil
}

// stage runs users command users and readers read users for warmup + measure and reports the
// measurement window.
func (e *env) stage(ctx context.Context, index, users int) (StageReport, error) {
	readers := int(math.Round(float64(users) * e.cfg.readRatio))
	st := StageReport{Stage: index, CommandUsers: users, ReadUsers: readers, HostBefore: hostState(), PoolCycles: e.pool.size()}
	if ended, err := e.api.exited(); ended {
		return st, fmt.Errorf("brant-api ended: %w", err)
	}
	e.rec.take()
	sctx, cancel := context.WithCancel(ctx)
	var sampler *sampler
	if e.obs != nil {
		sampler = e.obs.start(ctx, time.Second)
	}
	log := &cycleLog{}
	var wg sync.WaitGroup
	for v := 0; v < users; v++ {
		wg.Add(1)
		go func(vu int) { defer wg.Done(); e.commandUser(sctx, index*1000+vu, log) }(v + 1)
	}
	for v := 0; v < readers; v++ {
		wg.Add(1)
		go func(seed uint64) { defer wg.Done(); e.readUser(sctx, seed) }(uint64(index*1000 + v))
	}
	from := time.Now().Add(e.cfg.warmup)
	to := from.Add(e.cfg.measure)
	sleepUntil(ctx, from)
	before, apiCPU, workerCPU := e.counters(ctx), e.api.cpuSeconds(), e.worker.cpuSecondsOrZero()
	sleepUntil(ctx, to)
	after, apiCPU2, workerCPU2 := e.counters(ctx), e.api.cpuSeconds(), e.worker.cpuSecondsOrZero()
	st.HostAfter = hostState()
	cancel()
	wg.Wait()
	samples := e.rec.take()
	st.Window = Window{From: from.UTC().Format(time.RFC3339Nano), To: to.UTC().Format(time.RFC3339Nano), Seconds: to.Sub(from).Seconds()}
	st.Commands = aggregate(samples, classCommand, from, to)
	st.Reads = aggregate(samples, classRead, from, to)
	st.Cycles = cycleStats(log, from, to)
	secs := to.Sub(from).Seconds()
	st.APICores = round((apiCPU2-apiCPU)/secs, 2)
	st.WorkerCores = round((workerCPU2-workerCPU)/secs, 2)
	if sampler != nil {
		probes, errs := sampler.finish()
		lag, err := e.obs.relayLag(ctx, from, to)
		if err != nil {
			errs++
		}
		st.DB = summarizeProbes(probes, errs, from, to, before, after, lag)
		st.TopWaits = topWaits(st.DB.WaitEvents, 6)
	}
	return st, ctx.Err()
}

func (p *proc) cpuSecondsOrZero() float64 {
	if p == nil {
		return 0
	}
	return p.cpuSeconds()
}

func (e *env) counters(ctx context.Context) dbCounters {
	if e.obs == nil {
		return dbCounters{}
	}
	c, err := e.obs.counters(ctx)
	if err != nil {
		return dbCounters{}
	}
	return c
}

func (e *env) dbSize(ctx context.Context) int64 {
	var n int64
	_ = e.obs.pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&n)
	return n
}

func sleepUntil(ctx context.Context, t time.Time) {
	select {
	case <-ctx.Done():
	case <-time.After(time.Until(t)):
	}
}

func cycleStats(log *cycleLog, from, to time.Time) CycleStats {
	log.mu.Lock()
	defer log.mu.Unlock()
	cs := CycleStats{Failures: map[string]int{}, FirstErrors: log.errs}
	var durations []time.Duration
	for _, r := range log.results {
		if r.end.Before(from) || !r.end.Before(to) {
			continue
		}
		if r.step == "" && r.code == "" {
			cs.Completed++
			durations = append(durations, r.dur)
		} else {
			cs.Failed++
			cs.Failures[r.step+" "+r.code]++
		}
	}
	cs.PerSecond = round(float64(cs.Completed)/to.Sub(from).Seconds(), 3)
	cs.Latency = summarize(durations)
	return cs
}

// writes runs command users for the configured duration and logs every acknowledged command.
func (e *env) writes(ctx context.Context, rep *Report) error {
	f, err := os.OpenFile(e.cfg.ackLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	e.c.ack = f
	log := &cycleLog{}
	start := time.Now()
	wctx, cancel := context.WithTimeout(ctx, e.cfg.duration)
	defer cancel()
	var wg sync.WaitGroup
	for v := 1; v <= e.cfg.vus; v++ {
		wg.Add(1)
		go func(vu int) { defer wg.Done(); e.commandUser(wctx, 9000+vu, log) }(v)
	}
	wg.Wait()
	end := time.Now()
	samples := e.rec.take()
	rep.Writes = &WritesStats{Users: e.cfg.vus, Seconds: round(end.Sub(start).Seconds(), 2), AckLog: e.cfg.ackLog,
		Commands: aggregate(samples, classCommand, start, end.Add(time.Second)), Cycles: cycleStats(log, start, end.Add(time.Second))}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// drain keeps the relay running after the last stage until the Outbox holds no PENDING or LEASED
// event of the run's tenant, so that the next run starts on an empty backlog; it reports how long
// that took.
func (e *env) drain(ctx context.Context, rep *Report) {
	if e.worker == nil || e.obs == nil || e.cfg.drain <= 0 {
		return
	}
	start := time.Now()
	d := &DrainStats{}
	for {
		left, err := e.obs.backlogOf(ctx, e.cfg.tenant+"/"+company)
		if err == nil {
			d.Left = left
		}
		if (err == nil && d.Left == 0) || time.Since(start) >= e.cfg.drain || ctx.Err() != nil {
			break
		}
		sleepUntil(ctx, time.Now().Add(2*time.Second))
	}
	end := time.Now()
	d.Seconds = round(end.Sub(start).Seconds(), 1)
	d.Complete = d.Left == 0
	if lag, err := e.obs.relayLag(ctx, start, end.Add(time.Second)); err == nil {
		d.Published = lag.Published
		if d.Seconds > 0 {
			d.PerSecond = round(float64(lag.Published)/d.Seconds, 1)
		}
	}
	rep.Drain = d
}

// HostState is the host's load and memory when a measurement starts or ends (uptime, nproc,
// /proc/meminfo).
type HostState struct {
	At             string  `json:"at"`
	Uptime         string  `json:"uptime"`
	Load1          float64 `json:"load1"`
	Load5          float64 `json:"load5"`
	Load15         float64 `json:"load15"`
	CPUs           int     `json:"nproc"`
	MemFreeMiB     int64   `json:"mem_free_mib"`
	MemAvailMiB    int64   `json:"mem_available_mib"`
	MemTotalMiB    int64   `json:"mem_total_mib"`
	SwapUsedMiB    int64   `json:"swap_used_mib"`
	OtherWorkNotes string  `json:"other_work,omitempty"`
}

func hostState() HostState {
	h := HostState{At: time.Now().UTC().Format(time.RFC3339), CPUs: runtime.NumCPU()}
	if out, err := exec.Command("uptime").Output(); err == nil {
		h.Uptime = strings.TrimSpace(string(out))
	}
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(raw))
		if len(f) >= 3 {
			h.Load1, _ = strconv.ParseFloat(f[0], 64)
			h.Load5, _ = strconv.ParseFloat(f[1], 64)
			h.Load15, _ = strconv.ParseFloat(f[2], 64)
		}
	}
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		m := map[string]int64{}
		for _, line := range strings.Split(string(raw), "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				v, _ := strconv.ParseInt(f[1], 10, 64)
				m[strings.TrimSuffix(f[0], ":")] = v / 1024
			}
		}
		h.MemFreeMiB, h.MemAvailMiB, h.MemTotalMiB = m["MemFree"], m["MemAvailable"], m["MemTotal"]
		h.SwapUsedMiB = m["SwapTotal"] - m["SwapFree"]
	}
	return h
}

func round(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}

func writeReport(cfg config, rep *Report) error {
	if err := os.MkdirAll(cfg.out, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(cfg.out, "report.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cfg.out, "summary.md"), []byte(rep.summary()), 0o644)
}
