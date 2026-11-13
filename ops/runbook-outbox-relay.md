# Runbook: Outbox relay lag and dead letters

Operations runbook for `brant-worker`, the Outbox relay. It covers the relay latency targets and alert thresholds of
**S5R-D07**, the metrics the worker exposes, what to do when the relay lags, and how to handle dead letters
(**ADR-BR-015**).

> **Status.** S5R-D07 was signed off in a simulation (evidence tier E0, 2026-11-13). Its numbers are engineering
> targets for the reference deployment. A real owner of operations and finance must confirm them before B0
> (WBS S5R-11; approval matrix "运行目标：Outbox 中继延迟与告警阈值"). G4 stays NO-GO. The numbers have only been
> measured on one WSL2 development host. The alert thresholds are worker flags, so a confirmed value needs no release.

## 1. Targets and thresholds (S5R-D07)

| What | Value | Where it is measured |
|---|---|---|
| Relay latency P95, reference load | ≤ 5 s | `brant_outbox_relay_delivery_latency_seconds` (worker), `tests/load` (observer) |
| Relay latency P99, reference load | ≤ 30 s | same |
| Oldest undelivered event: alert | ≥ 60 s | `brant_outbox_oldest_undelivered_age_seconds`; worker log WARN |
| Oldest undelivered event: incident | ≥ 300 s | same; worker log ERROR |
| Dispatch workers, reference deployment | at least 2; scale to 3 under load | process count |
| Dead letters | a separate alert; not part of the latency | `brant_outbox_relay_dead_letters_total`; worker log ERROR |

- **Relay latency** runs from an event's commit (its `recorded_at`, stamped in the committing transaction) until the
  relay marks it published. With `-dispatch`, the relay marks an event published only after every subscribed consumer
  has processed it in its own transaction.
- **Reference load:** 16 command users plus 16 read users of `tests/load` (BRC-001's order cycle, one tenant, one
  customer).
- **Undelivered** means `PENDING` or `LEASED` in `bento.outbox`. Abandoned deliveries are dead letters and are counted
  apart.
- **What is shown so far (local evidence only):**
  - With one worker the backlog grows from 8 + 8 users: P95 20 s at 8 + 8 and 45 s at 16 + 16 (S5 C7).
  - Two workers kept P95 at 7.5 s at 8 + 8 and 9.7 s at 16 + 16 (C7 run 3).
  - The 5 s target at 16 + 16 with two workers has **not** been demonstrated. A third worker or tuning may be needed
    (section 5).
- **Business impact of lag.** While events are undelivered, downstream facts trail the commands: STL credit
  consumption and order accounts, reviews, CNX re-evaluation, FUL and PUR reactions. The latency target bounds that
  window (S5R-D46 (3)).

## 2. Deployment

```
IDP_BRANT_POSTGRES_DSN=... brant-worker -dispatch -metrics-listen 10.0.0.5:9464    # worker 1
IDP_BRANT_POSTGRES_DSN=... brant-worker -dispatch -metrics-listen 10.0.0.6:9464    # worker 2 (another host or address)
```

