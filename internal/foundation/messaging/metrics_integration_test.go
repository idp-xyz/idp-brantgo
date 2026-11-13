//go:build integration

package messaging_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
)

// S5R-D07: the probe counts every undelivered event of the Outbox by state, from Bento's index on
// undelivered rows, and dates the oldest by its recorded_at; published and abandoned deliveries
// (dead letters) are not part of the backlog.
func TestOutboxProbeReadsTheUndeliveredBacklog(t *testing.T) {
	db := outboxDB(t)
	store := newStore(t, db)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	probeAt := &testClock{now: t0.Add(5 * time.Minute)}
	reader := messaging.OutboxProbe{DB: db, Clock: probeAt}

	empty, err := reader.Read(ctx)
	if err != nil || empty.Events() != 0 || !empty.Oldest.IsZero() || empty.OldestAge() != 0 || !empty.At.Equal(probeAt.now) {
		t.Fatalf("empty outbox: %+v, %v", empty, err)
	}

	expired, retrying := probe("probe-expired", "partition-b", t0), probe("probe-retrying", "partition-c", t0.Add(20*time.Second))
	published, abandoned := probe("probe-published", "partition-e", t0.Add(-time.Hour)), probe("probe-abandoned", "partition-f", t0.Add(-2*time.Hour))
	enqueueAll(t, db, store, expired, retrying, published, abandoned)
	claimAt := t0.Add(time.Minute)
	first, err := store.Claim(ctx, eventing.OutboxClaim{Now: claimAt, Limit: 10, LeaseFor: 10 * time.Second, MaxAttempts: 10})
	if err != nil || len(first) != 4 {
		t.Fatalf("claim: %d, %v", len(first), err)
	}
	for _, d := range first {
		switch d.Envelope.ID {
		case published.ID:
			err = store.MarkPublished(ctx, d.Ref, claimAt)
		case abandoned.ID:
			err = store.Abandon(ctx, d.Ref, claimAt, "publish.rejected")
		case retrying.ID:
			err = store.RecordFailure(ctx, d.Ref, eventing.DeliveryFailure{FailedAt: claimAt, RetryAt: t0.Add(time.Hour),
				Code: messaging.FailurePublishFailed, MaxFailures: 5})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	leased, pending := probe("probe-leased", "partition-d", t0.Add(30*time.Second)), probe("probe-pending", "partition-a", t0.Add(40*time.Second))
	enqueueAll(t, db, store, leased, pending)
	second, err := store.Claim(ctx, eventing.OutboxClaim{Now: claimAt, Limit: 1, LeaseFor: time.Hour, MaxAttempts: 10})
	if err != nil || len(second) != 1 || second[0].Envelope.ID != leased.ID {
		t.Fatalf("second claim: %v, %v", second, err)
	}

	b, err := reader.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := messaging.OutboxBacklog{At: probeAt.now, Pending: 1, Retrying: 1, Leased: 1, LeaseExpired: 1, Oldest: t0}
	if b != want || b.Events() != 4 || b.OldestAge() != 5*time.Minute {
		t.Fatalf("backlog %+v, want %+v (4 events, oldest 5 min)", b, want)
	}
}

// syncBuffer collects a process's output while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var sampleRE = regexp.MustCompile(`(?m)^([a-z_]+(?:\{[^}]*\})?) (\S+)$`)

func scrape(t *testing.T, url string) map[string]float64 {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s, %v", url, resp.Status, err)
	}
	out := map[string]float64{}
	for _, m := range sampleRE.FindAllStringSubmatch(string(body), -1) {
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			t.Fatalf("%q: %v", m[0], err)
		}
		out[m[1]] = v
	}
	return out
}

func backlogOf(s map[string]float64) float64 {
	var n float64
	for _, state := range []string{"pending", "retrying", "leased", "lease_expired"} {
		n += s[`brant_outbox_backlog_events{state="`+state+`"}`]
	}
	return n
}

