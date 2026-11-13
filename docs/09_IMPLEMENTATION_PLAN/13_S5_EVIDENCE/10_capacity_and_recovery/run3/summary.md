# Load run run3-two-workers (stages)

- started 2026-11-12T15:10:01Z, finished 2026-11-12T15:13:33Z; tenant load.20261112T151001.f11d
- go1.26.8, linux/amd64, kernel 6.18.58.1-microsoft-standard-WSL2 (WSL true), nproc 20; PostgreSQL 16.12 on x86_64-pc-linux-musl, compiled by gcc (Alpine 15.2.0) 15.2.0, 64-bit
- host at start: 10:10:01 up 1 day,  8:59,  1 user,  load average: 4.68, 9.32, 7.46; MemAvailable 48500 MiB of 64301 MiB
- API pool: pgxpool pool_max_conns=20; settings map[archive_mode:on archive_timeout:60s checkpoint_timeout:300s default_transaction_isolation:read committed effective_cache_size:5242888kB fsync:on max_connections:100 max_wal_size:1024MB shared_buffers:163848kB synchronous_commit:on wal_level:replica work_mem:4096kB]
- setup 1.3 s (94 requests); prime 3 cycles in 1.9 s (commands P95 76.1 ms, reads P95 38.9 ms)

| stage | users cmd/read | cmd ok/s | cmd P50/P95/P99 ms | cmd err/timeouts | read ok/s | read P50/P95/P99 ms | read err/timeouts | cycles/s | cycle P95 s | API cores | conns max / active mean | lock waiters max (samples) | longest lock wait s | outbox backlog max / relay lag P95 s | load1 before→after | MemAvailable MiB |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 4/4 | 96.5 | 30.2 / 86.5 / 95.1 | 0/0 | 398.7 | 7.5 / 31.8 / 36.3 | 0/0 | 5.68 | 0.74 | 1.38 | 8 / 3.4 | 1 (1/50) | 0.007 | 99 / 0.887 | 4.71→8.49 | 48285 |
| 2 | 8/8 | 158.7 | 36.2 / 104.2 / 118.9 | 0/0 | 669.1 | 8.8 / 37.5 / 43.8 | 0/0 | 9.38 | 0.95 | 2.39 | 16 / 6.2 | 2 (5/50) | 0.011 | 359 / 7.469 | 7.89→13.69 | 48055 |
| 3 | 16/16 | 183.9 | 64.5 / 180.4 / 203.4 | 0/0 | 721.2 | 15.1 / 72.7 / 84.6 | 0/0 | 10.88 | 1.65 | 2.80 | 20 / 7.6 | 2 (10/50) | 0.039 | 937 / 9.686 | 12.83→18.84 | 47925 |

- stage 1: commits 70499, rollbacks 0, deadlocks 0, cache hit 0.9974, top waits [LWLock:WALWrite×71 IO:WALSync×41 Client:ClientRead×18 Lock:transactionid×1]
- stage 2: commits 116035, rollbacks 0, deadlocks 0, cache hit 0.9973, top waits [LWLock:WALWrite×150 Client:ClientRead×50 IO:WALSync×44 Lock:transactionid×6 LWLock:BufferContent×1]
- stage 3: commits 128591, rollbacks 0, deadlocks 0, cache hit 0.9926, top waits [LWLock:WALWrite×186 Client:ClientRead×62 IO:WALSync×39 Lock:transactionid×8 Lock:tuple×1]
