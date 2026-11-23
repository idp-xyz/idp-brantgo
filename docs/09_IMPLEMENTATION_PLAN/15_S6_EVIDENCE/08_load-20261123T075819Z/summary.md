# Load run  (stages)

- started 2026-11-23T07:58:19Z, finished 2026-11-23T07:59:34Z; tenant load.20261123T075819.4f72
- go1.26.8, linux/amd64, kernel 6.18.58.1-microsoft-standard-WSL2 (WSL true), nproc 20; PostgreSQL 16.12 on x86_64-pc-linux-musl, compiled by gcc (Alpine 15.2.0) 15.2.0, 64-bit
- host at start: 15:58:19 up 4 days,  2:38,  1 user,  load average: 11.56, 11.78, 9.43; MemAvailable 51393 MiB of 64301 MiB
- API pool: pgxpool pool_max_conns=20; settings map[archive_mode:off archive_timeout:0s checkpoint_timeout:300s default_transaction_isolation:read committed effective_cache_size:5242888kB fsync:on max_connections:300 max_wal_size:1024MB shared_buffers:163848kB synchronous_commit:on wal_level:replica work_mem:4096kB]
- setup 3.6 s (94 requests); prime 3 cycles in 5.0 s (commands P95 234.1 ms, reads P95 106.2 ms); 3 brant-worker -dispatch

| stage | users cmd/read | cmd ok/s | cmd P50/P95/P99 ms | cmd err/timeouts | read ok/s | read P50/P95/P99 ms | read err/timeouts | cycles/s | cycle P95 s | API cores | conns max / active mean | lock waiters max (samples) | longest lock wait s | outbox backlog max / relay lag P95 s | load1 before→after | MemAvailable MiB |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 16/16 | 61.4 | 203.9 / 667.6 / 825.2 | 0/0 | 240.0 | 53.8 / 232.0 / 414.4 | 0/0 | 3.74 | 5.88 | 1.15 | 20 / 13.2 | 4 (8/50) | 0.153 | 199 / 6.403 | 10.47→12.79 | 51497 |

- stage 1: commits 43532, rollbacks 0, deadlocks 0, cache hit 1.0000, top waits [LWLock:WALWrite×514 Client:ClientRead×41 IO:WALSync×41 Lock:transactionid×9 Lock:tuple×3]; relay lag P50/P95/P99/max 0.759/6.403/16.293/21.860 s over 3098 events, oldest undelivered max 21.072 s, wall clock step 0.000 s; workers' metrics: 3 workers published 3099, 94.06 % within the P95 target, 100.00 % within the P99 target, 0 failures, 0 dead letters, settlement mean 0.0216 s

Thresholds (asserted true; 3 dispatch workers; relay targets up to 16 command users, API targets up to 32): verdict fail, 1 failed

| stage | command users | target | basis | limit s | observed s | status | note |
|---|---|---|---|---|---|---|---|
| 1 | 16 | read_latency_p95 | S5-11 (C7) | 0.5 | 0.232 | pass |  |
| 1 | 16 | command_latency_p95 | S5-11 (C7) | 0.8 | 0.668 | pass |  |
| 1 | 16 | relay_latency_p95 | S5R-D07 | 5 | 6.403 | fail |  |
| 1 | 16 | relay_latency_p99 | S5R-D07 | 30 | 16.293 | pass |  |
| 1 | 16 | oldest_undelivered_below_alert | S5R-D07 | 60 | 21.072 | pass |  |

Outbox drain after the last stage: 4.0 s, 200 events published (50.0/s), 0 left, complete true
