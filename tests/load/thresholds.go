package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The targets a staged run checks. tests/load imports no internal package (AC6), so it states the
// values itself; internal/foundation/messaging holds the relay's as constants and brant-worker
// takes its alert thresholds as flags.
//
//   - S5R-D07 (simulated sign-off E0; the values need a real owner of operations and finance
//     before B0): at the reference deployment (at least 2 brant-worker -dispatch) and up to the
//     reference load (16 command users and 16 read users), the relay latency from an event's
//     recorded_at to its publication is P95 <= 5 s and P99 <= 30 s, and the oldest undelivered
//     event stays younger than the 60 s alert.
//   - WBS S5-11 (S5 C7 froze them on the measured baseline): reads P95 <= 500 ms and commands
//     P95 <= 800 ms per API instance up to 32 concurrent command users.
const (
	defaultRelayP95       = 5 * time.Second
	defaultRelayP99       = 30 * time.Second
	defaultLagAlert       = 60 * time.Second
	defaultReadP95        = 500 * time.Millisecond
	defaultCommandP95     = 800 * time.Millisecond
	defaultReferenceUsers = 16
	defaultAPIUsers       = 32
	referenceWorkers      = 2
)

// thresholds are the targets of a run and the loads they apply to.
type thresholds struct {
	assert                    bool
	relayP95, relayP99, alert time.Duration
	readP95, commandP95       time.Duration
	referenceUsers, apiUsers  int
}

// Check statuses. A check above the load its target applies to, or of a relay that is not the
// reference deployment, is measured and reported but not asserted.
const (
	statusPass        = "pass"
	statusFail        = "fail"
	statusNotAsserted = "not_asserted"
	statusNotMeasured = "not_measured"
)

// ThresholdCheck is one target in one stage.
type ThresholdCheck struct {
	Stage        int      `json:"stage"`
	CommandUsers int      `json:"command_users"`
	Target       string   `json:"target"`
	Basis        string   `json:"basis"`
	LimitS       float64  `json:"limit_s"`
	ObservedS    *float64 `json:"observed_s,omitempty"`
	Status       string   `json:"status"`
	Note         string   `json:"note,omitempty"`
}

// ThresholdReport is the run's checks and its verdict. With -assert a check that fails fails the run.
type ThresholdReport struct {
	Asserted       bool             `json:"asserted"`
	Workers        int              `json:"dispatch_workers"`
	ReferenceUsers int              `json:"reference_command_users"`
	APIUsers       int              `json:"api_capacity_command_users"`
	Checks         []ThresholdCheck `json:"checks"`
	Failed         int              `json:"failed"`
	Verdict        string           `json:"verdict"` // pass, fail, or none when no check was asserted
}

