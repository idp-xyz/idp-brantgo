package messaging

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-bento-go/testkit"
)

// series parses a Prometheus text exposition into its samples, keyed by name and labels as written,
// and fails on a line that is neither a comment nor a sample of the format.
func series(t *testing.T, m *RelayMetrics) map[string]float64 {
	t.Helper()
	var b bytes.Buffer
	if err := m.WritePrometheus(&b); err != nil {
		t.Fatal(err)
	}
	return parseExposition(t, b.String())
}

var sampleLine = regexp.MustCompile(`^([a-z_]+)(\{[a-z_]+="(?:[^"\\]|\\.)*"(?:,[a-z_]+="(?:[^"\\]|\\.)*")*\})? (\S+)$`)

func parseExposition(t *testing.T, text string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if strings.HasPrefix(line, "# HELP ") || strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		m := sampleLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("not a sample line of the text format: %q", line)
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if _, dup := out[m[1]+m[2]]; dup {
			t.Fatalf("duplicate series %s", m[1]+m[2])
		}
		out[m[1]+m[2]] = v
	}
	return out
}

func meteredRelay(outbox *fakeOutbox, publisher eventing.PublisherFunc, m *RelayMetrics) Relay {
	r := testRelay(outbox, publisher)
	r.Metrics = m
	return r
}

// recordedBefore is a delivery whose event was recorded age before relayNow.
func recordedBefore(id string, age time.Duration) eventing.Delivery {
	d := delivery(id, 0)
	d.Envelope.RecordedAt = relayNow.Add(-age)
	d.Envelope.OccurredAt = d.Envelope.RecordedAt
	return d
}

// S5R-D07: the relay latency of a delivery runs from its event's recorded_at to the instant the
// relay marks it published, in a histogram whose buckets hold the targets (5 s, 30 s) and the
// thresholds (60 s, 300 s) exactly; a delivery of exactly 5 s is within the 5 s target.
func TestRelayMetricsMeasureTheLatencyFromRecordToPublish(t *testing.T) {
	m := NewRelayMetrics()
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{
		recordedBefore("evt-fast", 40*time.Millisecond), recordedBefore("evt-5s", 5*time.Second),
		recordedBefore("evt-slow", 31*time.Second), recordedBefore("evt-late", 10*time.Minute+time.Second),
	}}
	relay := meteredRelay(outbox, eventing.PublisherFunc(func(context.Context, eventing.Envelope) error { return nil }), m)
	if n, err := relay.RunOnce(context.Background()); n != 4 || err != nil {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}
	s := series(t, m)
	src := `source="` + testkit.ValidEnvelope().Source + `"`
	for le, want := range map[string]float64{"0.05": 1, "2.5": 1, "5": 2, "10": 2, "30": 2, "60": 3, "600": 3, "+Inf": 4} {
		if got := s[metricLatency+`_bucket{`+src+`,le="`+le+`"}`]; got != want {
			t.Errorf("bucket le=%s: %v, want %v (cumulative)", le, got, want)
		}
	}
	if s[metricLatency+"_count{"+src+"}"] != 4 || math.Abs(s[metricLatency+"_sum{"+src+"}"]-637.04) > 1e-9 {
		t.Errorf("count %v sum %v", s[metricLatency+"_count{"+src+"}"], s[metricLatency+"_sum{"+src+"}"])
	}
	if s[metricSettle+"_count"] != 4 || s[metricSettle+`_bucket{le="+Inf"}`] != 4 {
		t.Errorf("settlements %v", s)
	}
	if s[metricClaimed] != 4 || s[metricLastRound] != float64(relayNow.Unix()) || s[metricRoundErrors] != 0 {
		t.Errorf("claimed %v, last round %v, round errors %v", s[metricClaimed], s[metricLastRound], s[metricRoundErrors])
	}
}

