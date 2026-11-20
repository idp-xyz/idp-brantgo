package messaging

import (
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
)

// S5R-D07 froze these as engineering targets of the Outbox relay for the reference deployment and
// the reference load (16 command users and 16 read users of tests/load). The sign-off is simulated
// (E0): a real owner of operations and finance must confirm the values before B0, and G4 stays
// NO-GO. brant-worker takes the alert thresholds as flags so a confirmed value needs no release.
const (
	// RelayLatencyP95 and RelayLatencyP99 bound the relay latency: from an event's commit (its
	// recorded_at, stamped in the committing transaction) until the relay marks it published, which
	// with brant-worker -dispatch is after every consumer processed it. Dead letters do not count.
	RelayLatencyP95 = 5 * time.Second
	RelayLatencyP99 = 30 * time.Second
	// OldestUndeliveredAlert and OldestUndeliveredIncident are the ages of the oldest undelivered
	// event at which operations is alerted and at which the lag is an incident.
	OldestUndeliveredAlert    = 60 * time.Second
	OldestUndeliveredIncident = 300 * time.Second
	// ReferenceDispatchWorkers is the number of brant-worker -dispatch processes of the reference
	// deployment, scaled to 3 under load.
	ReferenceDispatchWorkers = 2
)

// latencyBuckets are the upper bounds, in seconds, of the relay latency histogram. They hold the
// targets and thresholds of S5R-D07 (5, 30, 60 and 300 s), so the share of deliveries within a
// target is exact: P95 <= 5 s holds when at least 95 % of the deliveries fall in the 5 s bucket.
var latencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

// stepBuckets are the upper bounds, in seconds, of one step of one delivery: a consumer's time on
// it, or the relay's settlement in the Outbox.
var stepBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// RelayMetrics counts what the Outbox relay and the Dispatcher of one worker process do, keeps the
// relay latency histogram and the last Outbox backlog its RelayMonitor read (S5R-D07), and serves
// them in the Prometheus text format (Handler). Counters start at zero with the process; each
// worker reports its own deliveries, while the backlog gauges describe the one Outbox every worker
// shares. A nil *RelayMetrics counts nothing, so the relay and the dispatcher run without one.
type RelayMetrics struct {
	mu          sync.Mutex
	latency     map[string]*histogram // by event source
	settle      *histogram
	failures    map[eventing.FailureCode]uint64
	deadLetters map[deadLetterKey]uint64
	claimed     uint64
	leaseLost   uint64
	roundErrors uint64
	lastRound   time.Time
	consumers   map[consumerKey]uint64
	consumerDur map[eventing.InboxConsumer]*histogram
	backlog     OutboxBacklog
	probed      bool
	probeErrors uint64
}

type deadLetterKey struct {
	code             eventing.FailureCode
	critical, stored bool
}

type consumerKey struct {
	consumer eventing.InboxConsumer
	outcome  InboxOutcome
}

// NewRelayMetrics returns metrics with every counter at zero.
func NewRelayMetrics() *RelayMetrics {
	return &RelayMetrics{
		latency: map[string]*histogram{}, settle: newHistogram(stepBuckets), failures: map[eventing.FailureCode]uint64{}, deadLetters: map[deadLetterKey]uint64{},
		consumers: map[consumerKey]uint64{}, consumerDur: map[eventing.InboxConsumer]*histogram{},
	}
}

// round records a claim round of the relay: claimed deliveries, and the time of the round when it
// ended without an error.
func (m *RelayMetrics) round(claimed int, at time.Time, err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claimed += uint64(max(claimed, 0))
	if err != nil {
		m.roundErrors++
		return
	}
	m.lastRound = at
}

// published records a delivery the relay marked published latency after the event was recorded.
func (m *RelayMetrics) published(source string, latency time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.latency[source]
	if h == nil {
		h = newHistogram(latencyBuckets)
		m.latency[source] = h
	}
	h.observe(max(latency, 0).Seconds())
}

// settled records how long marking one published delivery took in the Outbox. The latency of the
// delivery ends before it (when every consumer is done), but the relay claims the partition's next
// event only after it, and it waits for the partition row an enqueueing transaction holds.
func (m *RelayMetrics) settled(took time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settle.observe(max(took, 0).Seconds())
}

// failed records a failed publish the relay will retry.
func (m *RelayMetrics) failed(code eventing.FailureCode) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures[code]++
}

// deadLettered records a delivery the relay abandoned (ADR-BR-015).
func (m *RelayMetrics) deadLettered(a DeadLetterAlert) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deadLetters[deadLetterKey{code: a.Code, critical: a.Critical, stored: a.Stored}]++
}

