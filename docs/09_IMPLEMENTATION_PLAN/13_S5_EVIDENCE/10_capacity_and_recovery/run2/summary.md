# Load run run2 (stages)

- started 2026-11-12T14:58:51Z, finished 2026-11-12T15:09:44Z; tenant load.20261112T145851.8155
- go1.26.8, linux/amd64, kernel 6.18.58.1-microsoft-standard-WSL2 (WSL true), nproc 20; PostgreSQL 16.12 on x86_64-pc-linux-musl, compiled by gcc (Alpine 15.2.0) 15.2.0, 64-bit
- host at start: 09:58:51 up 1 day,  8:49,  1 user,  load average: 1.97, 3.06, 3.90; MemAvailable 48602 MiB of 64301 MiB
- API pool: pgxpool pool_max_conns=20; settings map[archive_mode:on archive_timeout:60s checkpoint_timeout:300s default_transaction_isolation:read committed effective_cache_size:5242888kB fsync:on max_connections:100 max_wal_size:1024MB shared_buffers:163848kB synchronous_commit:on wal_level:replica work_mem:4096kB]
- setup 1.2 s (94 requests); prime 3 cycles in 1.7 s (commands P95 70.6 ms, reads P95 30.7 ms)

| stage | users cmd/read | cmd ok/s | cmd P50/P95/P99 ms | cmd err/timeouts | read ok/s | read P50/P95/P99 ms | read err/timeouts | cycles/s | cycle P95 s | API cores | conns max / active mean | lock waiters max (samples) | longest lock wait s | outbox backlog max / relay lag P95 s | load1 before→after | MemAvailable MiB |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 1/1 | 29.5 | 23.4 / 67.9 / 96.8 | 0/0 | 123.8 | 5.9 / 24.2 / 32.1 | 0/0 | 1.74 | 0.74 | 0.43 | 2 / 0.7 | 0 (0/50) | 0.000 | 31 / 0.989 | 1.82→6.43 | 48273 |
| 2 | 2/2 | 52.1 | 27.1 / 79.2 / 100.9 | 0/0 | 214.2 | 6.6 / 28.7 / 37.1 | 0/0 | 3.04 | 0.82 | 0.72 | 4 / 1.8 | 0 (0/50) | 0.000 | 53 / 0.996 | 6.40→6.82 | 47321 |
| 3 | 4/4 | 95.4 | 30.3 / 87.3 / 97.8 | 0/0 | 395.3 | 7.5 / 32.1 / 36.4 | 0/0 | 5.62 | 0.77 | 1.34 | 8 / 3.5 | 2 (2/50) | 0.011 | 96 / 1.014 | 6.43→10.12 | 48363 |
| 4 | 8/8 | 160.4 | 35.7 / 103.6 / 116.5 | 0/0 | 669.2 | 8.8 / 37.6 / 43.9 | 0/0 | 9.40 | 0.92 | 2.39 | 16 / 6.1 | 1 (3/50) | 0.009 | 3533 / 20.854 | 9.63→12.34 | 48080 |
| 5 | 16/16 | 188.0 | 63.6 / 176.0 / 192.5 | 0/0 | 731.9 | 14.9 / 72.2 / 82.5 | 0/0 | 11.18 | 1.56 | 2.87 | 20 / 7.8 | 2 (6/50) | 0.047 | 8455 / 47.086 | 12.40→17.42 | 47704 |
| 6 | 32/32 | 190.4 | 129.4 / 347.3 / 386.7 | 0/0 | 700.5 | 28.9 / 161.0 / 188.0 | 0/0 | 11.10 | 3.02 | 2.87 | 20 / 7.8 | 2 (12/50) | 0.049 | 13651 / 82.992 | 16.27→22.59 | 47865 |
| 7 | 64/64 | 172.4 | 266.0 / 799.7 / 1173.7 | 0/0 | 624.0 | 56.3 / 348.4 / 588.8 | 0/0 | 10.24 | 6.99 | 2.60 | 20 / 9.7 | 19 (19/50) | 4.812 | 17967 / 113.812 | 20.86→18.94 | 47712 |

- stage 1: commits 22174, rollbacks 0, deadlocks 0, cache hit 0.9992, top waits [IO:WALSync×20 LWLock:WALWrite×4 Client:ClientRead×2]
- stage 2: commits 38370, rollbacks 0, deadlocks 0, cache hit 0.9920, top waits [LWLock:WALWrite×32 IO:WALSync×24 Client:ClientRead×8]
- stage 3: commits 70166, rollbacks 0, deadlocks 0, cache hit 0.9960, top waits [LWLock:WALWrite×81 IO:WALSync×40 Client:ClientRead×11 Lock:transactionid×3 IO:WALWrite×2]
- stage 4: commits 113506, rollbacks 0, deadlocks 0, cache hit 0.9960, top waits [LWLock:WALWrite×166 IO:WALSync×45 Client:ClientRead×34 Lock:transactionid×1]
- stage 5: commits 127953, rollbacks 0, deadlocks 0, cache hit 0.9927, top waits [LWLock:WALWrite×218 Client:ClientRead×45 IO:WALSync×42 Lock:transactionid×7 IO:WALWrite×4]
- stage 6: commits 123847, rollbacks 0, deadlocks 0, cache hit 0.9909, top waits [LWLock:WALWrite×198 IO:WALSync×45 Client:ClientRead×42 Lock:transactionid×16 LWLock:BufferContent×1 Lock:tuple×1]
- stage 7: commits 110341, rollbacks 0, deadlocks 0, cache hit 0.9646, top waits [LWLock:WALWrite×167 Lock:tuple×126 Client:ClientRead×49 IO:WALSync×38 Lock:transactionid×22 IO:WALWrite×1]
Outbox drain after the last stage: 145.1 s, 19244 events published (132.6/s), 0 left, complete true
