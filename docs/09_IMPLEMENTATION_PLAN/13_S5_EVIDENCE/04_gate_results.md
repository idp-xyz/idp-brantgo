# S5 lead gates per merge batch

Source: the lead's gate logs `.claude/handover/scratchpad/lead/logs/<tag>-<step>.log`, written by `lead/gate.sh` in the lead's
checkout (`/home/tops/workspace/idp-brantgo`, branch s5/business) against the lead's PostgreSQL 16.12 container `brant-pg16`
on 127.0.0.1:55471, WSL2 Ubuntu-24.04, CI=true. db = scripts/dev-postgres.sh (migrate + grants), integration = `make
integration`, race = `make race`. The logs hold no exit status; the counts below are read from the log text (lines `ok`,
`FAIL`, `WARNING: DATA RACE`), the end time is the log file's modification time (UTC). The commit is the merge commit the lead
had checked out (s5-lead-notes.md). Generated on 2026-11-13T06:51Z by E2's gates.py.

| gate tag | commit | merged units | step | result | end (UTC) |
|---|---|---|---|---|---|
| after-b3 | 5662d7e | B3 | db | 1 applied, 43 recorded | 2026-11-12T07:42Z |
| after-b3 | 5662d7e | B3 | integration | 81 packages ok; 0 packages FAIL; internal/bootstrap 451.905 s | 2026-11-12T07:50Z |
| after-b3 | 5662d7e | B3 | race | 81 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 534.286 s | 2026-11-12T08:00Z |
| after-b6 | f2bda8e | B6 | db | 0 applied, 44 recorded | 2026-11-12T08:02Z |
| after-b6 | f2bda8e | B6 | integration | 81 packages ok; 1 packages FAIL; ended by an outside signal; internal/bootstrap 360.294 s | 2026-11-12T08:08Z |
| after-b6 | f2bda8e | B6 | race | 82 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 566.579 s | 2026-11-12T08:19Z |
| after-b6-rerun | f2bda8e | B6 (integration run again after an outside kill) | integration | 82 packages ok; 0 packages FAIL; internal/bootstrap 471.032 s | 2026-11-12T08:29Z |
| after-b2a | 7e59d81 | B1a 9b5be90, B2a 7e59d81 | db | 2 applied, 44 recorded | 2026-11-12T08:31Z |
| after-b2a | 7e59d81 | B1a 9b5be90, B2a 7e59d81 | integration | 88 packages ok; 0 packages FAIL; internal/bootstrap 495.436 s | 2026-11-12T08:40Z |
| after-b2a | 7e59d81 | B1a 9b5be90, B2a 7e59d81 | race | 88 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 561.388 s | 2026-11-12T08:51Z |
| after-b5 | e50492e | B5 | db | 1 applied, 46 recorded | 2026-11-12T08:52Z |
| after-b5 | e50492e | B5 | integration | 89 packages ok; 0 packages FAIL; internal/bootstrap 479.867 s | 2026-11-12T09:01Z |
| after-b5 | e50492e | B5 | race | 89 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 494.801 s | 2026-11-12T09:10Z |
| after-c11 | 32dc238 | B1a2 f1c6f48, C11 32dc238 | db | 1 applied, 47 recorded | 2026-11-12T09:12Z |
| after-c11 | 32dc238 | B1a2 f1c6f48, C11 32dc238 | integration | 90 packages ok; 0 packages FAIL; internal/bootstrap 467.118 s | 2026-11-12T09:20Z |
| after-c11 | 32dc238 | B1a2 f1c6f48, C11 32dc238 | race | 90 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 549.640 s | 2026-11-12T09:30Z |
| after-b6b | 9637ec1 | B6b | db | 0 applied, 48 recorded | 2026-11-12T09:31Z |
| after-b6b | 9637ec1 | B6b | integration | 90 packages ok; 0 packages FAIL; internal/bootstrap 499.918 s | 2026-11-12T09:40Z |
| after-b6b | 9637ec1 | B6b | race | 90 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 597.992 s | 2026-11-12T09:51Z |
| after-b4 | ceb8026 | B4 | db | 1 applied, 48 recorded | 2026-11-12T09:56Z |
| after-b4 | ceb8026 | B4 | integration | 90 packages ok; 0 packages FAIL; internal/bootstrap 598.522 s | 2026-11-12T10:07Z |
| after-b4 | ceb8026 | B4 | race | 90 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 592.429 s | 2026-11-12T10:18Z |
| after-c4a | f88bee4 | B1b 1a1ec07, C3b 79484ed, C4a f88bee4 | db | 2 applied, 49 recorded | 2026-11-12T11:11Z |
| after-c4a | f88bee4 | B1b 1a1ec07, C3b 79484ed, C4a f88bee4 | integration | 93 packages ok; 1 packages FAIL; internal/bootstrap 535.535 s; failing tests: TestShipmentLineAndExportLotLookups | 2026-11-12T11:21Z |
| after-c4a | f88bee4 | B1b 1a1ec07, C3b 79484ed, C4a f88bee4 | race | 93 packages ok; 1 packages FAIL; 0 data races; internal/bootstrap 616.216 s; failing tests: TestShipmentLineAndExportLotLookups | 2026-11-12T11:32Z |
| after-c4bs | 6244a7e | test fix 582b692, C4b-CNX 9d668fc, C4b-STL 6244a7e | db | 1 applied, 51 recorded | 2026-11-12T11:37Z |
| after-c4bs | 6244a7e | test fix 582b692, C4b-CNX 9d668fc, C4b-STL 6244a7e | integration | 95 packages ok; 0 packages FAIL; internal/bootstrap 599.360 s | 2026-11-12T11:48Z |
| after-c4bs | 6244a7e | test fix 582b692, C4b-CNX 9d668fc, C4b-STL 6244a7e | race | 95 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 722.363 s | 2026-11-12T12:01Z |
| after-c1 | a422f9e | C1 | db | 1 applied, 52 recorded | 2026-11-12T12:05Z |
| after-c1 | a422f9e | C1 | integration | 96 packages ok; 0 packages FAIL; internal/bootstrap 729.826 s | 2026-11-12T12:19Z |
| after-c1 | a422f9e | C1 | race | 96 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 1142.203 s | 2026-11-12T12:40Z |
| after-wave2 | 638d02a | B2b b760a7c, C3a 8f85e62, C2 6378737, C4b-PUR 638d02a | db | 4 applied, 53 recorded | 2026-11-12T12:42Z |
| after-wave2 | 638d02a | B2b b760a7c, C3a 8f85e62, C2 6378737, C4b-PUR 638d02a | integration | 101 packages ok; 0 packages FAIL; internal/bootstrap 642.787 s | 2026-11-12T12:53Z |
| after-wave2 | 638d02a | B2b b760a7c, C3a 8f85e62, C2 6378737, C4b-PUR 638d02a | race | 101 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 768.624 s | 2026-11-12T13:08Z |
| after-c5 | fe0b160 | C6 799b195, C5 fe0b160 | db | 2 applied, 57 recorded | 2026-11-12T13:30Z |
| after-c5 | fe0b160 | C6 799b195, C5 fe0b160 | integration | 102 packages ok; 0 packages FAIL; internal/bootstrap 833.804 s | 2026-11-12T13:45Z |
| after-c5 | fe0b160 | C6 799b195, C5 fe0b160 | race | 102 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 854.673 s | 2026-11-12T14:01Z |
| after-c8 | 2a69b7d | C4b-FUL 43122ce, C8 2a69b7d | db | 2 applied, 59 recorded | 2026-11-12T14:02Z |
| after-c8 | 2a69b7d | C4b-FUL 43122ce, C8 2a69b7d | integration | 104 packages ok; 0 packages FAIL; internal/bootstrap 714.896 s | 2026-11-12T14:15Z |
| after-c8 | 2a69b7d | C4b-FUL 43122ce, C8 2a69b7d | race | 104 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 827.425 s | 2026-11-12T14:30Z |
| after-c1b | 6a79b3e | C1b | db | 1 applied, 61 recorded | 2026-11-12T14:59Z |
| after-c1b | 6a79b3e | C1b | race | 104 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 968.419 s | 2026-11-12T15:17Z |
| after-c7 | ad9e7fd | C9 fc6fc58, C7 ad9e7fd | db | 1 applied, 62 recorded | 2026-11-13T01:24Z |
| after-c7 | ad9e7fd | C9 fc6fc58, C7 ad9e7fd | integration | 106 packages ok; 1 packages FAIL; internal/bootstrap 711.362 s; failing tests: TestLoadGeneratorSmoke | 2026-11-13T01:37Z |
| after-c7 | ad9e7fd | C9 fc6fc58, C7 ad9e7fd | race | 107 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 832.670 s | 2026-11-13T01:53Z |
| after-c1c | 4d1aa66 | C1c (s5/business already at 3a7cb7f) | db | 0 applied, 63 recorded | 2026-11-13T02:06Z |
| after-c1c | 4d1aa66 | C1c (s5/business already at 3a7cb7f) | integration | 107 packages ok; 0 packages FAIL; internal/bootstrap 777.537 s | 2026-11-13T02:20Z |
| after-c1c | 4d1aa66 | C1c (s5/business already at 3a7cb7f) | race | 107 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 859.374 s | 2026-11-13T02:36Z |
| after-c10 | fd6635b | D1 2c7cd8a, C10 fd6635b | db | 2 applied, 63 recorded | 2026-11-13T02:37Z |
| after-c10 | fd6635b | D1 2c7cd8a, C10 fd6635b | integration | 107 packages ok; 0 packages FAIL; internal/bootstrap 818.859 s | 2026-11-13T02:53Z |
| after-c10 | fd6635b | D1 2c7cd8a, C10 fd6635b | race | 107 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 880.249 s | 2026-11-13T03:09Z |
| after-d2a | 7aedd52 | D2a (its tree equals the unit's gated tree; integration ran there) | db | 0 applied, 65 recorded | 2026-11-13T03:45Z |
| after-d2a | 7aedd52 | D2a (its tree equals the unit's gated tree; integration ran there) | race | 107 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 1009.401 s | 2026-11-13T04:04Z |
| after-d2b | f76861c | D2b | db | 0 applied, 65 recorded | 2026-11-13T04:04Z |
| after-d2b | f76861c | D2b | integration | 107 packages ok; 0 packages FAIL; internal/bootstrap 939.472 s | 2026-11-13T04:22Z |
| after-d2b | f76861c | D2b | race | 107 packages ok; 0 packages FAIL; 0 data races; internal/bootstrap 1483.536 s | 2026-11-13T04:49Z |
| after-ci | ed1550c | CI unit (sharded gates, SHARDS=6) | integration | others       151 pkgs    brant_shard_1   exit 0      0m23s   bin/shards/integration/others.log; testshard: ./internal/bootstrap: 6 shards: all 600 tests passed, each exactly once, in its own shard (103 + 107 + 95 + 93 + 95 + 107); wall 3m52s (databases included); test-shards: integration SHARDS=6 passed | 2026-11-13T06:40Z |
| after-ci | ed1550c | CI unit (sharded gates, SHARDS=6) | race | others       151 pkgs    brant_shard_1   exit 0      0m30s   bin/shards/race/others.log; testshard: ./internal/bootstrap: 6 shards: all 600 tests passed, each exactly once, in its own shard (103 + 107 + 95 + 93 + 95 + 107); wall 4m20s (databases included); test-shards: race SHARDS=6 passed | 2026-11-13T06:45Z |

## make check at every merge

`lead/logs/merge-<unit>-check.log` (CI=true make check in the lead's checkout after each merge). A log that ends with the
dependency check's "no violations" line ran to the end; the lead notes record exit 0 for each.

| log | dependency-check line | end (UTC) |
|---|---|---|
| base-10d0bfe-check.log | 119 packages, 51 relationships, no violations | 2026-11-12T06:22Z |
| fix-load-check.log | 151 packages, 63 relationships, no violations | 2026-11-13T01:54Z |
| merge-b1a-check.log | 127 packages, 63 relationships, no violations | 2026-11-12T08:30Z |
| merge-b1a2-check.log | 131 packages, 63 relationships, no violations | 2026-11-12T09:11Z |
| merge-b1b-check.log | 134 packages, 63 relationships, no violations | 2026-11-12T11:10Z |
| merge-b2a-check.log | 130 packages, 63 relationships, no violations | 2026-11-12T08:31Z |
| merge-b2b-check.log | 141 packages, 63 relationships, no violations | 2026-11-12T12:40Z |
| merge-b3-check.log | 120 packages, 51 relationships, no violations | 2026-11-12T07:42Z |
| merge-b4-check.log | 132 packages, 63 relationships, no violations | 2026-11-12T09:56Z |
| merge-b5-check.log | 131 packages, 63 relationships, no violations | 2026-11-12T08:52Z |
| merge-b6-check.log | 121 packages, 51 relationships, no violations | 2026-11-12T08:02Z |
| merge-b6b-check.log | 131 packages, 63 relationships, no violations | 2026-11-12T09:31Z |
| merge-c1-check.log | 139 packages, 63 relationships, no violations | 2026-11-12T12:05Z |
| merge-c10-check.log | 151 packages, 63 relationships, no violations | 2026-11-13T02:37Z |
| merge-c11-check.log | 131 packages, 63 relationships, no violations | 2026-11-12T09:11Z |
| merge-c1b-check.log | 148 packages, 63 relationships, no violations | 2026-11-12T14:59Z |
| merge-c1c-check.log | 151 packages, 63 relationships, no violations | 2026-11-13T02:06Z |
| merge-c2-check.log | 144 packages, 63 relationships, no violations | 2026-11-12T12:41Z |
| merge-c3a-check.log | 143 packages, 63 relationships, no violations | 2026-11-12T12:41Z |
| merge-c3b-check.log | 136 packages, 63 relationships, no violations | 2026-11-12T11:10Z |
| merge-c4a-check.log | 137 packages, 63 relationships, no violations | 2026-11-12T11:11Z |
| merge-c4bc-check.log | 138 packages, 63 relationships, no violations | 2026-11-12T11:34Z |
| merge-c4bf-check.log | 148 packages, 63 relationships, no violations | 2026-11-12T14:02Z |
| merge-c4bp-check.log | 145 packages, 63 relationships, no violations | 2026-11-12T12:41Z |
| merge-c4bs-check.log | 138 packages, 63 relationships, no violations | 2026-11-12T11:37Z |
| merge-c5-check.log | 146 packages, 63 relationships, no violations | 2026-11-12T13:30Z |
| merge-c6-check.log | 145 packages, 63 relationships, no violations | 2026-11-12T13:27Z |
| merge-c7-check.log | 151 packages, 63 relationships, no violations | 2026-11-13T01:24Z |
| merge-c8-check.log | 148 packages, 63 relationships, no violations | 2026-11-12T14:02Z |
| merge-c9-check.log | 150 packages, 63 relationships, no violations | 2026-11-13T01:24Z |
| merge-ci-check.log | 152 packages, 63 relationships, no violations | 2026-11-13T06:36Z |
| merge-d1-check.log | 151 packages, 63 relationships, no violations | 2026-11-13T02:37Z |
| merge-d2a-check.log | 151 packages, 63 relationships, no violations | 2026-11-13T03:45Z |
| merge-d2b-check.log | 151 packages, 63 relationships, no violations | 2026-11-13T04:04Z |

Wave A (A1-A8, A7b) was gated by the previous lead session: `make integration` exit 0 on the adcc88e tree and GitHub
Actions run 43162711473 on 4afa604, 43164621807 on 10d0bfe (s5-lead-notes.md); its logs are not in lead/logs.