// lostLease records a settlement that found its lease taken over by another worker.
func (m *RelayMetrics) lostLease() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leaseLost++
}

// consumed records one consumer's outcome of one delivery of the Dispatcher and its time on it.
func (m *RelayMetrics) consumed(consumer eventing.InboxConsumer, outcome InboxOutcome, took time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.consumers[consumerKey{consumer, outcome}]++
	h := m.consumerDur[consumer]
	if h == nil {
		h = newHistogram(stepBuckets)
		m.consumerDur[consumer] = h
	}
	h.observe(max(took, 0).Seconds())
}

// observeBacklog keeps the backlog a probe read.
func (m *RelayMetrics) observeBacklog(b OutboxBacklog) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.backlog, m.probed = b, true
}

// probeFailed counts a probe that could not read the backlog; the last backlog read stays, with
// its time.
func (m *RelayMetrics) probeFailed() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.probeErrors++
}

// Handler serves the metrics in the Prometheus text exposition format 0.0.4 on GET and HEAD.
func (m *RelayMetrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = m.WritePrometheus(w)
	})
}

// Metric names. The runbook (ops/runbook-outbox-relay.md) and tests/load refer to them.
const (
	metricLatency        = "brant_outbox_relay_delivery_latency_seconds"
	metricSettle         = "brant_outbox_relay_settle_duration_seconds"
	metricClaimed        = "brant_outbox_relay_claimed_total"
	metricFailures       = "brant_outbox_relay_publish_failures_total"
	metricDeadLetters    = "brant_outbox_relay_dead_letters_total"
	metricLeaseLost      = "brant_outbox_relay_lease_lost_total"
	metricRoundErrors    = "brant_outbox_relay_round_errors_total"
	metricLastRound      = "brant_outbox_relay_last_round_timestamp_seconds"
	metricConsumer       = "brant_dispatch_consumer_deliveries_total"
	metricConsumerTime   = "brant_dispatch_consumer_duration_seconds"
	metricBacklog        = "brant_outbox_backlog_events"
	metricOldestAge      = "brant_outbox_oldest_undelivered_age_seconds"
	metricProbeTimestamp = "brant_outbox_probe_timestamp_seconds"
	metricProbeErrors    = "brant_outbox_probe_errors_total"
)