- Run **at least two** `-dispatch` workers. Leases and partitions let them share the Outbox: each partition (one
  aggregate's events) goes to one worker at a time, in order.
- Add a third worker when the P95 target is missed at the reference load. Under load one worker moved about
  85–94 events/s on the C7 host, while the peak load produces about 175 events/s.
- Each consumer's system actor `system:inbox.<consumer>` needs its grant in every company it serves:
  `brant-admin inbox -tenant <t> -company <c>`. Run it again when a consumer's permissions change.

Worker flags this runbook refers to:

| Flag | Default | Meaning |
|---|---|---|
| `-metrics-listen` | empty (none) | Address for `GET /metrics` (Prometheus text format 0.0.4). It has no authentication: bind it to loopback or a private interface only. |
| `-probe-interval` | 15 s | How often the worker reads the Outbox backlog and the oldest undelivered event. 0 turns the probe and its lag alerts off. |
| `-lag-alert` / `-lag-incident` | 60 s / 300 s | Age of the oldest undelivered event that logs the alert (WARN) and the incident (ERROR). |
| `-batch`, `-lease`, `-sink-timeout` | 10, 1 min, 5 s | `-lease` must be longer than `-batch` × `-sink-timeout`. |
| `-poll` | 1 s | Wait between claims while the Outbox is idle. It sets most of the latency at low load: C7 measured P95 ≈ 1 s there. |
| `-max-failures`, `-retry-base`, `-retry-max` | 10, 1 s, 5 min | A failing head event is retried after 1, 2, 4, … 256 s and dead-lettered at the 10th failure, about 8.5 minutes later. Its partition waits until then. |

**Clocks.** `recorded_at` comes from the API host's wall clock, and `published_at` and the lag figures from the worker
host's. Leases compare wall clocks too. Hosts must slew their clocks (NTP/chrony), not step them.

- A forward step adds itself to the lag of every event in flight, and can trip the alert.
- R11 measured the WSL2 development host stepping its wall clock forward by about 3.6 s every 20–30 s under load.
  `tests/load` reports the step it saw in each measurement window (`wall_clock_step_s`).

## 3. Metrics

Each worker serves its own metrics. Counters and histograms count that worker's work since it started, so **sum** them
over workers. The backlog gauges describe the single Outbox that all workers share, so take the **max** over workers.

| Metric | Type | Meaning |
|---|---|---|
| `brant_outbox_relay_delivery_latency_seconds{source}` | histogram | Relay latency of each published delivery, by event source (`idp-brant.trd`, …). The buckets include 5, 30, 60 and 300 s, so the share within a target is exact. |
| `brant_outbox_relay_settle_duration_seconds` | histogram | Time to mark one delivery published in the Outbox. The partition's next event waits for it. |
| `brant_outbox_relay_claimed_total` | counter | Deliveries claimed. |
| `brant_outbox_relay_publish_failures_total{failure_code}` | counter | Failed publishes that will be retried. |
| `brant_outbox_relay_dead_letters_total{failure_code,critical,stored}` | counter | Deliveries abandoned (section 6). |
| `brant_outbox_relay_lease_lost_total` | counter | Settlements whose lease another worker had taken over: the delivery outlived its lease. |
| `brant_outbox_relay_round_errors_total` | counter | Claim rounds that ended with an error (database, settlement). |
| `brant_outbox_relay_last_round_timestamp_seconds` | gauge | Last claim round without an error, idle rounds included. It stops moving when the relay stalls. |
| `brant_dispatch_consumer_deliveries_total{consumer,outcome}` | counter | With `-dispatch`: each consumer's outcome per delivery (`processed`, `duplicate`, `retry`, `rejected`). |
| `brant_dispatch_consumer_duration_seconds{consumer}` | histogram | With `-dispatch`: one consumer's time on one delivery, its transaction included. |
| `brant_outbox_backlog_events{state}` | gauge | Undelivered events by state (see the next table). |
| `brant_outbox_oldest_undelivered_age_seconds` | gauge | Age of the oldest undelivered event, or 0 when there is none. |
| `brant_outbox_probe_timestamp_seconds`, `brant_outbox_probe_errors_total` | gauge, counter | Last successful probe, and failed probes. The backlog gauges keep their last value when a probe fails. |

States of `brant_outbox_backlog_events`:

| State | Meaning |
|---|---|
| `pending` | Waiting to be claimed and never failed. |
| `retrying` | Waiting for a retry after a failed publish. |
| `leased` | Claimed by a live worker. |
| `lease_expired` | Claimed by a worker that stopped or stalled; another worker re-claims it after the lease. |

- The probe reads Bento's index of undelivered rows (`outbox_active_recorded_idx`), so its cost follows the backlog,
  not the table.
- The backlog gauges have no samples until a worker's first probe.

### Alert rules (Prometheus example)

```yaml
groups:
  - name: brant-outbox-relay
    rules:
      - alert: BrantOutboxRelayLag            # S5R-D07 alert
        expr: max(brant_outbox_oldest_undelivered_age_seconds) >= 60
        labels: {severity: warning}
      - alert: BrantOutboxRelayLagIncident    # S5R-D07 incident
        expr: max(brant_outbox_oldest_undelivered_age_seconds) >= 300
        labels: {severity: critical}
      - alert: BrantOutboxRelayLatencyP95     # fewer than 95 % within 5 s
        expr: sum(rate(brant_outbox_relay_delivery_latency_seconds_bucket{le="5"}[5m]))
              / sum(rate(brant_outbox_relay_delivery_latency_seconds_count[5m])) < 0.95
        labels: {severity: warning}
      - alert: BrantOutboxRelayLatencyP99     # fewer than 99 % within 30 s
        expr: sum(rate(brant_outbox_relay_delivery_latency_seconds_bucket{le="30"}[5m]))
              / sum(rate(brant_outbox_relay_delivery_latency_seconds_count[5m])) < 0.99
        labels: {severity: warning}
      - alert: BrantOutboxDeadLetter          # ADR-BR-015, separate from the latency
        expr: sum by (failure_code, critical, stored) (increase(brant_outbox_relay_dead_letters_total[10m])) > 0
        labels: {severity: critical}
      - alert: BrantOutboxRelayStalled        # a worker completes no round (one round can take Batch x sink-timeout)
        expr: time() - brant_outbox_relay_last_round_timestamp_seconds > 120
        labels: {severity: critical}
      - alert: BrantOutboxDispatchWorkers     # the reference deployment runs at least 2
        expr: count(up{job="brant-worker"} == 1) < 2
        labels: {severity: warning}
      - alert: BrantOutboxProbeStale
        expr: time() - max(brant_outbox_probe_timestamp_seconds) > 120
        labels: {severity: warning}
```

**Without Prometheus**, alert on the worker's log lines:

| Level | Message (prefix) |
|---|---|
| WARN | `outbox relay lag alert` |
| ERROR | `outbox relay lag incident` |
| INFO | `outbox relay lag recovered` |
| ERROR | `outbox delivery dead-lettered` (`critical=true` for revocation, repudiation and risk events) |
| ERROR | `outbox delivery abandoned without a dead letter` |
| WARN | `outbox probe failed` |
| ERROR | `outbox relay round failed` |

Each worker logs a lag line once per change of level, and each worker raises it.

## 4. When the relay lags

Work through these in order. Read-only SQL can run as `brant_app` or the DBA: `bento.outbox` has no RLS.

1. **Is every worker alive and completing rounds?**
   - Check `up`, `brant_outbox_relay_last_round_timestamp_seconds` and `brant_outbox_relay_round_errors_total`, and the
     log line `outbox relay round failed`.
   - Restart a stalled worker. Its unsettled deliveries are re-claimed by the others after the lease (1 min).
   - Round errors usually mean database trouble: connections, failover or disk.
2. **Where is the backlog?** Read `brant_outbox_backlog_events` by state:
   - **`pending` grows, publish failures stay flat → capacity.**
     - Compare `sum(rate(brant_outbox_relay_delivery_latency_seconds_count[1m]))`, what is delivered, with the
       rate at which commands write events.
     - Check `brant_dispatch_consumer_duration_seconds` for a slow consumer, and
       `brant_outbox_relay_settle_duration_seconds` and the database's waits (C7: WAL flush dominates).
     - Add a worker, up to 3 under load.
   - **`retrying` > 0 and `brant_outbox_relay_publish_failures_total` rises → a consumer (or the sink) fails.**
     - Find the consumer in `brant_dispatch_consumer_deliveries_total{outcome="retry"}` and the log lines
       `outbox publish failed` (event ID and error).
     - Usual causes:
       - a missing grant for `system:inbox.<consumer>`: run `brant-admin inbox` for that tenant and company;
       - the database;
       - an upstream fact the consumer needs is not there yet.
     - The partition (one aggregate) waits behind its failing head event until it succeeds or is dead-lettered, by
       default about 8.5 minutes, which passes the 300 s incident threshold.
   - **`lease_expired` > 0 → a worker died or hung while it held deliveries.** They are re-claimed after the lease.
     If this repeats, a publish is outliving the lease: look at the consumer durations and `-sink-timeout`.
   - **`leased` high while latency is within target:** the workers are busy. That is normal under load.
3. **Which partitions hold the oldest events?**

   ```sql
   SELECT partition_key, count(*) AS undelivered, min(recorded_at) AS oldest, max(failures) AS failures, max(attempt) AS attempts
   FROM bento.outbox WHERE status IN ('PENDING', 'LEASED')
   GROUP BY partition_key ORDER BY oldest LIMIT 20;

   SELECT source, event_id, event_type, partition_key, status, attempt, failures, failure_code, last_failed_at,
          available_at, lease_expires_at, scope
   FROM bento.outbox WHERE status IN ('PENDING', 'LEASED') AND failures > 0
   ORDER BY recorded_at LIMIT 20;
   ```

4. **Tell the users what to expect (S5R-D46 (3)).**
   - Right after an order amendment, STL may not yet have moved the order account to the new revision. The payment
     condition guard then answers `revision_mismatch`, and a write-off or application answers `REFERENCE_STALE` (409,
     "refresh the reference and resubmit").
   - This is eventual consistency, not a data error. Ask users to **retry later**, once the oldest undelivered age is
     below the alert threshold again. Nothing needs correcting by hand.
5. **Incident (≥ 300 s).**
   - Escalate to the engineer on call.
   - Record when the incident started and ended (from the lag log lines), which partitions and consumers were
     involved, and what was changed.
   - Undelivered revocation and risk events do not open a gap: new downstream actions still pass the upstream guard
     functions (ADR-BR-015).

## 5. Measuring the targets (`tests/load`)

```
BRANT_PG_CONTAINER=brant-pg16-load BRANT_PG_PORT=<p> scripts/dev-postgres.sh      # a database of its own
make load PG_PORT=<p> LOAD_STAGES=1,2,4,8,16 LOAD_ARGS="-assert -label d07"        # 2 workers by default
make load PG_PORT=<p> LOAD_STAGES=4,8,16 LOAD_ARGS="-workers 3 -label d07-3w"     # scaled out
```

The run's dispatch workers relay every tenant's pending events, so give it a database of its own.

- `-workers N` sets the number of `brant-worker -dispatch` processes (default 2, the reference deployment).
  `-worker=false` runs none.
- **Checks per stage** (`report.json` → `thresholds`, `summary.md`):

  | Target | Basis | Asserted when |
  |---|---|---|
  | Relay latency P95 ≤ 5 s | S5R-D07 | up to the reference load (`-reference-users 16`) and with at least 2 workers |
  | Relay latency P99 ≤ 30 s | S5R-D07 | same |
  | Oldest undelivered event < 60 s | S5R-D07 | same |
  | Reads P95 ≤ 500 ms | S5-11 (C7) | up to 32 command users per API instance (`-api-users`) |
  | Commands P95 ≤ 800 ms | S5-11 (C7) | same |

  The relay latency is the observer's exact percentile of `published_at − recorded_at` from the Outbox. Above those
  loads, or with fewer workers, a check is reported as `not_asserted`. With `-assert`, a failed check makes the run
  exit 1.
- **Cross-check.** Each stage also reports what the workers' own metrics counted (`worker_relay_metrics`: published,
  share within each target, failures, dead letters, mean settlement). It should match the observer's count for the
  window.