// S5R-D07 end to end in the worker process: brant-worker -metrics-listen serves the relay latency
// from recorded_at to publish, the publish failures, the backlog and the age of the oldest
// undelivered event, and logs a lag alert when that age passes -lag-alert and the recovery when the
// event is delivered.
func TestWorkerServesRelayMetricsAndAlertsOnLag(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "brant-worker")
	if out, err := exec.Command("go", "build", "-o", bin, "go.idp.xyz/idp-brant/cmd/brant-worker").CombinedOutput(); err != nil {
		t.Fatalf("build brant-worker: %v\n%s", err, out)
	}
	db := outboxDB(t)
	store := newStore(t, db)
	now := time.Now().UTC()
	fresh1, fresh2 := probe("metrics-fresh-1", "metrics-1", now), probe("metrics-fresh-2", "metrics-2", now)
	late := probe("metrics-late", "metrics-3", now.Add(-2*time.Minute)) // older than the 60 s alert, younger than the 300 s incident
	enqueueAll(t, db, store, fresh1, fresh2, late)

	var accept atomic.Bool
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") == string(late.ID) && !accept.Load() {
			w.WriteHeader(http.StatusServiceUnavailable) // retried with backoff
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sink.Close()

	logs := &syncBuffer{}
	cmd := exec.Command(bin, "-schema", db.Schema(), "-sink", sink.URL, "-batch", "5", "-lease", "10s", "-sink-timeout", "1s",
		"-poll", "50ms", "-retry-base", "100ms", "-retry-max", "500ms", "-max-failures", "1000", "-max-attempts", "5000",
		"-probe-interval", "100ms", "-metrics-listen", "127.0.0.1:0")
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("%s did not happen within 30 s; worker log:\n%s", what, logs)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	addr := regexp.MustCompile(`msg="serving metrics" address=(\S+)`)
	waitFor("the metrics server", func() bool { return addr.MatchString(logs.String()) })
	url := "http://" + addr.FindStringSubmatch(logs.String())[1] + "/metrics"

	waitFor("the fresh events' delivery and the lag alert", func() bool {
		return readOutbox(t, db, fresh1.ID).published && readOutbox(t, db, fresh2.ID).published &&
			strings.Contains(logs.String(), "outbox relay lag alert")
	})
	src := `source="idp-brant.relay-probe"`
	var s map[string]float64
	waitFor("a probe after the fresh deliveries", func() bool {
		s = scrape(t, url)
		return backlogOf(s) == 1 && s["brant_outbox_relay_publish_failures_total{failure_code=\"publish.failed\"}"] >= 1
	})
	if s["brant_outbox_relay_delivery_latency_seconds_count{"+src+"}"] != 2 ||
		s["brant_outbox_relay_delivery_latency_seconds_bucket{"+src+`,le="30"}`] != 2 ||
		s["brant_outbox_oldest_undelivered_age_seconds"] < 120 || s["brant_outbox_relay_last_round_timestamp_seconds"] == 0 {
		t.Fatalf("while the late event waits: %v", s)
	}
	if !regexp.MustCompile(`level=WARN msg="outbox relay lag alert[^"]*" lag_level=alert oldest_undelivered_age_s=1[2-9]\d`).MatchString(logs.String()) {
		t.Fatalf("lag alert line:\n%s", logs)
	}

	accept.Store(true)
	waitFor("the late event's delivery and the recovery", func() bool {
		return readOutbox(t, db, late.ID).published && strings.Contains(logs.String(), "outbox relay lag recovered")
	})
	waitFor("a probe after the recovery", func() bool { s = scrape(t, url); return backlogOf(s) == 0 })
	if s["brant_outbox_relay_delivery_latency_seconds_count{"+src+"}"] != 3 ||
		s["brant_outbox_relay_delivery_latency_seconds_bucket{"+src+`,le="60"}`] != 2 ||
		s["brant_outbox_relay_delivery_latency_seconds_bucket{"+src+`,le="300"}`] != 3 ||
		s["brant_outbox_oldest_undelivered_age_seconds"] != 0 {
		t.Fatalf("after the recovery: %v", s)
	}
	for k := range s {
		if strings.HasPrefix(k, "brant_outbox_relay_dead_letters_total") {
			t.Fatalf("a retried delivery counted as a dead letter: %s", k)
		}
	}
	if strings.Contains(logs.String(), "outbox relay lag incident") {
		t.Fatalf("an incident was logged for an event younger than 300 s:\n%s", logs)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	stopped = true
	if err := cmd.Wait(); err != nil {
		t.Fatalf("graceful shutdown: %v\n%s", err, logs)
	}
}
