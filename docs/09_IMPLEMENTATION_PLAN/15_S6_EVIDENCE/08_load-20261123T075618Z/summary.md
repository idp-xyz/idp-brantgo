# Load run  (stages)

- started 2026-11-23T07:56:18Z, finished 2026-11-23T07:57:51Z; tenant load.20261123T075618.c768
- go1.26.8, linux/amd64, kernel 6.18.58.1-microsoft-standard-WSL2 (WSL true), nproc 20; PostgreSQL 16.12 on x86_64-pc-linux-musl, compiled by gcc (Alpine 15.2.0) 15.2.0, 64-bit
- host at start: 15:56:18 up 4 days,  2:36,  1 user,  load average: 13.68, 10.35, 8.62; MemAvailable 51029 MiB of 64301 MiB
- API pool: pgxpool pool_max_conns=20; settings map[archive_mode:off archive_timeout:0s checkpoint_timeout:300s default_transaction_isolation:read committed effective_cache_size:5242888kB fsync:on max_connections:300 max_wal_size:1024MB shared_buffers:163848kB synchronous_commit:on wal_level:replica work_mem:4096kB]
- setup 1.6 s (94 requests); prime 3 cycles in 2.4 s (commands P95 93.0 ms, reads P95 44.8 ms); 2 brant-worker -dispatch

| stage | users cmd/read | cmd ok/s | cmd P50/P95/P99 ms | cmd err/timeouts | read ok/s | read P50/P95/P99 ms | read err/timeouts | cycles/s | cycle P95 s | API cores | conns max / active mean | lock waiters max (samples) | longest lock wait s | outbox backlog max / relay lag P95 s | load1 before→after | MemAvailable MiB |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 16/16 | 103.0 | 118.9 / 328.3 / 528.3 | 0/0 | 436.5 | 24.7 / 115.1 / 175.1 | 0/0 | 6.08 | 3.97 | 1.81 | 20 / 6.9 | 4 (9/50) | 0.051 | 1299 / 13.011 | 12.98→21.60 | 51259 |

- stage 1: commits 76522, rollbacks 0, deadlocks 0, cache hit 1.0000, top waits [LWLock:WALWrite×144 Client:ClientRead×47 IO:WALSync×40 Lock:transactionid×10 Lock:tuple×3 IO:WALInitWrite×1]; relay lag P50/P95/P99/max 7.593/13.011/24.744/29.343 s over 4260 events, oldest undelivered max 29.014 s, wall clock step 0.000 s; workers' metrics: 2 workers published 4261, 35.02 % within the P95 target, 100.00 % within the P99 target, 0 failures, 0 dead letters, settlement mean 0.0087 s

Thresholds (asserted true; 2 dispatch workers; relay targets up to 16 command users, API targets up to 32): verdict fail, 1 failed

| stage | command users | target | basis | limit s | observed s | status | note |
|---|---|---|---|---|---|---|---|
| 1 | 16 | read_latency_p95 | S5-11 (C7) | 0.5 | 0.115 | pass |  |
| 1 | 16 | command_latency_p95 | S5-11 (C7) | 0.8 | 0.328 | pass |  |
| 1 | 16 | relay_latency_p95 | S5R-D07 | 5 | 13.011 | fail |  |
| 1 | 16 | relay_latency_p99 | S5R-D07 | 30 | 24.744 | pass |  |
| 1 | 16 | oldest_undelivered_below_alert | S5R-D07 | 60 | 29.014 | pass |  |

Outbox drain after the last stage: 28.1 s, 1460 events published (52.0/s), 0 left, complete true