// A failed publish with budget left is counted as a failure, not as a delivery; a rejection is a
// dead letter by code, criticality and storage (ADR-BR-015), not a delivery: dead letters do not
// count in the latency (S5R-D07). A settlement that lost its lease counts as lost, not delivered.
func TestRelayMetricsCountFailuresDeadLettersAndLostLeasesApart(t *testing.T) {
	m := NewRelayMetrics()
	retry, reject := recordedBefore("evt-retry", time.Second), recordedBefore("evt-reject", time.Second)
	revoked := recordedBefore("evt-revoked", time.Second)
	revoked.Envelope.Type = "idp-brant.trd.quote_approval_revoked.v1"
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{retry, reject, revoked}}
	relay := meteredRelay(outbox, eventing.PublisherFunc(func(_ context.Context, e eventing.Envelope) error {
		if e.ID == retry.Envelope.ID {
			return errors.New("sink unavailable")
		}
		return eventing.ErrPublishRejected
	}), m)
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := series(t, m)
	if s[metricFailures+`{failure_code="publish.failed"}`] != 1 {
		t.Errorf("failures %v", s)
	}
	if s[metricDeadLetters+`{failure_code="publish.rejected",critical="false",stored="true"}`] != 1 ||
		s[metricDeadLetters+`{failure_code="publish.rejected",critical="true",stored="true"}`] != 1 {
		t.Errorf("dead letters %v", s)
	}
	for k := range s {
		if strings.HasPrefix(k, metricLatency) {
			t.Errorf("a failure or a dead letter counted as a delivery: %s", k)
		}
	}

	lost := NewRelayMetrics()
	outbox = &fakeOutbox{deliveries: []eventing.Delivery{recordedBefore("evt-lost", time.Second)}, finalizeErr: eventing.ErrLeaseLost}
	relay = meteredRelay(outbox, eventing.PublisherFunc(func(context.Context, eventing.Envelope) error { return nil }), lost)
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s = series(t, lost); s[metricLeaseLost] != 1 || s[metricClaimed] != 1 {
		t.Errorf("lease lost %v", s)
	}
	for k := range s {
		if strings.HasPrefix(k, metricLatency) {
			t.Errorf("a delivery whose lease was lost counted as delivered: %s", k)
		}
	}
}

// A round whose settlement fails counts as a round error and does not move the last round's time,
// so a stalled relay shows in the metrics.
func TestRelayMetricsCountRoundErrors(t *testing.T) {
	m := NewRelayMetrics()
	outbox := &fakeOutbox{deliveries: []eventing.Delivery{recordedBefore("evt-1", time.Second)}, finalizeErr: errors.New("connection reset")}
	relay := meteredRelay(outbox, eventing.PublisherFunc(func(context.Context, eventing.Envelope) error { return nil }), m)
	if _, err := relay.RunOnce(context.Background()); err == nil {
		t.Fatal("want the settlement error")
	}
	s := series(t, m)
	if s[metricRoundErrors] != 1 || s[metricClaimed] != 1 {
		t.Errorf("round errors %v claimed %v", s[metricRoundErrors], s[metricClaimed])
	}
	if _, ok := s[metricLastRound]; ok {
		t.Error("a failed round set the last round's time")
	}
}

// The Dispatcher counts each consumer's outcome of each delivery and its time on it.
func TestDispatcherMetricsCountEachConsumersOutcome(t *testing.T) {
	m := NewRelayMetrics()
	failing := true
	stl := &recorder{fail: func(Event) error {
		if failing {
			return errors.New("credit store unavailable")
		}
		return nil
	}}
	cnx := &recorder{}
	f := newFakeDispatch(t, subscription(stlConsumer, stl.handle, orderActivated), subscription(cnxConsumer, cnx.handle, orderActivated))
	f.SetMetrics(m)
	e := trdEnvelope("evt-metrics", orderActivated)
	if err := f.Publish(context.Background(), e); err == nil {
		t.Fatal("want the retry")
	}
	failing = false
	if err := f.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	s := series(t, m)
	for k, want := range map[string]float64{
		`{consumer="cnx.order_compliance",outcome="processed"}`: 1, `{consumer="cnx.order_compliance",outcome="duplicate"}`: 1,
		`{consumer="stl.order_credit",outcome="retry"}`: 1, `{consumer="stl.order_credit",outcome="processed"}`: 1,
	} {
		if s[metricConsumer+k] != want {
			t.Errorf("%s = %v, want %v", k, s[metricConsumer+k], want)
		}
	}
	if s[metricConsumerTime+`_count{consumer="stl.order_credit"}`] != 2 || s[metricConsumerTime+`_count{consumer="cnx.order_compliance"}`] != 2 {
		t.Errorf("consumer durations %v", s)
	}
}

type fakeBacklog struct {
	backlogs []OutboxBacklog
	errs     []error
}

func (f *fakeBacklog) Read(context.Context) (OutboxBacklog, error) {
	b, err := f.backlogs[0], f.errs[0]
	f.backlogs, f.errs = f.backlogs[1:], f.errs[1:]
	return b, err
}