// evaluate checks every stage against th. workers is the number of brant-worker -dispatch
// processes of the run; observed says whether the observer measured the relay.
func evaluate(th thresholds, workers int, observed bool, stages []StageReport) *ThresholdReport {
	r := &ThresholdReport{Asserted: th.assert, Workers: workers, ReferenceUsers: th.referenceUsers, APIUsers: th.apiUsers}
	asserted := 0
	for _, s := range stages {
		add := func(target, basis string, limit time.Duration, observed *float64, applies bool, why string) {
			c := ThresholdCheck{Stage: s.Stage, CommandUsers: s.CommandUsers, Target: target, Basis: basis, LimitS: limit.Seconds(), ObservedS: observed}
			switch {
			case observed == nil:
				c.Status, c.Note = statusNotMeasured, why
			case !applies:
				c.Status, c.Note = statusNotAsserted, why
			case *observed <= limit.Seconds():
				c.Status = statusPass
			default:
				c.Status = statusFail
			}
			if c.Status == statusPass || c.Status == statusFail {
				asserted++
			}
			if c.Status == statusFail {
				r.Failed++
			}
			r.Checks = append(r.Checks, c)
		}
		api := s.CommandUsers <= th.apiUsers
		apiWhy := fmt.Sprintf("above %d command users per API instance (S5-11 capacity bound)", th.apiUsers)
		var readP95, commandP95 *float64
		if s.Reads.OK > 0 {
			readP95 = ptr(s.Reads.Latency.P95 / 1000)
		}
		if s.Commands.OK > 0 {
			commandP95 = ptr(s.Commands.Latency.P95 / 1000)
		}
		add("read_latency_p95", "S5-11 (C7)", th.readP95, readP95, api, whyNot(readP95 == nil, "no successful read", apiWhy))
		add("command_latency_p95", "S5-11 (C7)", th.commandP95, commandP95, api, whyNot(commandP95 == nil, "no successful command", apiWhy))

		relay := s.CommandUsers <= th.referenceUsers && workers >= referenceWorkers
		relayWhy := fmt.Sprintf("above the reference load of %d command users", th.referenceUsers)
		if workers < referenceWorkers {
			relayWhy = fmt.Sprintf("%d dispatch workers, the reference deployment runs %d", workers, referenceWorkers)
		}
		var p95, p99, oldest *float64
		missing := "no relay (-workers 0)"
		if workers > 0 {
			missing = "no event published in the window"
			if !observed {
				missing = "no observer (-observer-dsn)"
			}
		}
		if workers > 0 && observed && s.DB.RelayLag.Published > 0 {
			p95, p99 = ptr(s.DB.RelayLag.P95), ptr(s.DB.RelayLag.P99)
		}
		if workers > 0 && observed && s.DB.Samples > 0 {
			oldest = ptr(s.DB.OldestUndeliveredMaxS)
		}
		add("relay_latency_p95", "S5R-D07", th.relayP95, p95, relay, whyNot(p95 == nil, missing, relayWhy))
		add("relay_latency_p99", "S5R-D07", th.relayP99, p99, relay, whyNot(p99 == nil, missing, relayWhy))
		add("oldest_undelivered_below_alert", "S5R-D07", th.alert, oldest, relay, whyNot(oldest == nil, missing, relayWhy))
	}
	switch {
	case r.Failed > 0:
		r.Verdict = statusFail
	case asserted == 0:
		r.Verdict = "none"
	default:
		r.Verdict = statusPass
	}
	return r
}

func ptr(v float64) *float64 { return &v }

func whyNot(missing bool, missingWhy, notAppliedWhy string) string {
	if missing {
		return missingWhy
	}
	return notAppliedWhy
}

// Names of the worker metrics the run reads (internal/foundation/messaging, metrics.go).
const (
	workerLatencyMetric     = "brant_outbox_relay_delivery_latency_seconds"
	workerFailuresMetric    = "brant_outbox_relay_publish_failures_total"
	workerDeadLettersMetric = "brant_outbox_relay_dead_letters_total"
	workerSettleMetric      = "brant_outbox_relay_settle_duration_seconds"
)

// workerCounters are the relay counters of one or more workers, summed over event sources: the
// published deliveries per latency bucket upper bound (cumulative), failures and dead letters.
type workerCounters struct {
	buckets     map[float64]float64
	published   float64
	failures    float64
	deadLetters float64
	settleSum   float64
	settleCount float64
}

// parseWorkerMetrics reads the relay counters from a Prometheus text exposition.
func parseWorkerMetrics(r io.Reader) (workerCounters, error) {
	c := workerCounters{buckets: map[float64]float64{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			return c, fmt.Errorf("metrics line %q", line)
		}
		series, raw := line[:i], line[i+1:]
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return c, fmt.Errorf("metrics line %q: %w", line, err)
		}
		name := series
		if j := strings.IndexByte(series, '{'); j >= 0 {
			name = series[:j]
		}
		switch name {
		case workerLatencyMetric + "_bucket":
			le := labelValue(series, "le")
			bound := math.Inf(1)
			if le != "+Inf" {
				if bound, err = strconv.ParseFloat(le, 64); err != nil {
					return c, fmt.Errorf("bucket bound %q: %w", le, err)
				}
			}
			c.buckets[bound] += v
		case workerLatencyMetric + "_count":
			c.published += v
		case workerFailuresMetric:
			c.failures += v
		case workerDeadLettersMetric:
			c.deadLetters += v
		case workerSettleMetric + "_sum":
			c.settleSum += v
		case workerSettleMetric + "_count":
			c.settleCount += v
		}
	}
	return c, sc.Err()
}

