package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Application names the load generator gives the API's and the worker's connections, so the
// observer can tell their sessions apart in pg_stat_activity.
const (
	appAPI    = "brant-api-load"
	appWorker = "brant-worker-load"
)

// observer samples PostgreSQL while a stage runs: the API's connection pool as the server sees it,
// sessions waiting on locks, ungranted locks, and the Outbox backlog (baseline 11.1 数据库与任务:
// 连接数、锁等待、事务冲突、队列积压及单租户争用). It connects with its own DSN, ideally a superuser or a
// member of pg_read_all_stats; with less it sees fewer columns of other roles' sessions.
type observer struct {
	pool *pgxpool.Pool
}

func newObserver(ctx context.Context, dsn string) (*observer, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.RuntimeParams["application_name"] = "brant-load-observer"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &observer{pool: pool}, nil
}

func (o *observer) close() { o.pool.Close() }

// probe is one sample.
type probe struct {
	at                                  time.Time
	apiConns, apiActive, apiIdleTx      int
	workerConns, lockWaiters, ungranted int
	longestLockWait                     float64 // seconds a session has waited on a lock, at most
	outboxPending, outboxLeased         int
	waits                               map[string]int // wait_event_type:wait_event of active API sessions
}

func (o *observer) probe(ctx context.Context) (probe, error) {
	p := probe{at: time.Now(), waits: map[string]int{}}
	rows, err := o.pool.Query(ctx, `SELECT coalesce(application_name, ''), coalesce(state, ''), coalesce(wait_event_type, ''),
		coalesce(wait_event, ''), coalesce(extract(epoch FROM now() - query_start), 0)::float8
		FROM pg_stat_activity WHERE datname = current_database() AND backend_type = 'client backend'`)
	if err != nil {
		return p, fmt.Errorf("pg_stat_activity: %w", err)
	}
	for rows.Next() {
		var app, state, waitType, wait string
		var running float64
		if err = rows.Scan(&app, &state, &waitType, &wait, &running); err != nil {
			rows.Close()
			return p, err
		}
		switch app {
		case appAPI:
			p.apiConns++
			switch state {
			case "active":
				p.apiActive++
				if waitType != "" {
					p.waits[waitType+":"+wait]++
				}
			case "idle in transaction":
				p.apiIdleTx++
			}
		case appWorker:
			p.workerConns++
		}
		if waitType == "Lock" {
			p.lockWaiters++
			p.longestLockWait = math.Max(p.longestLockWait, running)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return p, err
	}
	if err = o.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&p.ungranted); err != nil {
		return p, fmt.Errorf("pg_locks: %w", err)
	}
	if err = o.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'PENDING'), count(*) FILTER (WHERE status = 'LEASED')
		FROM bento.outbox WHERE status IN ('PENDING', 'LEASED')`).Scan(&p.outboxPending, &p.outboxLeased); err != nil {
		return p, fmt.Errorf("bento.outbox: %w", err)
	}
	return p, nil
}

// backlogOf counts the PENDING and LEASED events of one scope ("tenant/company").
func (o *observer) backlogOf(ctx context.Context, scope string) (int, error) {
	var n int
	err := o.pool.QueryRow(ctx, `SELECT count(*) FROM bento.outbox WHERE status IN ('PENDING', 'LEASED') AND scope = $1`, scope).Scan(&n)
	return n, err
}

// dbCounters are the database's cumulative counters (pg_stat_database).
type dbCounters struct {
	commits, rollbacks, deadlocks, conflicts, tempFiles, blksRead, blksHit, inserted int64
}

func (o *observer) counters(ctx context.Context) (dbCounters, error) {
	var c dbCounters
	err := o.pool.QueryRow(ctx, `SELECT xact_commit, xact_rollback, deadlocks, conflicts, temp_files, blks_read, blks_hit, tup_inserted
		FROM pg_stat_database WHERE datname = current_database()`).
		Scan(&c.commits, &c.rollbacks, &c.deadlocks, &c.conflicts, &c.tempFiles, &c.blksRead, &c.blksHit, &c.inserted)
	return c, err
}

// relayLag is the Outbox relay's delay (published_at − recorded_at) of the events published in
// [from, to).
func (o *observer) relayLag(ctx context.Context, from, to time.Time) (RelayLag, error) {
	var l RelayLag
	var p50, p95, mx *float64
	err := o.pool.QueryRow(ctx, `SELECT count(*),
		percentile_disc(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM published_at - recorded_at))::float8,
		percentile_disc(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM published_at - recorded_at))::float8,
		max(extract(epoch FROM published_at - recorded_at))::float8
		FROM bento.outbox WHERE published_at >= $1 AND published_at < $2`, from, to).Scan(&l.Published, &p50, &p95, &mx)
	if err != nil {
		return l, err
	}
	for _, v := range []struct {
		dst *float64
		src *float64
	}{{&l.P50, p50}, {&l.P95, p95}, {&l.Max, mx}} {
		if v.src != nil {
			*v.dst = math.Round(*v.src*1000) / 1000
		}
	}
	return l, nil
}

// RelayLag summarizes the relay delay of the events published in a stage window, in seconds.
type RelayLag struct {
	Published int64   `json:"published"`
	P50       float64 `json:"p50_s"`
	P95       float64 `json:"p95_s"`
	Max       float64 `json:"max_s"`
}

// DBStats is what the observer saw during a stage window.
type DBStats struct {
	Samples           int            `json:"samples"`
	APIConnsMax       int            `json:"api_connections_max"`
	APIActiveMean     float64        `json:"api_active_mean"`
	APIActiveMax      int            `json:"api_active_max"`
	APIIdleInTxMax    int            `json:"api_idle_in_transaction_max"`
	WorkerConnsMax    int            `json:"worker_connections_max"`
	LockWaitSamples   int            `json:"samples_with_lock_waiters"`
	LockWaitersMax    int            `json:"lock_waiters_max"`
	LockWaitersMean   float64        `json:"lock_waiters_mean"`
	LongestLockWaitS  float64        `json:"longest_lock_wait_s"`
	UngrantedMax      int            `json:"ungranted_locks_max"`
	OutboxBacklogMax  int            `json:"outbox_backlog_max"`
	OutboxBacklogEnd  int            `json:"outbox_backlog_end"`
	WaitEvents        map[string]int `json:"wait_events_of_active_api_sessions"`
	Commits           int64          `json:"commits"`
	Rollbacks         int64          `json:"rollbacks"`
	Deadlocks         int64          `json:"deadlocks"`
	Conflicts         int64          `json:"conflicts"`
	TempFiles         int64          `json:"temp_files"`
	BlocksRead        int64          `json:"blocks_read"`
	CacheHitRatio     float64        `json:"cache_hit_ratio"`
	TuplesInserted    int64          `json:"tuples_inserted"`
	RelayLag          RelayLag       `json:"outbox_relay_lag"`
	ObservationErrors int            `json:"observation_errors,omitempty"`
}

// sampler probes every interval until stopped and keeps the probes.
type sampler struct {
	mu     sync.Mutex
	probes []probe
	errs   int
	stop   chan struct{}
	done   chan struct{}
}

func (o *observer) start(ctx context.Context, every time.Duration) *sampler {
	s := &sampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			pctx, cancel := context.WithTimeout(ctx, every)
			p, err := o.probe(pctx)
			cancel()
			s.mu.Lock()
			if err != nil {
				s.errs++
			} else {
				s.probes = append(s.probes, p)
			}
			s.mu.Unlock()
			select {
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return s
}

func (s *sampler) finish() ([]probe, int) {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probes, s.errs
}

// summarizeProbes folds the probes taken in [from, to) and the counter deltas into DBStats.
func summarizeProbes(probes []probe, errs int, from, to time.Time, before, after dbCounters, lag RelayLag) DBStats {
	d := DBStats{WaitEvents: map[string]int{}, ObservationErrors: errs, RelayLag: lag}
	var active, waiters int
	for _, p := range probes {
		if p.at.Before(from) || !p.at.Before(to) {
			continue
		}
		d.Samples++
		active += p.apiActive
		waiters += p.lockWaiters
		d.APIConnsMax = max(d.APIConnsMax, p.apiConns)
		d.APIActiveMax = max(d.APIActiveMax, p.apiActive)
		d.APIIdleInTxMax = max(d.APIIdleInTxMax, p.apiIdleTx)
		d.WorkerConnsMax = max(d.WorkerConnsMax, p.workerConns)
		d.LockWaitersMax = max(d.LockWaitersMax, p.lockWaiters)
		d.UngrantedMax = max(d.UngrantedMax, p.ungranted)
		d.LongestLockWaitS = math.Max(d.LongestLockWaitS, math.Round(p.longestLockWait*1000)/1000)
		if p.lockWaiters > 0 {
			d.LockWaitSamples++
		}
		backlog := p.outboxPending + p.outboxLeased
		d.OutboxBacklogMax = max(d.OutboxBacklogMax, backlog)
		d.OutboxBacklogEnd = backlog
		for k, v := range p.waits {
			d.WaitEvents[k] += v
		}
	}
	if d.Samples > 0 {
		d.APIActiveMean = math.Round(float64(active)/float64(d.Samples)*100) / 100
		d.LockWaitersMean = math.Round(float64(waiters)/float64(d.Samples)*100) / 100
	}
	d.Commits = after.commits - before.commits
	d.Rollbacks = after.rollbacks - before.rollbacks
	d.Deadlocks = after.deadlocks - before.deadlocks
	d.Conflicts = after.conflicts - before.conflicts
	d.TempFiles = after.tempFiles - before.tempFiles
	d.BlocksRead = after.blksRead - before.blksRead
	d.TuplesInserted = after.inserted - before.inserted
	if hits, reads := after.blksHit-before.blksHit, d.BlocksRead; hits+reads > 0 {
		d.CacheHitRatio = math.Round(float64(hits)/float64(hits+reads)*10000) / 10000
	}
	return d
}

// topWaits lists the n most frequent wait events.
func topWaits(waits map[string]int, n int) []string {
	type kv struct {
		k string
		v int
	}
	var all []kv
	for k, v := range waits {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v || (all[i].v == all[j].v && all[i].k < all[j].k) })
	var out []string
	for i := 0; i < len(all) && i < n; i++ {
		out = append(out, fmt.Sprintf("%s×%d", all[i].k, all[i].v))
	}
	return out
}
