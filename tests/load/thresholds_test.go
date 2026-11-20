package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func defaultThresholds() thresholds {
	return thresholds{assert: true, relayP95: defaultRelayP95, relayP99: defaultRelayP99, alert: defaultLagAlert,
		readP95: defaultReadP95, commandP95: defaultCommandP95, referenceUsers: defaultReferenceUsers, apiUsers: defaultAPIUsers}
}

// stageWith is a stage of users command users with the given P95s (ms), relay lag (s) and oldest
// undelivered age (s).
func stageWith(n, users int, readP95, commandP95, relayP95, relayP99, oldest float64) StageReport {
	s := StageReport{Stage: n, CommandUsers: users, ReadUsers: users}
	s.Reads.OK, s.Reads.Latency.P95 = 100, readP95
	s.Commands.OK, s.Commands.Latency.P95 = 100, commandP95
	s.DB.Samples, s.DB.OldestUndeliveredMaxS = 50, oldest
	s.DB.RelayLag = RelayLag{Published: 500, P95: relayP95, P99: relayP99}
	return s
}

func statuses(r *ThresholdReport) map[string]string {
	out := map[string]string{}
	for _, c := range r.Checks {
		out[strings.Join([]string{itoa(c.Stage), c.Target}, " ")] = c.Status
	}
	return out
}

func itoa(n int) string { return string(rune('0' + n)) }

// S5R-D07 and S5-11: the relay targets are asserted up to the reference load (16 command users)
// with the reference 2 dispatch workers, the read and command targets up to 32 command users per
// API instance; above those loads a stage is measured and reported, not asserted.
func TestThresholdsApplyUpToTheirLoad(t *testing.T) {
	stages := []StageReport{
		stageWith(1, 8, 37, 103, 1.2, 4.0, 3),
		stageWith(2, 16, 72, 176, 6.1, 29.0, 61), // P95 and the oldest event miss S5R-D07
		stageWith(3, 32, 165, 363, 82.0, 120.0, 200),
		stageWith(4, 64, 368, 834, 117.0, 150.0, 300),
	}
	r := evaluate(defaultThresholds(), 2, true, stages)
	got := statuses(r)
	want := map[string]string{
		"1 read_latency_p95": statusPass, "1 command_latency_p95": statusPass, "1 relay_latency_p95": statusPass,
		"1 relay_latency_p99": statusPass, "1 oldest_undelivered_below_alert": statusPass,
		"2 read_latency_p95": statusPass, "2 command_latency_p95": statusPass, "2 relay_latency_p95": statusFail,
		"2 relay_latency_p99": statusPass, "2 oldest_undelivered_below_alert": statusFail,
		"3 read_latency_p95": statusPass, "3 command_latency_p95": statusPass, "3 relay_latency_p95": statusNotAsserted,
		"3 relay_latency_p99": statusNotAsserted, "3 oldest_undelivered_below_alert": statusNotAsserted,
		"4 read_latency_p95": statusNotAsserted, "4 command_latency_p95": statusNotAsserted, "4 relay_latency_p95": statusNotAsserted,
		"4 relay_latency_p99": statusNotAsserted, "4 oldest_undelivered_below_alert": statusNotAsserted,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s, want %s", k, got[k], v)
		}
	}
	if len(r.Checks) != 20 || r.Failed != 2 || r.Verdict != statusFail || r.Workers != 2 {
		t.Fatalf("report %+v", r)
	}
	for _, c := range r.Checks {
		if c.Stage == 2 && c.Target == "relay_latency_p95" && (c.LimitS != 5 || *c.ObservedS != 6.1 || c.Basis != "S5R-D07") {
			t.Fatalf("check %+v", c)
		}
		if c.Stage == 4 && c.Target == "command_latency_p95" && (c.LimitS != 0.8 || math.Abs(*c.ObservedS-0.834) > 1e-9 || !strings.Contains(c.Note, "32")) {
			t.Fatalf("check %+v", c)
		}
	}
}

