package main

import (
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// sample is one completed request.
type sample struct {
	end     time.Time
	dur     time.Duration
	class   string
	name    string // "GET trd.quote" or "trd quote.freeze"
	outcome string
	code    string
}

// recorder collects samples from every virtual user.
type recorder struct {
	mu      sync.Mutex
	samples []sample
}

func (r *recorder) add(s sample) {
	r.mu.Lock()
	r.samples = append(r.samples, s)
	r.mu.Unlock()
}

// take returns the samples recorded so far and starts afresh.
func (r *recorder) take() []sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.samples
	r.samples = nil
	return out
}

// Latency is a latency distribution in milliseconds (nearest-rank percentiles).
type Latency struct {
	Count int     `json:"count"`
	Mean  float64 `json:"mean_ms"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
	Max   float64 `json:"max_ms"`
}

// percentile is the nearest-rank percentile p (0 < p <= 100) of sorted.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func ms(d time.Duration) float64 { return math.Round(float64(d.Microseconds())/10) / 100 }

func summarize(durations []time.Duration) Latency {
	if len(durations) == 0 {
		return Latency{}
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}
	return Latency{Count: len(sorted), Mean: ms(sum / time.Duration(len(sorted))), P50: ms(percentile(sorted, 50)),
		P95: ms(percentile(sorted, 95)), P99: ms(percentile(sorted, 99)), Max: ms(sorted[len(sorted)-1])}
}

// ClassStats is one request class (reads or commands) in a stage window.
type ClassStats struct {
	Requests   int                  `json:"requests"`
	OK         int                  `json:"ok"`
	Errors     int                  `json:"errors"`   // HTTP answers other than 2xx
	Timeouts   int                  `json:"timeouts"` // no answer within the client timeout
	Transport  int                  `json:"transport_errors"`
	ErrorRate  float64              `json:"error_rate"`   // errors / requests
	TimeoutPct float64              `json:"timeout_rate"` // timeouts / requests
	RPS        float64              `json:"rps"`          // ok answers per second
	Latency    Latency              `json:"latency_ok"`
	ErrorCodes map[string]int       `json:"error_codes,omitempty"`
	ByName     map[string]NameStats `json:"by_name"`
}

// NameStats is one route or command in a stage window.
type NameStats struct {
	Requests int     `json:"requests"`
	Errors   int     `json:"errors"`
	Latency  Latency `json:"latency_ok"`
}

// aggregate summarizes the samples of class that completed in [from, to).
func aggregate(samples []sample, class string, from, to time.Time) ClassStats {
	cs := ClassStats{ErrorCodes: map[string]int{}, ByName: map[string]NameStats{}}
	var durations []time.Duration
	perName := map[string][]time.Duration{}
	errsByName := map[string]int{}
	countByName := map[string]int{}
	for _, s := range samples {
		if s.class != class || s.end.Before(from) || !s.end.Before(to) {
			continue
		}
		cs.Requests++
		countByName[s.name]++
		switch s.outcome {
		case outcomeOK:
			cs.OK++
			durations = append(durations, s.dur)
			perName[s.name] = append(perName[s.name], s.dur)
		case outcomeTimeout:
			cs.Timeouts++
			errsByName[s.name]++
		case outcomeTransport:
			cs.Transport++
			errsByName[s.name]++
		default:
			cs.Errors++
			errsByName[s.name]++
			cs.ErrorCodes[strings.TrimPrefix(s.outcome, "http_")+" "+s.code]++
		}
	}
	if cs.Requests > 0 {
		cs.ErrorRate = float64(cs.Errors+cs.Transport) / float64(cs.Requests)
		cs.TimeoutPct = float64(cs.Timeouts) / float64(cs.Requests)
	}
	if secs := to.Sub(from).Seconds(); secs > 0 {
		cs.RPS = math.Round(float64(cs.OK)/secs*100) / 100
	}
	cs.Latency = summarize(durations)
	for name, n := range countByName {
		cs.ByName[name] = NameStats{Requests: n, Errors: errsByName[name], Latency: summarize(perName[name])}
	}
	return cs
}
