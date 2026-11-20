# S4 lead gates per merge commit

Source: the lead's gate logs `.claude/handover/scratchpad/lead/logs/<tag>-<step>.log` and `<tag>-gate.out`, written by the lead's
gate scripts in the lead's checkout (`/home/tops/workspace/idp-brantgo`, branch s4/tasks-ai) against the lead's PostgreSQL 16.12
container `brant-pg16` (127.0.0.1:55471, `max_connections` 300 from 2026-11-13) and, for the scripted evaluation suite, against `brant-pg16-ci100`
(127.0.0.1:56310, `max_connections` 100 as CI's server, from 2026-11-17), WSL2 Ubuntu-24.04, CI=true. db = `scripts/dev-postgres.sh`
(migrate and grants), integration = `make integration` and race = `make race`, both sharded by `scripts/test-shards.sh` with SHARDS=6
(each bootstrap test runs in exactly one shard; the log says how many tests and that each ran once). The logs hold no exit status of
their own; the results below are read from the log text, the end time is the log file's modification time (UTC). The commit is the
merge commit the lead had checked out (s4-lead-notes.md and the first-parent history). Generated on 2026-11-20T03:34Z by E2's gates.py.

| gate tag | commit | merged units | step | result | end (UTC) |
|---|---|---|---|---|---|
| s4-after-a2 | 37c4667 | A2 (S4-01b) | db | 1 applied, 65 recorded | 2026-11-13T08:10Z |
| s4-after-a2 | 37c4667 | A2 (S4-01b) | integration | others 151 pkgs brant_shard_1 exit 0 0m27s bin/shards/integration/others.log; 6 shards: all 604 tests passed, each exactly once, in its own shard (104 + 108 + 95 + 93 + 95 + 109); wall 5m32s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-13T08:15Z |
| s4-after-a2 | 37c4667 | A2 (S4-01b) | race | others 151 pkgs brant_shard_1 exit 0 0m38s bin/shards/race/others.log; 6 shards: all 604 tests passed, each exactly once, in its own shard (104 + 108 + 95 + 93 + 95 + 109); wall 4m53s (databases included); test-shards: race SHARDS=6 passed | 2026-11-13T08:20Z |
| s4-after-a1a5 | b671767 | A1 a365e7c (S4-01a), A5 b671767 (S4-03a) | db | 2 applied, 66 recorded | 2026-11-13T08:22Z |
| s4-after-a1a5 | b671767 | A1 a365e7c (S4-01a), A5 b671767 (S4-03a) | integration | others 164 pkgs brant_shard_1 exit 0 0m20s bin/shards/integration/others.log; 6 shards: all 609 tests passed, each exactly once, in its own shard (104 + 108 + 97 + 94 + 95 + 111); wall 3m49s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-13T08:26Z |
| s4-after-a1a5 | b671767 | A1 a365e7c (S4-01a), A5 b671767 (S4-03a) | race | others 164 pkgs brant_shard_1 exit 0 0m30s bin/shards/race/others.log; 6 shards: all 609 tests passed, each exactly once, in its own shard (104 + 108 + 97 + 94 + 95 + 111); wall 4m32s (databases included); test-shards: race SHARDS=6 passed | 2026-11-13T08:30Z |
| s4-after-a3 | 141929f | A3 (S4-01c) | db | 0 applied, 68 recorded | 2026-11-13T09:00Z |
| s4-after-a3 | 141929f | A3 (S4-01c) | integration | others 164 pkgs brant_shard_1 exit 0 0m24s bin/shards/integration/others.log; 6 shards: all 618 tests passed, each exactly once, in its own shard (104 + 110 + 100 + 96 + 96 + 112); wall 4m31s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-13T09:04Z |
| s4-after-a3 | 141929f | A3 (S4-01c) | race | others 164 pkgs brant_shard_1 exit 0 1m06s bin/shards/race/others.log; 6 shards: all 618 tests passed, each exactly once, in its own shard (104 + 110 + 100 + 96 + 96 + 112); wall 6m04s (databases included); test-shards: race SHARDS=6 passed | 2026-11-13T09:10Z |
| s4-after-r0 | 90925f3 | R0 (the 47 S5R decisions in the design sources) | db | 0 applied, 68 recorded | 2026-11-13T09:11Z |
| s4-after-r0 | 90925f3 | R0 (the 47 S5R decisions in the design sources) | integration | others 164 pkgs brant_shard_1 exit 0 0m20s bin/shards/integration/others.log; 6 shards: all 618 tests passed, each exactly once, in its own shard (104 + 110 + 100 + 96 + 96 + 112); wall 4m25s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-13T09:16Z |
| s4-after-r0 | 90925f3 | R0 (the 47 S5R decisions in the design sources) | race | others 164 pkgs brant_shard_1 exit 0 0m34s bin/shards/race/others.log; 6 shards: all 618 tests passed, each exactly once, in its own shard (104 + 110 + 100 + 96 + 96 + 112); wall 5m38s (databases included); test-shards: race SHARDS=6 passed | 2026-11-13T09:21Z |
| s4-after-b3 | 1cd8e9e | A6 04baa4d (S4-04a), B3 1cd8e9e (S4-06b); the lead's 4e00b2b followed it | db | 1 applied, 68 recorded | 2026-11-13T09:38Z |
| s4-after-b3 | 1cd8e9e | A6 04baa4d (S4-04a), B3 1cd8e9e (S4-06b); the lead's 4e00b2b followed it | integration | others 170 pkgs brant_shard_1 exit 0 0m29s bin/shards/integration/others.log; testshard: ./internal/bootstrap: verify failed:; 1 failed: TestGoalReadinessKeepsATimelyContractAfterTheQuoteExpired; wall 4m50s (databases included); test-shards: integration SHARDS=6 FAILED | 2026-11-13T09:43Z |
| s4-after-b3 | 1cd8e9e | A6 04baa4d (S4-04a), B3 1cd8e9e (S4-06b); the lead's 4e00b2b followed it | race | others 170 pkgs brant_shard_1 exit 0 0m41s bin/shards/race/others.log; 6 shards: all 621 tests passed, each exactly once, in its own shard (104 + 111 + 101 + 96 + 97 + 112); wall 7m03s (databases included); test-shards: race SHARDS=6 passed | 2026-11-13T09:50Z |
| s4-after-b3 | 1cd8e9e | A6 04baa4d (S4-04a), B3 1cd8e9e (S4-06b); the lead's 4e00b2b followed it | gate summary (`s4-after-b3-gate.out`) | overall=1 | |
| s4-after-b2 | 47b51fc | B1 a74796d (S4-02), B2 47b51fc (S4-01d) | db | 2 applied, 69 recorded | 2026-11-13T09:55Z |
| s4-after-b2 | 47b51fc | B1 a74796d (S4-02), B2 47b51fc (S4-01d) | integration | others 172 pkgs brant_shard_1 exit 0 0m23s bin/shards/integration/others.log; testshard: ./internal/bootstrap: verify failed:; 1 failed: TestGoalVerifyOfABoundTaskFollowsTRDsControl; wall 5m11s (databases included); test-shards: integration SHARDS=6 FAILED | 2026-11-13T10:00Z |
| s4-after-b2 | 47b51fc | B1 a74796d (S4-02), B2 47b51fc (S4-01d) | race | others 172 pkgs brant_shard_1 exit 0 0m35s bin/shards/race/others.log; testshard: ./internal/bootstrap: verify failed:; 1 failed: TestGoalVerifyOfABoundTaskFollowsTRDsControl; wall 4m25s (databases included); test-shards: race SHARDS=6 FAILED | 2026-11-13T10:05Z |
| s4-after-b2 | 47b51fc | B1 a74796d (S4-02), B2 47b51fc (S4-01d) | gate summary (`s4-after-b2-gate.out`) | overall=1 | |
| s4-after-fix | dc045fe | the lead's test fix dc045fe after the B2 failure | db | 0 applied, 71 recorded | 2026-11-13T11:12Z |
| s4-after-fix | dc045fe | the lead's test fix dc045fe after the B2 failure | integration | others 172 pkgs brant_shard_1 exit 0 0m27s bin/shards/integration/others.log; 6 shards: all 629 tests passed, each exactly once, in its own shard (106 + 111 + 102 + 97 + 98 + 115); wall 4m41s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-13T11:17Z |
| s4-after-fix | dc045fe | the lead's test fix dc045fe after the B2 failure | race | others 172 pkgs brant_shard_1 exit 0 0m28s bin/shards/race/others.log; 6 shards: all 629 tests passed, each exactly once, in its own shard (106 + 111 + 102 + 97 + 98 + 115); wall 6m00s (databases included); test-shards: race SHARDS=6 passed | 2026-11-13T11:23Z |
| s4-after-r2 | a84a10a | R2 | db | 4 applied, 71 recorded | 2026-11-13T11:23Z |
| s4-after-r2 | a84a10a | R2 | integration | others 172 pkgs brant_shard_1 exit 0 0m23s bin/shards/integration/others.log; 6 shards: all 636 tests passed, each exactly once, in its own shard (108 + 113 + 103 + 97 + 98 + 117); wall 6m31s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-13T11:30Z |
| s4-after-r2 | a84a10a | R2 | race | others 172 pkgs brant_shard_1 exit 0 1m16s bin/shards/race/others.log; 6 shards: all 636 tests passed, each exactly once, in its own shard (108 + 113 + 103 + 97 + 98 + 117); wall 11m14s (databases included); test-shards: race SHARDS=6 passed | 2026-11-13T11:41Z |
| s4-after-r2 | a84a10a | R2 | gate summary (`s4-after-r2-gate.out`) | overall=0 | |
| s4-after-r4 | 6ff5ed6 | R5 803727a, R11 4ff4821, R4 6ff5ed6 | db | 3 applied, 75 recorded | 2026-11-13T11:46Z |
| s4-after-r4 | 6ff5ed6 | R5 803727a, R11 4ff4821, R4 6ff5ed6 | integration | others 174 pkgs brant_shard_1 exit 0 0m31s bin/shards/integration/others.log; 6 shards: all 645 tests passed, each exactly once, in its own shard (110 + 115 + 103 + 99 + 100 + 118); wall 6m07s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-13T11:52Z |
| s4-after-r4 | 6ff5ed6 | R5 803727a, R11 4ff4821, R4 6ff5ed6 | race | others 174 pkgs brant_shard_1 exit 0 0m47s bin/shards/race/others.log; 6 shards: all 645 tests passed, each exactly once, in its own shard (110 + 115 + 103 + 99 + 100 + 118); wall 6m32s (databases included); test-shards: race SHARDS=6 passed | 2026-11-13T11:58Z |
| s4-after-r4 | 6ff5ed6 | R5 803727a, R11 4ff4821, R4 6ff5ed6 | gate summary (`s4-after-r4-gate.out`) | overall=0 | |
| s4-after-r7 | cb36096 | C1 c1c0e04, B4 0c8ad9c, R1 cb86467, R7 cb36096 | db | 4 applied, 78 recorded | 2026-11-13T12:44Z |
| s4-after-r7 | cb36096 | C1 c1c0e04, B4 0c8ad9c, R1 cb86467, R7 cb36096 | integration | others 175 pkgs brant_shard_1 exit 0 0m36s bin/shards/integration/others.log; 6 shards: all 668 tests passed, each exactly once, in its own shard (115 + 121 + 106 + 101 + 103 + 122); wall 6m05s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-13T12:50Z |
| s4-after-r7 | cb36096 | C1 c1c0e04, B4 0c8ad9c, R1 cb86467, R7 cb36096 | race | others 175 pkgs brant_shard_1 exit 0 0m40s bin/shards/race/others.log; 6 shards: all 668 tests passed, each exactly once, in its own shard (115 + 121 + 106 + 101 + 103 + 122); wall 5m22s (databases included); test-shards: race SHARDS=6 passed | 2026-11-13T12:56Z |
| s4-after-r7 | cb36096 | C1 c1c0e04, B4 0c8ad9c, R1 cb86467, R7 cb36096 | gate summary (`s4-after-r7-gate.out`) | overall=0 | |
| s4-after-c2 | 8279edd | R9 357fa71, R3 1f42d3b, C2 8279edd | db | 2 applied, 82 recorded | 2026-11-13T12:59Z |
| s4-after-c2 | 8279edd | R9 357fa71, R3 1f42d3b, C2 8279edd | integration | others 178 pkgs brant_shard_1 exit 0 0m24s bin/shards/integration/others.log; 6 shards: all 689 tests passed, each exactly once, in its own shard (117 + 124 + 111 + 102 + 109 + 126); wall 4m23s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-13T13:03Z |
| s4-after-c2 | 8279edd | R9 357fa71, R3 1f42d3b, C2 8279edd | race | others 178 pkgs brant_shard_1 exit 0 0m37s bin/shards/race/others.log; 6 shards: all 689 tests passed, each exactly once, in its own shard (117 + 124 + 111 + 102 + 109 + 126); wall 5m50s (databases included); test-shards: race SHARDS=6 passed | 2026-11-13T13:09Z |
| s4-after-c2 | 8279edd | R9 357fa71, R3 1f42d3b, C2 8279edd | gate summary (`s4-after-c2-gate.out`) | overall=0 | |
| s4-after-r10 | c3d024e | R10 (database and race only; the lead notes take the integration step from R10's own run on the same tree) | db | 1 applied, 84 recorded | 2026-11-13T13:18Z |
| s4-after-r10 | c3d024e | R10 (database and race only; the lead notes take the integration step from R10's own run on the same tree) | race | others 178 pkgs brant_shard_1 exit 0 0m39s bin/shards/race/others.log; 6 shards: all 697 tests passed, each exactly once, in its own shard (119 + 126 + 112 + 102 + 109 + 129); wall 6m03s (databases included); test-shards: race SHARDS=6 passed | 2026-11-13T13:24Z |
| s4-after-r10 | c3d024e | R10 (database and race only; the lead notes take the integration step from R10's own run on the same tree) | gate summary (`s4-after-r10-gate.out`) | overall=0 | |
| s4-after-c4 | 0cf5920 | C4 | db | 1 applied, 85 recorded | 2026-11-13T13:26Z |
| s4-after-c4 | 0cf5920 | C4 | integration | others 179 pkgs brant_shard_1 exit 0 0m25s bin/shards/integration/others.log; 6 shards: all 700 tests passed, each exactly once, in its own shard (120 + 126 + 112 + 102 + 110 + 130); wall 5m17s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-13T13:31Z |
| s4-after-c4 | 0cf5920 | C4 | race | others 179 pkgs brant_shard_1 exit 0 0m47s bin/shards/race/others.log; 6 shards: all 700 tests passed, each exactly once, in its own shard (120 + 126 + 112 + 102 + 110 + 130); wall 7m49s (databases included); test-shards: race SHARDS=6 passed | 2026-11-13T13:39Z |
| s4-after-c4 | 0cf5920 | C4 | gate summary (`s4-after-c4-gate.out`) | overall=0 | |
| s4-after-r6 | a29d087 | C3 bb5d850, R6 a29d087 (result recorded in the lead notes only after the restart of 2026-11-17) | db | 1 applied, 86 recorded | 2026-11-13T13:53Z |
| s4-after-r6 | a29d087 | C3 bb5d850, R6 a29d087 (result recorded in the lead notes only after the restart of 2026-11-17) | integration | others 179 pkgs brant_shard_1 exit 0 0m31s bin/shards/integration/others.log; 6 shards: all 710 tests passed, each exactly once, in its own shard (122 + 128 + 114 + 104 + 112 + 130); wall 5m28s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-13T13:58Z |
| s4-after-r6 | a29d087 | C3 bb5d850, R6 a29d087 (result recorded in the lead notes only after the restart of 2026-11-17) | race | others 179 pkgs brant_shard_1 exit 0 0m39s bin/shards/race/others.log; testshard: ./internal/bootstrap: verify failed:; 1 failed: TestS5RD18TheConcessionShowsOnTheShipmentAndItsDocuments; wall 7m12s (databases included); test-shards: race SHARDS=6 FAILED | 2026-11-13T14:06Z |
| s4-after-r6 | a29d087 | C3 bb5d850, R6 a29d087 (result recorded in the lead notes only after the restart of 2026-11-17) | gate summary (`s4-after-r6-gate.out`) | overall=1 | |
| s4-after-d2 | 8e01e00 | R6 test fix 1d7ad7e, R8 48699f5, F1 6148691, D2 8e01e00 | db | 1 applied, 87 recorded | 2026-11-17T09:07Z |
| s4-after-d2 | 8e01e00 | R6 test fix 1d7ad7e, R8 48699f5, F1 6148691, D2 8e01e00 | integration | others 186 pkgs brant_shard_1 exit 0 0m25s bin/shards/integration/others.log; 6 shards: all 722 tests passed, each exactly once, in its own shard (124 + 130 + 117 + 104 + 114 + 133); wall 4m32s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-17T09:11Z |
| s4-after-d2 | 8e01e00 | R6 test fix 1d7ad7e, R8 48699f5, F1 6148691, D2 8e01e00 | race | others 186 pkgs brant_shard_1 exit 0 0m36s bin/shards/race/others.log; 6 shards: all 722 tests passed, each exactly once, in its own shard (124 + 130 + 117 + 104 + 114 + 133); wall 5m20s (databases included); test-shards: race SHARDS=6 passed | 2026-11-17T09:17Z |
| s4-after-pools | d89b2c1 | the lead's harness fix d89b2c1 (integration and race only) | integration | others 186 pkgs brant_shard_1 exit 0 0m27s bin/shards/integration/others.log; 6 shards: all 722 tests passed, each exactly once, in its own shard (124 + 130 + 117 + 104 + 114 + 133); wall 4m36s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-17T09:29Z |
| s4-after-pools | d89b2c1 | the lead's harness fix d89b2c1 (integration and race only) | race | others 186 pkgs brant_shard_1 exit 0 0m32s bin/shards/race/others.log; 6 shards: all 722 tests passed, each exactly once, in its own shard (124 + 130 + 117 + 104 + 114 + 133); wall 5m15s (databases included); test-shards: race SHARDS=6 passed | 2026-11-17T09:34Z |
| s4-after-r13 | 82c49f6 | R13 | db | 1 applied, 88 recorded | 2026-11-17T10:03Z |
| s4-after-r13 | 82c49f6 | R13 | integration | others 186 pkgs brant_shard_1 exit 0 0m24s bin/shards/integration/others.log; 6 shards: all 728 tests passed, each exactly once, in its own shard (124 + 130 + 119 + 105 + 115 + 135); wall 5m35s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-17T10:09Z |
| s4-after-r13 | 82c49f6 | R13 | race | others 186 pkgs brant_shard_1 exit 0 0m40s bin/shards/race/others.log; 6 shards: all 728 tests passed, each exactly once, in its own shard (124 + 130 + 119 + 105 + 115 + 135); wall 5m50s (databases included); test-shards: race SHARDS=6 passed | 2026-11-17T10:14Z |
| s4-after-d1 | 41fbd71 | D1 (S4-07a) | db | 0 applied, 89 recorded | 2026-11-17T10:24Z |
| s4-after-d1 | 41fbd71 | D1 (S4-07a) | integration | others 187 pkgs brant_shard_1 exit 0 0m22s bin/shards/integration/others.log; 6 shards: all 728 tests passed, each exactly once, in its own shard (124 + 130 + 119 + 105 + 115 + 135); wall 4m29s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-17T10:29Z |
| s4-after-d1 | 41fbd71 | D1 (S4-07a) | race | others 187 pkgs brant_shard_1 exit 0 0m30s bin/shards/race/others.log; 6 shards: all 728 tests passed, each exactly once, in its own shard (124 + 130 + 119 + 105 + 115 + 135); wall 5m18s (databases included); test-shards: race SHARDS=6 passed | 2026-11-17T10:34Z |
| s4-after-r12 | d3a8fca | R12 (S5R-12) | db | 0 applied, 89 recorded | 2026-11-17T11:53Z |
| s4-after-r12 | d3a8fca | R12 (S5R-12) | integration | others 187 pkgs brant_shard_1 exit 0 0m27s bin/shards/integration/others.log; 6 shards: all 736 tests passed, each exactly once, in its own shard (124 + 131 + 122 + 105 + 118 + 136); wall 5m37s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-17T11:58Z |
| s4-after-r12 | d3a8fca | R12 (S5R-12) | race | others 187 pkgs brant_shard_1 exit 0 0m37s bin/shards/race/others.log; 6 shards: all 736 tests passed, each exactly once, in its own shard (124 + 131 + 122 + 105 + 118 + 136); wall 6m00s (databases included); test-shards: race SHARDS=6 passed | 2026-11-17T12:04Z |
| s4-after-g1 | e7ae60d | G1 | db | 1 applied, 89 recorded | 2026-11-17T12:16Z |
| s4-after-g1 | e7ae60d | G1 | integration | others 187 pkgs brant_shard_1 exit 0 0m26s bin/shards/integration/others.log; 6 shards: all 738 tests passed, each exactly once, in its own shard (124 + 131 + 122 + 106 + 119 + 136); wall 5m01s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-17T12:21Z |
| s4-after-g1 | e7ae60d | G1 | race | others 187 pkgs brant_shard_1 exit 0 0m43s bin/shards/race/others.log; 6 shards: all 738 tests passed, each exactly once, in its own shard (124 + 131 + 122 + 106 + 119 + 136); wall 6m05s (databases included); test-shards: race SHARDS=6 passed | 2026-11-17T12:27Z |
| s4-after-g2 | 0337f55 | G2 - VOID: the lead edited the tree while the gate ran (lead notes, 2026-11-18) | db | 1 applied, 90 recorded | 2026-11-18T04:10Z |
| s4-after-g2 | 0337f55 | G2 - VOID: the lead edited the tree while the gate ran (lead notes, 2026-11-18) | integration | others 187 pkgs brant_shard_1 exit 0 0m33s bin/shards/integration/others.log; 6 shards: all 741 tests passed, each exactly once, in its own shard (124 + 133 + 122 + 106 + 119 + 137); wall 8m36s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-18T04:19Z |
| s4-after-g2 | 0337f55 | G2 - VOID: the lead edited the tree while the gate ran (lead notes, 2026-11-18) | race | others 187 pkgs brant_shard_1 exit 1 0m37s bin/shards/race/others.log; 6 shards: all 741 tests passed, each exactly once, in its own shard (124 + 133 + 122 + 106 + 119 + 137); wall 6m06s (databases included); test-shards: race SHARDS=6 FAILED | 2026-11-18T04:25Z |
| s4-after-g2-fix | 3185097 | G2 with the lead's fix 3185097 | db | 0 applied, 91 recorded | 2026-11-18T04:53Z |
| s4-after-g2-fix | 3185097 | G2 with the lead's fix 3185097 | integration | others 187 pkgs brant_shard_1 exit 0 0m26s bin/shards/integration/others.log; 6 shards: all 741 tests passed, each exactly once, in its own shard (124 + 133 + 122 + 106 + 119 + 137); wall 5m09s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-18T04:58Z |
| s4-after-g2-fix | 3185097 | G2 with the lead's fix 3185097 | race | others 187 pkgs brant_shard_1 exit 0 0m36s bin/shards/race/others.log; 6 shards: all 741 tests passed, each exactly once, in its own shard (124 + 133 + 122 + 106 + 119 + 137); wall 5m57s (databases included); test-shards: race SHARDS=6 passed | 2026-11-18T05:04Z |
| g4a-merged | 13a8f8b | G4a (database, integration and race by the lead) | db | 1 applied, 91 recorded | 2026-11-18T08:05Z |
| g4a-merged | 13a8f8b | G4a (database, integration and race by the lead) | integration | others 187 pkgs brant_shard_1 exit 0 0m27s bin/shards/integration/others.log; 6 shards: all 746 tests passed, each exactly once, in its own shard (124 + 134 + 122 + 107 + 121 + 138); wall 5m18s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-18T07:57Z |
| g4a-merged | 13a8f8b | G4a (database, integration and race by the lead) | race | others 187 pkgs brant_shard_1 exit 0 0m44s bin/shards/race/others.log; 6 shards: all 746 tests passed, each exactly once, in its own shard (124 + 134 + 122 + 107 + 121 + 138); wall 6m51s (databases included); test-shards: race SHARDS=6 passed | 2026-11-18T08:04Z |
| g3-merged | a35c81c | G3 on top of G4a | db | 2 applied, 92 recorded | 2026-11-18T08:18Z |
| g3-merged | a35c81c | G3 on top of G4a | integration | others 187 pkgs brant_shard_1 exit 0 0m28s bin/shards/integration/others.log; 6 shards: all 750 tests passed, each exactly once, in its own shard (125 + 134 + 122 + 109 + 122 + 138); wall 5m35s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-18T08:23Z |
| g3-merged | a35c81c | G3 on top of G4a | race | others 187 pkgs brant_shard_1 exit 0 0m36s bin/shards/race/others.log; 6 shards: all 750 tests passed, each exactly once, in its own shard (125 + 134 + 122 + 109 + 122 + 138); wall 5m57s (databases included); test-shards: race SHARDS=6 passed | 2026-11-18T08:29Z |
| g3-merged | a35c81c | G3 on top of G4a | scripted suite at 100 connections | 70 of 70 trials passed | 2026-11-18T08:20Z |
| g5 | d8ed11a | the lead's change d8ed11a | db | 0 applied, 94 recorded | 2026-11-18T10:36Z |
| g5 | d8ed11a | the lead's change d8ed11a | integration | others 187 pkgs brant_shard_1 exit 0 0m27s bin/shards/integration/others.log; 6 shards: all 751 tests passed, each exactly once, in its own shard (125 + 134 + 122 + 110 + 122 + 138); wall 5m27s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-18T10:41Z |
| g5 | d8ed11a | the lead's change d8ed11a | race | others 187 pkgs brant_shard_1 exit 0 0m37s bin/shards/race/others.log; 6 shards: all 751 tests passed, each exactly once, in its own shard (125 + 134 + 122 + 110 + 122 + 138); wall 6m13s (databases included); test-shards: race SHARDS=6 passed | 2026-11-18T10:47Z |
| g5 | d8ed11a | the lead's change d8ed11a | scripted suite at 100 connections | 70 of 70 trials passed | 2026-11-18T10:49Z |
| g5 | d8ed11a | the lead's change d8ed11a | gate summary (`g5-gate.out`) | overall=0; gate_rc=0; scripted suite exit 0, 132 s, peak 13 client connections; run 20261118T104748Z | |
| g6 | c9331e9 | G6 | db | 0 applied, 94 recorded | 2026-11-18T11:42Z |
| g6 | c9331e9 | G6 | integration | others 187 pkgs brant_shard_1 exit 0 0m30s bin/shards/integration/others.log; 6 shards: all 754 tests passed, each exactly once, in its own shard (125 + 134 + 122 + 111 + 122 + 140); wall 5m50s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-18T11:47Z |
| g6 | c9331e9 | G6 | race | others 187 pkgs brant_shard_1 exit 0 0m39s bin/shards/race/others.log; 6 shards: all 754 tests passed, each exactly once, in its own shard (125 + 134 + 122 + 111 + 122 + 140); wall 6m47s (databases included); test-shards: race SHARDS=6 passed | 2026-11-18T11:54Z |
| g6 | c9331e9 | G6 | scripted suite at 100 connections | 70 of 70 trials passed | 2026-11-18T11:56Z |
| g6 | c9331e9 | G6 | gate summary (`g6-gate.out`) | overall=0; gate_rc=0; scripted suite exit 0, 100 s, peak 13 client connections; run 20261118T115445Z | |
| g7 | 1bd4019 | G7a 69ea129, G7b 1bd4019 | db | 1 applied, 94 recorded | 2026-11-19T08:06Z |
| g7 | 1bd4019 | G7a 69ea129, G7b 1bd4019 | integration | others 188 pkgs brant_shard_1 exit 0 0m23s bin/shards/integration/others.log; 6 shards: all 765 tests passed, each exactly once, in its own shard (127 + 138 + 123 + 114 + 122 + 141); wall 5m00s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-19T08:11Z |
| g7 | 1bd4019 | G7a 69ea129, G7b 1bd4019 | race | others 188 pkgs brant_shard_1 exit 0 0m34s bin/shards/race/others.log; 6 shards: all 765 tests passed, each exactly once, in its own shard (127 + 138 + 123 + 114 + 122 + 141); wall 7m57s (databases included); test-shards: race SHARDS=6 passed | 2026-11-19T08:19Z |
| g7 | 1bd4019 | G7a 69ea129, G7b 1bd4019 | scripted suite at 100 connections | 70 of 70 trials passed | 2026-11-19T08:21Z |
| g7 | 1bd4019 | G7a 69ea129, G7b 1bd4019 | gate summary (`g7-gate.out`) | overall=0; gate_rc=0; scripted suite exit 0, 140 s, peak 13 client connections; run 20261119T081935Z | |
| g8 | 1aa0290 | G8 | db | 0 applied, 95 recorded | 2026-11-19T09:59Z |
| g8 | 1aa0290 | G8 | integration | others 188 pkgs brant_shard_1 exit 0 0m44s bin/shards/integration/others.log; 6 shards: all 767 tests passed, each exactly once, in its own shard (127 + 138 + 123 + 115 + 123 + 141); wall 8m57s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-19T10:08Z |
| g8 | 1aa0290 | G8 | race | others 188 pkgs brant_shard_1 exit 0 1m02s bin/shards/race/others.log; 6 shards: all 767 tests passed, each exactly once, in its own shard (127 + 138 + 123 + 115 + 123 + 141); wall 9m09s (databases included); test-shards: race SHARDS=6 passed | 2026-11-19T10:17Z |
| g8 | 1aa0290 | G8 | scripted suite at 100 connections | 70 of 70 trials passed | 2026-11-19T10:19Z |
| g8 | 1aa0290 | G8 | gate summary (`g8-gate.out`) | overall=0; gate_rc=0; scripted suite exit 0, 127 s, peak 12 client connections; run 20261119T101737Z | |

## Gates the lead did not run, or did not record

- A6 (04baa4d) had `make check` only at its merge (`s4-merge-a6-check.log`); the gate on 1cd8e9e contains it.
- G4b (e4b577a) was pushed by the lead without a database gate: the lead notes record `make check` 0 and the scripted suite (65 of 70, then the reads family 5 of 5), and that integration and race were not run locally; the remote run on e4b577a is in the table below.
- `69ea129` (G7a) has no gate of its own: the gate `g7` ran on `1bd4019`, which contains it. The gate `g5` ran on d8ed11a, `g6` on c9331e9, `g8` on 1aa0290.
- `s4-after-g2` is void (the lead edited the source tree while it ran; the race step compiled the edited tree and failed `TestQ01PromptStatesTheOrderOfWork`); `s4-after-g2-fix` is the gate that counts for G2.
- The race step of `s4-after-r6` failed one test, `TestS5RD18TheConcessionShowsOnTheShipmentAndItsDocuments` (no data race); the lead's fix is 1d7ad7e and the gate `s4-after-d2` covers it.
- Remote runs: 43399159790 (141929f) through 43437479902 (0cf5920) and 44054240504 (8e01e00) are red. The lead read the logs of 43437479902 and 44054240504 (2026-11-17) and found one failing test in each, `TestGoalReadinessJudgesQuoteReady`, with `remaining connection slots are reserved for roles with the SUPERUSER attribute (SQLSTATE 53300)`: a CI job's PostgreSQL has 100 connections, the lead's local containers had 300 (2da45f9), so every local gate passed. The lead notes attribute every red run since 141929f to it (the logs of the other red runs are not kept), fixed it in d89b2c1 (the harness helpers close their pool after each statement), and 44056969203 on d89b2c1 is the first green run since b671767. Local gates cannot see this condition; from d89b2c1 on the lead also runs the scripted suite against `brant-pg16-ci100`, a server with 100 connections.
- The integration step of `s4-after-b3` failed one test, `TestGoalReadinessKeepsATimelyContractAfterTheQuoteExpired` (a 4 s validity window that lapsed under the sharded gate; lead notes); the lead's fix is 4e00b2b. The integration and race steps of `s4-after-b2` each failed `TestGoalVerifyOfABoundTaskFollowsTRDsControl` (a semantic merge conflict between B1 and B2); the fix is dc045fe.

## make check at every merge

`s4-merge-<unit>-check.log` and `g*-check.log` (CI=true make check in the lead's checkout after each merge). A log that ends with the
dependency check's "no violations" line ran to the end; the lead notes record exit 0 for each. The logs hold no exit status.

| log | dependency-check line | end (UTC) |
|---|---|---|
| g3-check.log | 188 packages, 72 relationships, no violations | 2026-11-18T08:17Z |
| g4a-check.log | 188 packages, 72 relationships, no violations | 2026-11-18T07:50Z |
| g4b-check.log | 188 packages, 72 relationships, no violations | 2026-11-18T08:38Z |
| g4b-check2.log | 188 packages, 72 relationships, no violations | 2026-11-18T09:19Z |
| g5-check.log | 188 packages, 72 relationships, no violations | 2026-11-18T10:36Z |
| g6-check.log | 188 packages, 72 relationships, no violations | 2026-11-18T11:41Z |
| g7a-check.log | 188 packages, 72 relationships, no violations | 2026-11-19T08:03Z |
| g7b-check.log | 189 packages, 72 relationships, no violations | 2026-11-19T08:05Z |
| g8-check.log | 189 packages, 72 relationships, no violations | 2026-11-19T09:59Z |
| s4-after-g2-fix-check.log | 188 packages, 72 relationships, no violations | 2026-11-18T04:53Z |
| s4-merge-a1-check.log | 158 packages, 69 relationships, no violations | 2026-11-13T08:21Z |
| s4-merge-a2-check.log | 152 packages, 63 relationships, no violations | 2026-11-13T08:10Z |
| s4-merge-a3-check.log | 165 packages, 69 relationships, no violations | 2026-11-13T08:59Z |
| s4-merge-a5-check.log | 165 packages, 69 relationships, no violations | 2026-11-13T08:22Z |
| s4-merge-a6-check.log | 169 packages, 70 relationships, no violations | 2026-11-13T09:34Z |
| s4-merge-b1-check.log | 173 packages, 70 relationships, no violations | 2026-11-13T09:53Z |
| s4-merge-b2-check.log | 173 packages, 70 relationships, no violations | 2026-11-13T09:54Z |
| s4-merge-b3-check.log | 171 packages, 70 relationships, no violations | 2026-11-13T09:38Z |
| s4-merge-b4-check.log | 176 packages, 70 relationships, no violations | 2026-11-13T12:42Z |
| s4-merge-c1-check.log | 175 packages, 70 relationships, no violations | 2026-11-13T12:40Z |
| s4-merge-c2-check.log | 179 packages, 72 relationships, no violations | 2026-11-13T12:58Z |
| s4-merge-c3-check.log | 180 packages, 72 relationships, no violations | 2026-11-13T13:52Z |
| s4-merge-c4-check.log | 180 packages, 72 relationships, no violations | 2026-11-13T13:26Z |
| s4-merge-d1-check.log | 188 packages, 72 relationships, no violations | 2026-11-17T10:23Z |
| s4-merge-d2-check.log | 187 packages, 72 relationships, no violations | 2026-11-17T09:07Z |
| s4-merge-f1-check.log | 182 packages, 72 relationships, no violations | 2026-11-17T09:05Z |
| s4-merge-g1-check.log | 188 packages, 72 relationships, no violations | 2026-11-17T12:16Z |
| s4-merge-g2-check.log | 188 packages, 72 relationships, no violations | 2026-11-18T04:10Z |
| s4-merge-r0-check.log | 165 packages, 70 relationships, no violations | 2026-11-13T09:11Z |
| s4-merge-r1-check.log | 176 packages, 72 relationships, no violations | 2026-11-13T12:43Z |
| s4-merge-r10-check.log | 179 packages, 72 relationships, no violations | 2026-11-13T13:18Z |
| s4-merge-r11-check.log | 175 packages, 70 relationships, no violations | 2026-11-13T11:45Z |
| s4-merge-r12-check.log | 188 packages, 72 relationships, no violations | 2026-11-17T11:52Z |
| s4-merge-r13-check.log | 187 packages, 72 relationships, no violations | 2026-11-17T10:03Z |
| s4-merge-r2-check.log | 173 packages, 70 relationships, no violations | 2026-11-13T11:23Z |
| s4-merge-r3-check.log | 176 packages, 72 relationships, no violations | 2026-11-13T12:58Z |
| s4-merge-r4-check.log | 175 packages, 70 relationships, no violations | 2026-11-13T11:45Z |
| s4-merge-r5-check.log | 175 packages, 70 relationships, no violations | 2026-11-13T11:44Z |
| s4-merge-r6-check.log | 180 packages, 72 relationships, no violations | 2026-11-13T13:53Z |
| s4-merge-r7-check.log | 176 packages, 72 relationships, no violations | 2026-11-13T12:44Z |
| s4-merge-r8-check.log | 181 packages, 72 relationships, no violations | 2026-11-17T09:03Z |
| s4-merge-r9-check.log | 176 packages, 72 relationships, no violations | 2026-11-13T12:57Z |

## Remote runs of branch s4/tasks-ai (from 05_github_actions_runs.json.txt)

Workflow `Brant M0 / PG16 / Bento RC2` (job `brant-ci` is the one `main` requires). One row per push. e98e826 is the commit the S4 work started from; 3b76201 is an earlier push of the same branch. The last row is 1aa0290, the tree this evidence describes.

| commit | run | result | created (UTC) | updated (UTC) |
|---|---|---|---|---|
| 3b76201 | [43147271186](https://github.com/idp-xyz/idp-brantgo/actions/runs/43147271186) | success | 2026-11-12T03:18:05Z | 2026-11-12T03:29:51Z |
| e98e826 | [43384984992](https://github.com/idp-xyz/idp-brantgo/actions/runs/43384984992) | success | 2026-11-13T07:19:57Z | 2026-11-13T07:24:30Z |
| 37c4667 | [43392556783](https://github.com/idp-xyz/idp-brantgo/actions/runs/43392556783) | success | 2026-11-13T08:21:15Z | 2026-11-13T08:25:32Z |
| b671767 | [43393823895](https://github.com/idp-xyz/idp-brantgo/actions/runs/43393823895) | success | 2026-11-13T08:31:03Z | 2026-11-13T08:35:42Z |
| 141929f | [43399159790](https://github.com/idp-xyz/idp-brantgo/actions/runs/43399159790) | failure | 2026-11-13T09:10:54Z | 2026-11-13T09:15:14Z |
| 90925f3 | [43402302640](https://github.com/idp-xyz/idp-brantgo/actions/runs/43402302640) | failure | 2026-11-13T09:33:26Z | 2026-11-13T09:37:59Z |
| 04baa4d | [43402436187](https://github.com/idp-xyz/idp-brantgo/actions/runs/43402436187) | failure | 2026-11-13T09:34:25Z | 2026-11-13T09:41:28Z |
| dc045fe | [43416647475](https://github.com/idp-xyz/idp-brantgo/actions/runs/43416647475) | failure | 2026-11-13T11:23:14Z | 2026-11-13T11:28:14Z |
| a84a10a | [43419370878](https://github.com/idp-xyz/idp-brantgo/actions/runs/43419370878) | failure | 2026-11-13T11:43:48Z | 2026-11-13T11:48:55Z |
| 6ff5ed6 | [43426963263](https://github.com/idp-xyz/idp-brantgo/actions/runs/43426963263) | failure | 2026-11-13T12:38:58Z | 2026-11-13T12:44:09Z |
| cb36096 | [43429485131](https://github.com/idp-xyz/idp-brantgo/actions/runs/43429485131) | failure | 2026-11-13T12:57:14Z | 2026-11-13T13:02:24Z |
| 8279edd | [43432526599](https://github.com/idp-xyz/idp-brantgo/actions/runs/43432526599) | failure | 2026-11-13T13:18:24Z | 2026-11-13T13:24:05Z |
| c3d024e | [43433557966](https://github.com/idp-xyz/idp-brantgo/actions/runs/43433557966) | failure | 2026-11-13T13:25:44Z | 2026-11-13T13:31:18Z |
| 0cf5920 | [43437479902](https://github.com/idp-xyz/idp-brantgo/actions/runs/43437479902) | failure | 2026-11-13T13:52:27Z | 2026-11-13T13:58:09Z |
| 8e01e00 | [44054240504](https://github.com/idp-xyz/idp-brantgo/actions/runs/44054240504) | failure | 2026-11-17T09:17:36Z | 2026-11-17T09:23:21Z |
| d89b2c1 | [44056969203](https://github.com/idp-xyz/idp-brantgo/actions/runs/44056969203) | success | 2026-11-17T09:35:33Z | 2026-11-17T09:40:44Z |
| 82c49f6 | [44063110593](https://github.com/idp-xyz/idp-brantgo/actions/runs/44063110593) | success | 2026-11-17T10:15:21Z | 2026-11-17T10:21:22Z |
| 41fbd71 | [44071375459](https://github.com/idp-xyz/idp-brantgo/actions/runs/44071375459) | success | 2026-11-17T11:09:05Z | 2026-11-17T11:14:54Z |
| d3a8fca | [44079783198](https://github.com/idp-xyz/idp-brantgo/actions/runs/44079783198) | success | 2026-11-17T12:04:50Z | 2026-11-17T12:10:44Z |
| e7ae60d | [44083523522](https://github.com/idp-xyz/idp-brantgo/actions/runs/44083523522) | success | 2026-11-17T12:28:54Z | 2026-11-17T12:34:27Z |
| 3185097 | [44241733658](https://github.com/idp-xyz/idp-brantgo/actions/runs/44241733658) | success | 2026-11-18T05:05:08Z | 2026-11-18T05:10:58Z |
| 13a8f8b | [44263986831](https://github.com/idp-xyz/idp-brantgo/actions/runs/44263986831) | success | 2026-11-18T08:05:17Z | 2026-11-18T08:10:44Z |
| a35c81c | [44267532304](https://github.com/idp-xyz/idp-brantgo/actions/runs/44267532304) | success | 2026-11-18T08:30:11Z | 2026-11-18T08:36:23Z |
| e4b577a | [44274884768](https://github.com/idp-xyz/idp-brantgo/actions/runs/44274884768) | success | 2026-11-18T09:19:46Z | 2026-11-18T09:25:55Z |
| d8ed11a | [44291475627](https://github.com/idp-xyz/idp-brantgo/actions/runs/44291475627) | success | 2026-11-18T11:05:34Z | 2026-11-18T11:11:11Z |
| c9331e9 | [44299520933](https://github.com/idp-xyz/idp-brantgo/actions/runs/44299520933) | success | 2026-11-18T11:58:31Z | 2026-11-18T12:03:48Z |
| 1bd4019 | [44487941841](https://github.com/idp-xyz/idp-brantgo/actions/runs/44487941841) | success | 2026-11-19T08:28:48Z | 2026-11-19T08:35:00Z |
| 1aa0290 | [44506427352](https://github.com/idp-xyz/idp-brantgo/actions/runs/44506427352) | success | 2026-11-19T10:31:17Z | 2026-11-19T10:35:54Z |