// The relay targets hold for the reference deployment: with one worker they are measured but not
// asserted, without a relay or an observer they are not measured, and a run whose every check is
// unasserted has no verdict.
func TestRelayThresholdsNeedTheReferenceDeployment(t *testing.T) {
	stages := []StageReport{stageWith(1, 8, 37, 103, 20.3, 40, 90)}
	one := evaluate(defaultThresholds(), 1, true, stages)
	if s := statuses(one); s["1 relay_latency_p95"] != statusNotAsserted || s["1 oldest_undelivered_below_alert"] != statusNotAsserted ||
		one.Failed != 0 || one.Verdict != statusPass {
		t.Fatalf("one worker: %+v", one)
	}
	if !strings.Contains(one.Checks[2].Note, "1 dispatch workers, the reference deployment runs 2") {
		t.Fatalf("note %q", one.Checks[2].Note)
	}
	none := evaluate(defaultThresholds(), 0, true, stages)
	for _, c := range none.Checks[2:] {
		if c.Status != statusNotMeasured || c.ObservedS != nil || c.Note != "no relay (-workers 0)" {
			t.Fatalf("no relay: %+v", c)
		}
	}
	blind := evaluate(defaultThresholds(), 2, false, stages)
	if c := blind.Checks[2]; c.Status != statusNotMeasured || c.Note != "no observer (-observer-dsn)" {
		t.Fatalf("no observer: %+v", c)
	}
	idle := stageWith(1, 1, 30, 70, 0, 0, 0)
	idle.DB.RelayLag.Published, idle.Reads.OK = 0, 0
	r := evaluate(defaultThresholds(), 2, true, []StageReport{idle})
	if s := statuses(r); s["1 relay_latency_p95"] != statusNotMeasured || s["1 read_latency_p95"] != statusNotMeasured ||
		s["1 oldest_undelivered_below_alert"] != statusPass || s["1 command_latency_p95"] != statusPass {
		t.Fatalf("idle stage: %v", s)
	}
	high := evaluate(defaultThresholds(), 2, true, []StageReport{stageWith(1, 64, 368, 834, 117, 150, 300)})
	if high.Verdict != "none" || high.Failed != 0 {
		t.Fatalf("nothing asserted: %+v", high)
	}
}

const workerExposition = `# HELP brant_outbox_relay_delivery_latency_seconds Time from ...
# TYPE brant_outbox_relay_delivery_latency_seconds histogram
brant_outbox_relay_delivery_latency_seconds_bucket{source="idp-brant.trd",le="1"} 50
brant_outbox_relay_delivery_latency_seconds_bucket{source="idp-brant.trd",le="5"} 90
brant_outbox_relay_delivery_latency_seconds_bucket{source="idp-brant.trd",le="30"} 99
brant_outbox_relay_delivery_latency_seconds_bucket{source="idp-brant.trd",le="+Inf"} 100
brant_outbox_relay_delivery_latency_seconds_sum{source="idp-brant.trd"} 210.5
brant_outbox_relay_delivery_latency_seconds_count{source="idp-brant.trd"} 100
brant_outbox_relay_delivery_latency_seconds_bucket{source="idp-brant.stl",le="1"} 10
brant_outbox_relay_delivery_latency_seconds_bucket{source="idp-brant.stl",le="5"} 10
brant_outbox_relay_delivery_latency_seconds_bucket{source="idp-brant.stl",le="30"} 10
brant_outbox_relay_delivery_latency_seconds_bucket{source="idp-brant.stl",le="+Inf"} 10
brant_outbox_relay_delivery_latency_seconds_count{source="idp-brant.stl"} 10
# TYPE brant_outbox_relay_publish_failures_total counter
brant_outbox_relay_publish_failures_total{failure_code="publish.failed"} 3
brant_outbox_relay_dead_letters_total{failure_code="publish.rejected",critical="false",stored="true"} 1
brant_outbox_backlog_events{state="pending"} 7
brant_outbox_relay_settle_duration_seconds_bucket{le="+Inf"} 110
brant_outbox_relay_settle_duration_seconds_sum 2.2
brant_outbox_relay_settle_duration_seconds_count 110
`