// WritePrometheus writes every metric in the Prometheus text exposition format 0.0.4, label values
// in sorted order.
func (m *RelayMetrics) WritePrometheus(w io.Writer) error {
	var b strings.Builder
	m.mu.Lock()
	family(&b, metricLatency, "histogram", "Time from an event's recorded_at (its committing transaction) until the relay marked it published, "+
		"every consumer done (S5R-D07: P95 <= 5 s, P99 <= 30 s at the reference load). Dead letters are not counted.")
	for _, source := range sortedKeys(m.latency) {
		m.latency[source].write(&b, metricLatency, label{"source", source})
	}
	family(&b, metricSettle, "histogram", "Time the relay took to mark one published delivery in the Outbox (its transaction, "+
		"which waits for the partition row an enqueueing transaction holds); the next event of the partition waits for it.")
	m.settle.write(&b, metricSettle)
	family(&b, metricClaimed, "counter", "Deliveries the relay claimed from the Outbox.")
	sample(&b, metricClaimed, float64(m.claimed))
	family(&b, metricFailures, "counter", "Publishes that failed and will be retried with backoff.")
	for _, code := range sortedKeys(m.failures) {
		sample(&b, metricFailures, float64(m.failures[code]), label{"failure_code", string(code)})
	}
	family(&b, metricDeadLetters, "counter", "Deliveries the relay abandoned (ADR-BR-015); stored=false: no dead letter, the scope does not parse.")
	keys := make([]deadLetterKey, 0, len(m.deadLetters))
	for k := range m.deadLetters {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b deadLetterKey) int {
		return strings.Compare(string(a.code)+" "+strconv.FormatBool(a.critical)+" "+strconv.FormatBool(a.stored),
			string(b.code)+" "+strconv.FormatBool(b.critical)+" "+strconv.FormatBool(b.stored))
	})
	for _, k := range keys {
		sample(&b, metricDeadLetters, float64(m.deadLetters[k]), label{"failure_code", string(k.code)},
			label{"critical", strconv.FormatBool(k.critical)}, label{"stored", strconv.FormatBool(k.stored)})
	}
	family(&b, metricLeaseLost, "counter", "Settlements that found the lease taken over by another worker (the delivery outlived its lease).")
	sample(&b, metricLeaseLost, float64(m.leaseLost))
	family(&b, metricRoundErrors, "counter", "Claim rounds that ended with an error (database unreachable, settlement failed).")
	sample(&b, metricRoundErrors, float64(m.roundErrors))
	family(&b, metricLastRound, "gauge", "Unix time of the last claim round that ended without an error; it stops moving when the relay stalls.")
	if !m.lastRound.IsZero() {
		sample(&b, metricLastRound, unixSeconds(m.lastRound))
	}
	family(&b, metricConsumer, "counter", "Deliveries of the Dispatcher by consumer and outcome (processed, duplicate, retry, rejected).")
	ckeys := make([]consumerKey, 0, len(m.consumers))
	for k := range m.consumers {
		ckeys = append(ckeys, k)
	}
	slices.SortFunc(ckeys, func(a, b consumerKey) int {
		if c := strings.Compare(string(a.consumer), string(b.consumer)); c != 0 {
			return c
		}
		return strings.Compare(a.outcome.String(), b.outcome.String())
	})
	for _, k := range ckeys {
		sample(&b, metricConsumer, float64(m.consumers[k]), label{"consumer", string(k.consumer)}, label{"outcome", k.outcome.String()})
	}
	family(&b, metricConsumerTime, "histogram", "Time one consumer took on one delivery of the Dispatcher, its transaction included.")
	for _, c := range sortedKeys(m.consumerDur) {
		m.consumerDur[c].write(&b, metricConsumerTime, label{"consumer", string(c)})
	}
	family(&b, metricBacklog, "gauge", "Undelivered Outbox events of every tenant at the last probe, by state: pending (never failed), "+
		"retrying (after a failed publish), leased (claimed), lease_expired (claimed by a worker that stopped).")
	family(&b, metricOldestAge, "gauge", "Age of the oldest undelivered Outbox event at the last probe, 0 when there is none "+
		"(S5R-D07: alert at 60 s, incident at 300 s).")
	family(&b, metricProbeTimestamp, "gauge", "Unix time of the last probe that read the backlog.")
	if m.probed {
		bl := m.backlog
		for _, s := range []struct {
			state string
			n     int64
		}{{"lease_expired", bl.LeaseExpired}, {"leased", bl.Leased}, {"pending", bl.Pending}, {"retrying", bl.Retrying}} {
			sample(&b, metricBacklog, float64(s.n), label{"state", s.state})
		}
		sample(&b, metricOldestAge, bl.OldestAge().Seconds())
		sample(&b, metricProbeTimestamp, unixSeconds(bl.At))
	}
	family(&b, metricProbeErrors, "counter", "Probes that could not read the backlog.")
	sample(&b, metricProbeErrors, float64(m.probeErrors))
	m.mu.Unlock()
	_, err := io.WriteString(w, b.String())
	return err
}

// histogram is a Prometheus histogram: counts per bucket (not cumulative), the last one above every
// bound, and the sum and count of the observations.
type histogram struct {
	bounds []float64
	counts []uint64
	sum    float64
	count  uint64
}

func newHistogram(bounds []float64) *histogram {
	return &histogram{bounds: bounds, counts: make([]uint64, len(bounds)+1)}
}

func (h *histogram) observe(v float64) {
	i, _ := slices.BinarySearch(h.bounds, v) // the first bound >= v: buckets are "less than or equal"
	h.counts[i]++
	h.sum += v
	h.count++
}

func (h *histogram) write(b *strings.Builder, name string, labels ...label) {
	var cumulative uint64
	for i, n := range h.counts {
		cumulative += n
		le := "+Inf"
		if i < len(h.bounds) {
			le = formatFloat(h.bounds[i])
		}
		sample(b, name+"_bucket", float64(cumulative), append(slices.Clone(labels), label{"le", le})...)
	}
	sample(b, name+"_sum", h.sum, labels...)
	sample(b, name+"_count", float64(h.count), labels...)
}

type label struct{ name, value string }

func family(b *strings.Builder, name, kind, help string) {
	b.WriteString("# HELP " + name + " " + strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(help) + "\n")
	b.WriteString("# TYPE " + name + " " + kind + "\n")
}

func sample(b *strings.Builder, name string, value float64, labels ...label) {
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l.name + `="` + labelEscaper.Replace(l.value) + `"`)
		}
		b.WriteByte('}')
	}
	b.WriteString(" " + formatFloat(value) + "\n")
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func unixSeconds(t time.Time) float64 { return float64(t.UnixMicro()) / 1e6 }

func sortedKeys[K ~string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
