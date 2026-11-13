package main

import (
	"strings"
	"testing"
	"time"
)

// WBS S5-11 (baseline 11.1 交互体验: P50, P95, P99, 错误率与超时分开): the percentiles are nearest-rank
// over the successful answers of a window, errors and timeouts are counted apart, and a request
// belongs to the window it completed in.
func TestPercentilesAreNearestRank(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 100; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	l := summarize(d)
	if l.Count != 100 || l.P50 != 50 || l.P95 != 95 || l.P99 != 99 || l.Max != 100 || l.Mean != 50.5 {
		t.Fatalf("latency of 1..100 ms: %+v", l)
	}
	if l := summarize([]time.Duration{7 * time.Millisecond}); l.P50 != 7 || l.P99 != 7 {
		t.Fatalf("one sample: %+v", l)
	}
	if l := summarize(nil); l != (Latency{}) {
		t.Fatalf("no sample: %+v", l)
	}
	// Unsorted input is not reordered in place.
	in := []time.Duration{3 * time.Millisecond, time.Millisecond, 2 * time.Millisecond}
	if summarize(in).P50 != 2 || in[0] != 3*time.Millisecond {
		t.Fatalf("input changed: %v", in)
	}
}

func TestAggregateSeparatesErrorsTimeoutsAndWindow(t *testing.T) {
	from := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	to := from.Add(10 * time.Second)
	at := func(s int) time.Time { return from.Add(time.Duration(s) * time.Second) }
	samples := []sample{
		{end: at(1), dur: 10 * time.Millisecond, class: classCommand, name: "trd quote.freeze", outcome: outcomeOK},
		{end: at(2), dur: 30 * time.Millisecond, class: classCommand, name: "trd quote.freeze", outcome: outcomeOK},
		{end: at(3), dur: 900 * time.Millisecond, class: classCommand, name: "stl credit.reserve", outcome: "http_409", code: "CONFLICT"},
		{end: at(4), dur: 15 * time.Second, class: classCommand, name: "trd order.activate", outcome: outcomeTimeout},
		{end: at(5), dur: time.Millisecond, class: classCommand, name: "trd order.activate", outcome: outcomeTransport},
		{end: at(6), dur: 5 * time.Millisecond, class: classRead, name: "GET trd.quote", outcome: outcomeOK},
		{end: from.Add(-time.Millisecond), dur: time.Millisecond, class: classCommand, name: "warmup", outcome: outcomeOK},
		{end: to, dur: time.Millisecond, class: classCommand, name: "after", outcome: outcomeOK},
	}
	c := aggregate(samples, classCommand, from, to)
	if c.Requests != 5 || c.OK != 2 || c.Errors != 1 || c.Timeouts != 1 || c.Transport != 1 {
		t.Fatalf("counts %+v", c)
	}
	if c.ErrorRate != 0.4 || c.TimeoutPct != 0.2 || c.RPS != 0.2 {
		t.Fatalf("rates %+v", c)
	}
	if c.Latency.Count != 2 || c.Latency.P95 != 30 || c.Latency.Max != 30 {
		t.Fatalf("latency over ok answers only %+v", c.Latency)
	}
	if c.ErrorCodes["409 CONFLICT"] != 1 || c.ByName["trd order.activate"].Errors != 2 || c.ByName["trd quote.freeze"].Latency.P50 != 10 {
		t.Fatalf("breakdown %+v %+v", c.ErrorCodes, c.ByName)
	}
	if _, ok := c.ByName["warmup"]; ok {
		t.Fatal("a sample before the window was counted")
	}
	if r := aggregate(samples, classRead, from, to); r.Requests != 1 || r.OK != 1 || r.Latency.P99 != 5 {
		t.Fatalf("reads %+v", r)
	}
}

func TestCycleStatsCountOnlyTheWindow(t *testing.T) {
	from := time.Now()
	to := from.Add(10 * time.Second)
	log := &cycleLog{}
	log.add(cycleResult{end: from.Add(time.Second), dur: 600 * time.Millisecond}, nil)
	log.add(cycleResult{end: from.Add(2 * time.Second), dur: 700 * time.Millisecond}, nil)
	log.add(cycleResult{end: from.Add(3 * time.Second), step: "stl credit.reserve", code: "409 CONFLICT"}, &stepError{step: "stl credit.reserve", err: &refusal{Status: 409, Code: "CONFLICT"}})
	log.add(cycleResult{end: from.Add(-time.Second), dur: time.Second}, nil)
	cs := cycleStats(log, from, to)
	if cs.Completed != 2 || cs.Failed != 1 || cs.PerSecond != 0.2 || cs.Latency.P95 != 700 || cs.Failures["stl credit.reserve 409 CONFLICT"] != 1 {
		t.Fatalf("cycles %+v", cs)
	}
}

func TestFlagsAndDSN(t *testing.T) {
	if _, err := parseFlags([]string{"-dsn", "postgres://a@h/db", "-stages", "1,0"}); err == nil {
		t.Fatal("a stage of 0 users was accepted")
	}
	if _, err := parseFlags([]string{"-dsn", "postgres://a@h/db", "-mode", "writes"}); err == nil {
		t.Fatal("writes mode without an acknowledgement log was accepted")
	}
	cfg, err := parseFlags([]string{"-dsn", "postgres://a@h/db", "-stages", "1, 4,16"})
	if err != nil || len(cfg.stages) != 3 || cfg.stages[2] != 16 || !strings.HasPrefix(cfg.tenant, "load.") {
		t.Fatalf("%+v %v", cfg, err)
	}
	dsn, err := withParams("postgres://brant_app:pw@127.0.0.1:55850/brant?sslmode=disable",
		map[string]string{"application_name": appAPI, "pool_max_conns": ""})
	if err != nil || !strings.Contains(dsn, "application_name="+appAPI) || !strings.Contains(dsn, "sslmode=disable") ||
		strings.Contains(dsn, "pool_max_conns") {
		t.Fatalf("%s %v", dsn, err)
	}
	if _, err = withParams("host=127.0.0.1 user=x", nil); err == nil {
		t.Fatal("a keyword DSN was accepted")
	}
}

func TestStampMatchesTheContractsTimestamp(t *testing.T) {
	got := stamp(time.Date(2026, 11, 12, 20, 1, 2, 123456789, time.FixedZone("CST", 8*3600)))
	if got != "2026-11-12T12:01:02.123456Z" {
		t.Fatal(got)
	}
}
