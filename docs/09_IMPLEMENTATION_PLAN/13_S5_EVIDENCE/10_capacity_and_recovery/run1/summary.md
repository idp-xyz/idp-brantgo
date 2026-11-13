# Load run run1 (stages)

- started 2026-11-12T13:49:33Z, finished 2026-11-12T13:57:46Z; tenant load.20261112T134933.4bc8
- go1.26.8, linux/amd64, kernel 6.18.58.1-microsoft-standard-WSL2 (WSL true), nproc 20; PostgreSQL 16.12 on x86_64-pc-linux-musl, compiled by gcc (Alpine 15.2.0) 15.2.0, 64-bit
- host at start: 08:49:34 up 1 day,  7:45,  1 user,  load average: 1.97, 4.45, 6.43; MemAvailable 48694 MiB of 64301 MiB
- API pool: pgxpool pool_max_conns=20; settings map[archive_mode:on archive_timeout:60s checkpoint_timeout:300s default_transaction_isolation:read committed effective_cache_size:5242888kB fsync:on max_connections:100 max_wal_size:1024MB shared_buffers:163848kB synchronous_commit:on wal_level:replica work_mem:4096kB]
- setup 1.2 s (94 requests); prime 3 cycles in 1.6 s (commands P95 65.3 ms, reads P95 30.4 ms)

| stage | users cmd/read | cmd ok/s | cmd P50/P95/P99 ms | cmd err/timeouts | read ok/s | read P50/P95/P99 ms | read err/timeouts | cycles/s | cycle P95 s | API cores | conns max / active mean | lock waiters max (samples) | longest lock wait s | outbox backlog max / relay lag P95 s | load1 before→after | MemAvailable MiB |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 1/1 | 30.5 | 23.4 / 68.4 / 77.8 | 0/0 | 127.5 | 6.0 / 24.2 / 29.6 | 0/0 | 1.78 | 0.63 | 0.45 | 2 / 0.8 | 0 (0/50) | 0.000 | 30 / 0.995 | 1.97→3.02 | 48822 |
| 2 | 2/2 | 56.3 | 25.8 / 74.4 / 81.3 | 0/0 | 233.5 | 6.4 / 27.0 / 31.5 | 0/0 | 3.32 | 0.65 | 0.78 | 4 / 1.6 | 1 (1/50) | 0.004 | 59 / 0.999 | 2.94→4.96 | 48766 |
| 3 | 4/4 | 99.9 | 28.9 / 83.5 / 92.8 | 0/0 | 413.9 | 7.3 / 30.4 / 34.9 | 0/0 | 5.90 | 0.72 | 1.40 | 8 / 3.8 | 1 (2/50) | 0.004 | 112 / 1.025 | 4.72→7.59 | 48626 |
| 4 | 8/8 | 160.3 | 35.4 / 102.3 / 117.3 | 0/0 | 674.2 | 8.8 / 37.1 / 45.0 | 0/0 | 9.44 | 0.96 | 2.45 | 16 / 5.9 | 1 (4/50) | 0.013 | 3397 / 20.339 | 7.06→15.84 | 48172 |
| 5 | 16/16 | 192.3 | 62.2 / 172.1 / 188.6 | 0/0 | 751.1 | 14.5 / 70.0 / 81.2 | 0/0 | 11.24 | 1.57 | 2.87 | 20 / 7.6 | 2 (8/50) | 0.017 | 8294 / 45.467 | 14.98→20.30 | 48064 |
| 6 | 32/32 | 182.8 | 135.6 / 363.3 / 420.0 | 0/0 | 672.6 | 29.8 / 165.3 / 200.3 | 0/0 | 10.54 | 3.55 | 2.81 | 20 / 8.1 | 4 (10/50) | 0.124 | 13350 / 82.386 | 19.00→19.86 | 45034 |
| 7 | 64/64 | 167.2 | 285.8 / 833.8 / 1054.4 | 0/0 | 601.7 | 60.2 / 368.2 / 569.0 | 0/0 | 9.38 | 7.36 | 2.53 | 20 / 9.2 | 15 (14/50) | 0.492 | 17531 / 117.043 | 18.91→22.58 | 44178 |

- stage 1: commits 22854, rollbacks 0, deadlocks 0, cache hit 1.0000, top waits [IO:WALSync×24 LWLock:WALWrite×5 Client:ClientRead×4]
- stage 2: commits 41695, rollbacks 0, deadlocks 0, cache hit 1.0000, top waits [IO:WALSync×34 LWLock:WALWrite×24 Client:ClientRead×6]
- stage 3: commits 73726, rollbacks 0, deadlocks 0, cache hit 1.0000, top waits [LWLock:WALWrite×83 IO:WALSync×45 Client:ClientRead×16 Lock:transactionid×2]
- stage 4: commits 113621, rollbacks 0, deadlocks 0, cache hit 1.0000, top waits [LWLock:WALWrite×142 IO:WALSync×45 Client:ClientRead×35 Lock:transactionid×4 IO:WALWrite×1]
- stage 5: commits 129127, rollbacks 0, deadlocks 0, cache hit 0.9997, top waits [LWLock:WALWrite×189 Client:ClientRead×62 IO:WALSync×44 Lock:transactionid×6 LWLock:LockManager×1]
- stage 6: commits 119420, rollbacks 0, deadlocks 0, cache hit 0.9960, top waits [LWLock:WALWrite×188 Client:ClientRead×52 IO:WALSync×47 Lock:transactionid×11 Lock:tuple×7 LWLock:BufferContent×1]
- stage 7: commits 107090, rollbacks 0, deadlocks 0, cache hit 0.9933, top waits [LWLock:WALWrite×163 Lock:tuple×88 Client:ClientRead×62 IO:WALSync×44 Lock:transactionid×17 LWLock:LockManager×1]