// The workers' metrics (S5R-D07) are summed over sources and workers; a window's share within a
// target is the difference of the cumulative bucket at the target's bound over the published.
func TestWorkerMetricsWindow(t *testing.T) {
	before, err := parseWorkerMetrics(strings.NewReader(workerExposition))
	if err != nil {
		t.Fatal(err)
	}
	if before.published != 110 || before.buckets[5] != 100 || before.buckets[math.Inf(1)] != 110 || before.failures != 3 || before.deadLetters != 1 {
		t.Fatalf("parsed %+v", before)
	}
	two := workerCounters{buckets: map[float64]float64{}}
	two.add(before)
	two.add(before) // two workers' scrapes add up
	if two.published != 220 || two.buckets[30] != 218 || two.failures != 6 {
		t.Fatalf("sum %+v", two)
	}
	// 210 deliveries in the window: 180 within 5 s, 209 within 30 s.
	after := workerCounters{buckets: map[float64]float64{1: 160, 5: 280, 30: 318, math.Inf(1): 320}, published: 320, failures: 5, deadLetters: 1,
		settleSum: 2.2 + 21, settleCount: 320}
	w := workerWindow(before, after, 2, 1, defaultThresholds())
	if w.Workers != 2 || w.Published != 210 || w.ShareP95 != round(180.0/210, 4) || w.ShareP99 != round(209.0/210, 4) ||
		w.P95Met || !w.P99Met || w.Failures != 2 || w.DeadLetters != 0 || w.ScrapeErrors != 1 || w.SettleMeanS != 0.1 {
		t.Fatalf("window %+v", w)
	}
	if _, err := parseWorkerMetrics(strings.NewReader("brant_outbox_relay_claimed_total x\n")); err == nil {
		t.Fatal("a sample without a number was accepted")
	}
	if within(before, after, 0.5) != 0 {
		t.Fatal("no bucket below the limit counts nothing")
	}
}

// S5R-D07: the reference deployment runs 2 dispatch workers, so a run starts 2 unless told
// otherwise; -worker=false (the integration smoke test, the PITR drill) starts none. -assert needs
// the observer to measure the relay.
func TestWorkerAndThresholdFlags(t *testing.T) {
	cfg, err := parseFlags([]string{"-dsn", "postgres://x@127.0.0.1/brant"})
	if err != nil || cfg.workers != 2 || cfg.th.assert || cfg.th.relayP95 != 5*time.Second || cfg.th.relayP99 != 30*time.Second ||
		cfg.th.alert != time.Minute || cfg.th.readP95 != 500*time.Millisecond || cfg.th.commandP95 != 800*time.Millisecond ||
		cfg.th.referenceUsers != 16 || cfg.th.apiUsers != 32 {
		t.Fatalf("defaults %+v, %v", cfg, err)
	}
	if cfg, err = parseFlags([]string{"-dsn", "postgres://x@127.0.0.1/brant", "-worker=false", "-workers", "3"}); err != nil || cfg.workers != 0 {
		t.Fatalf("-worker=false: %d, %v", cfg.workers, err)
	}
	if cfg, err = parseFlags([]string{"-dsn", "postgres://x@127.0.0.1/brant", "-workers", "0", "-assert"}); err != nil || cfg.workers != 0 || !cfg.th.assert {
		t.Fatalf("-workers 0 -assert: %+v, %v", cfg, err)
	}
	for _, bad := range [][]string{
		{"-assert"}, {"-workers", "-1"}, {"-relay-p95", "0s"}, {"-relay-p95", "10s", "-relay-p99", "5s"}, {"-reference-users", "0"},
	} {
		if _, err = parseFlags(append([]string{"-dsn", "postgres://x@127.0.0.1/brant"}, bad...)); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
}