- **Evidence.** Record the date, the host (the report's `environment` and load averages), each window's wall-clock
  step, and what was not executed (`not_executed` in the report). These numbers are host evidence only, never a
  production or commercial commitment.

## 6. Dead letters (ADR-BR-015)

The relay abandons a delivery when one of these happens:

| Code | Cause | Replay |
|---|---|---|
| `publish.failed` | The failure budget (`-max-failures`) or attempt budget ran out. | Fix the cause first: grants (`brant-admin inbox`), consumer bug, database. Then replay; consumers that already processed the event see a duplicate. |
| `publish.rejected` | A consumer or the sink refused the event permanently. | A consumer's rejection is recorded in its Inbox, so a replay is refused again. It needs a business correction, not a replay. A sink's 4xx can be replayed once the sink is fixed. |
| `outbox.claim_limit_exceeded` | Earlier attempts ended unsettled (crash, lost lease) until the attempt budget ran out. | Find out why the deliveries outlived their lease (consumer durations, `-sink-timeout`), then replay. |
| `envelope.invalid_on_publish` | The stored envelope is invalid. | Replay fails validation again. Escalate to development. |

Abandoning a delivery and storing its dead letter happen in one transaction. The dead letter goes to
`brant_msg.dead_letter` (FORCE RLS per tenant and company) and keeps the envelope, attempts, failures and last error.
The alert follows.

- **`critical=true`** means a revocation, repudiation or risk event type. Handle these first. They are never dropped
  quietly.
- **`stored=false`** (`outbox delivery abandoned without a dead letter`): the envelope's scope does not parse, so RLS
  cannot hold a letter. The abandoned row in `bento.outbox` keeps the envelope. There is no replay path; escalate.

**Find the dead letters of one company** (as `brant_app`, inside a transaction bound to the scope):

```sql
BEGIN;
SELECT set_config('app.tenant_id', '<tenant>', true), set_config('app.company_id', '<company>', true);
SELECT source, event_id, event_type, failure_code, critical, dead_at, attempt, failures, last_error,
       replay_attempts, last_replay_error
FROM brant_msg.dead_letter WHERE state = 'dead' ORDER BY critical DESC, dead_at;
COMMIT;
```

A DBA session that bypasses RLS can count the outstanding letters per company:
`SELECT tenant_id, company_id, count(*) FROM brant_msg.dead_letter WHERE state = 'dead' GROUP BY 1, 2;`

**Replay** is done by a human holding `dead_letter.replay` (a human-only command), with an OIDC access token for that
tenant and company. The event goes out unchanged, with its original event ID, so consumers deduplicate it.

- Start `brant-api` with `-replay-dispatch` when the workers run `-dispatch`, or with `-replay-sink <url>` for a sink.
- Then call:

  ```
  curl -X POST -H "Authorization: Bearer $TOKEN" \
       https://<api>/api/brant/v1/msg/dead_letters/<source>/<event_id>/replay
  ```

| Answer | Meaning |
|---|---|
| 200 | Replayed. The letter is marked `replayed`, with who replayed it and the authority epoch. |
| 404 `NOT_FOUND` | No such letter in your scope. |
| 422 `INVALID_STATE` | Already replayed. |
| 403 `PERMISSION_DENIED` | You lack the grant. |
| 502 `REPLAY_FAILED` | The publish failed. The letter stays dead and the attempt and error are recorded; fix the cause and retry. |

Afterwards, check that the consumer processed the event (`brant_dispatch_consumer_deliveries_total` or the
consumer's own state). Record what was replayed and why.

## 7. Not covered here

- A real monitoring stack. The alert rules above are examples; no Prometheus or Alertmanager has been deployed.
- Production capacity figures.
- Off-host log shipping.
- Documentation clerks' manual compliance re-screening before document release (S5R-D10 (b)). That is a FUL
  operations step, not a relay step.