// labelValue returns the value of label name in a series written by the worker (values without
// quotes or escapes, as bucket bounds are).
func labelValue(series, name string) string {
	k := strings.Index(series, name+`="`)
	if k < 0 {
		return ""
	}
	rest := series[k+len(name)+2:]
	if e := strings.IndexByte(rest, '"'); e >= 0 {
		return rest[:e]
	}
	return ""
}

// add sums o into c.
func (c *workerCounters) add(o workerCounters) {
	for b, v := range o.buckets {
		c.buckets[b] += v
	}
	c.published += o.published
	c.failures += o.failures
	c.deadLetters += o.deadLetters
	c.settleSum += o.settleSum
	c.settleCount += o.settleCount
}

// WorkerRelayStats is what the workers' own metrics counted in a stage window (S5R-D07): the
// deliveries they published, the share within each latency target, failed publishes and dead
// letters. It cross-checks the observer's relay lag, which reads the Outbox.
type WorkerRelayStats struct {
	Workers      int     `json:"workers"`
	Seconds      float64 `json:"seconds"` // between the two scrapes, about the stage window`
	Published    float64 `json:"published"`
	ShareP95     float64 `json:"share_within_p95_target"` // of the published, those within the P95 target
	ShareP99     float64 `json:"share_within_p99_target"`
	P95Met       bool    `json:"p95_target_met"` // at least 95 % within the P95 target
	P99Met       bool    `json:"p99_target_met"`
	Failures     float64 `json:"publish_failures"`
	DeadLetters  float64 `json:"dead_letters"`
	ScrapeErrors int     `json:"scrape_errors,omitempty"`
	// SettleMeanS is the mean time a worker took to mark a delivery published (its Outbox
	// transaction). The workers count a delivery when that transaction returns, the observer by the
	// published_at stamped before it, so the two counts of a window differ at its edges by the
	// deliveries settling there; the latency of each delivery is the same in both.
	SettleMeanS float64 `json:"settle_mean_s"`
}

// workerWindow is the difference of two scrapes of every worker.
func workerWindow(before, after workerCounters, workers, scrapeErrors int, th thresholds) *WorkerRelayStats {
	w := &WorkerRelayStats{Workers: workers, ScrapeErrors: scrapeErrors, Published: after.published - before.published,
		Failures: after.failures - before.failures, DeadLetters: after.deadLetters - before.deadLetters}
	if n := after.settleCount - before.settleCount; n > 0 {
		w.SettleMeanS = round((after.settleSum-before.settleSum)/n, 4)
	}
	if w.Published > 0 {
		w.ShareP95 = round(within(before, after, th.relayP95.Seconds())/w.Published, 4)
		w.ShareP99 = round(within(before, after, th.relayP99.Seconds())/w.Published, 4)
	}
	w.P95Met = w.Published > 0 && w.ShareP95 >= 0.95
	w.P99Met = w.Published > 0 && w.ShareP99 >= 0.99
	return w
}

// within is how many deliveries of the window took at most limit seconds, from the cumulative
// bucket whose bound is the limit (the worker's buckets hold the S5R-D07 targets), or from the
// largest bound below it.
func within(before, after workerCounters, limit float64) float64 {
	best := math.Inf(-1)
	for b := range after.buckets {
		if b <= limit && b > best {
			best = b
		}
	}
	if math.IsInf(best, -1) {
		return 0
	}
	return after.buckets[best] - before.buckets[best]
}

// scrapeWorkers reads and sums the metrics of every worker; it returns how many scrapes failed.
func scrapeWorkers(ctx context.Context, urls []string) (workerCounters, int) {
	sum := workerCounters{buckets: map[float64]float64{}}
	errs := 0
	for _, u := range urls {
		c, err := scrapeWorker(ctx, u)
		if err != nil {
			errs++
			continue
		}
		sum.add(c)
	}
	return sum, errs
}

func scrapeWorker(ctx context.Context, url string) (workerCounters, error) {
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, url, nil)
	if err != nil {
		return workerCounters{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return workerCounters{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return workerCounters{}, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return parseWorkerMetrics(resp.Body)
}