// S5R-D07: the monitor keeps the backlog in the gauges and logs once per change of level: WARN when
// the oldest undelivered event reaches 60 s, ERROR at 300 s, INFO when it is younger than 60 s
// again; a failed probe is counted and logged and keeps the level and the last backlog.
func TestRelayMonitorAlertsOnTheOldestUndeliveredEvent(t *testing.T) {
	at := func(age time.Duration, pending int64) OutboxBacklog {
		b := OutboxBacklog{At: relayNow, Pending: pending, Retrying: 1, Leased: 2, LeaseExpired: 3}
		if age > 0 {
			b.Oldest = relayNow.Add(-age)
		}
		return b
	}
	probe := &fakeBacklog{
		backlogs: []OutboxBacklog{at(10*time.Second, 4), at(OldestUndeliveredAlert, 5), at(90*time.Second, 6), {},
			at(OldestUndeliveredIncident, 7), at(100*time.Second, 8), at(0, 0)},
		errs: []error{nil, nil, nil, errors.New("connection refused"), nil, nil, nil},
	}
	var logs bytes.Buffer
	m := NewRelayMetrics()
	mon := RelayMonitor{Probe: probe, Metrics: m, Interval: time.Second, Alert: OldestUndeliveredAlert, Incident: OldestUndeliveredIncident,
		Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	if err := mon.validate(); err != nil {
		t.Fatal(err)
	}
	level := lagNormal
	var levels []string
	for range 7 {
		level = mon.check(context.Background(), level)
		levels = append(levels, level.String())
		if len(levels) == 3 {
			s := series(t, m)
			if s[metricOldestAge] != 90 || s[metricBacklog+`{state="pending"}`] != 6 || s[metricBacklog+`{state="retrying"}`] != 1 ||
				s[metricBacklog+`{state="leased"}`] != 2 || s[metricBacklog+`{state="lease_expired"}`] != 3 ||
				s[metricProbeTimestamp] != float64(relayNow.Unix()) {
				t.Fatalf("gauges %v", s)
			}
		}
	}
	if want := "normal alert alert alert incident alert normal"; strings.Join(levels, " ") != want {
		t.Fatalf("levels %v, want %s", levels, want)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		got = append(got, regexp.MustCompile(`level=(\w+) msg="([a-z ]+)`).FindStringSubmatch(line)[1:]...)
	}
	want := []string{"WARN", "outbox relay lag alert", "WARN", "outbox probe failed", "ERROR", "outbox relay lag incident",
		"WARN", "outbox relay lag alert", "INFO", "outbox relay lag recovered"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("logs:\n%s", logs.String())
	}
	if s := series(t, m); s[metricProbeErrors] != 1 || s[metricOldestAge] != 0 || s[metricBacklog+`{state="pending"}`] != 0 {
		t.Fatalf("after recovery %v", s)
	}
	for _, bad := range []RelayMonitor{{}, {Probe: probe, Interval: time.Second, Alert: time.Minute, Incident: time.Minute}} {
		if bad.validate() == nil {
			t.Fatalf("%+v accepted", bad)
		}
	}
}

// Before the first probe the backlog gauges have no samples, so a monitoring system sees them
// absent instead of an empty Outbox.
func TestRelayMetricsHaveNoBacklogBeforeTheFirstProbe(t *testing.T) {
	s := series(t, NewRelayMetrics())
	for k := range s {
		if strings.HasPrefix(k, metricBacklog) || strings.HasPrefix(k, metricOldestAge) || strings.HasPrefix(k, metricProbeTimestamp) {
			t.Fatalf("sample %s before a probe", k)
		}
	}
	if s[metricClaimed] != 0 || s[metricProbeErrors] != 0 {
		t.Fatalf("%v", s)
	}
}

// The handler serves the text format on GET and HEAD only, and escapes label values.
func TestRelayMetricsHandlerServesTheTextFormat(t *testing.T) {
	m := NewRelayMetrics()
	m.published("idp-brant.\"odd\"\\source\n", time.Second)
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("%s %q", resp.Status, resp.Header.Get("Content-Type"))
	}
	s := parseExposition(t, string(body))
	if s[metricLatency+`_count{source="idp-brant.\"odd\"\\source\n"}`] != 1 {
		t.Fatalf("escaped label not found in\n%s", body)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "# TYPE ") && len(strings.Fields(line)) != 4 {
			t.Fatalf("type line %q", line)
		}
	}
	resp, err = http.Post(srv.URL, "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST: %s", resp.Status)
	}
}

// A nil *RelayMetrics counts nothing and does not stop the relay or the dispatcher.
func TestNilRelayMetricsCountNothing(t *testing.T) {
	var m *RelayMetrics
	m.round(1, relayNow, nil)
	m.published("s", time.Second)
	m.failed(FailurePublishFailed)
	m.deadLettered(DeadLetterAlert{})
	m.lostLease()
	m.consumed(stlConsumer, InboxProcessed, time.Second)
	m.observeBacklog(OutboxBacklog{})
	m.probeFailed()
}
