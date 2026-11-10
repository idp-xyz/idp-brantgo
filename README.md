# IDP Brant · Go + DDD + Bento-Go · M0 engineering baseline

**IDP Brant is one product repository and one Go module. Business code will be organized by DDD bounded context; IDP-Bento-Go stays a separate repository referenced as a Go module version.**

- Product module: `go.idp.xyz/idp-brant` (candidate; the `go.idp.xyz` vanity host serves only `idp-bento-go` so far).
- Framework module: `go.idp.xyz/idp-bento-go@v0.1.0-rc.2` (release candidate), resolved from its public repository.
- Go `1.26.5` (`toolchain` directive); PostgreSQL 16.
- The master archive (DDD, contracts, plans, gates, ADRs) lives in `docs/` and is not published with the code. `docs/00_START_HERE/00_INDEX.html` is its entry page.

M0 builds the engineering baseline only. No bounded context exists yet; S1 adds EVD, PTY, CAT, POL and INQ.

## Layout

```
cmd/brant-migrate/                operator-only migrations: Bento schema, framework migration, then each manifest directory in order
cmd/brant-worker/                 Outbox relay: claim, publish to an HTTP sink, settle; at least once
internal/bootstrap/               composition root: one connection pool, a Bento transaction owner per context and for the relay
internal/kernel/                  value objects shared by every context; standard library only
                                  Decimal (math/big), Money in minor units, largest-remainder allocation, tax splits,
                                  ExchangeRate, Quantity, Ratio, ValidityInterval, timestamps, Scope, ActorRef
internal/foundation/bento/        ScopeTransactor (app.tenant_id and app.company_id per transaction), SystemClock
internal/foundation/database/     PostgreSQL 16 pool with server and application-role checks; pgtest for integration tests
internal/foundation/identity/     Principal (actor and scope from a verified token) in the Go context
internal/foundation/authorization/ command rules (human-only commands), authority guard and grants in brant_auth
internal/foundation/messaging/    Outbox relay, HTTP webhook publisher, dead letters with alerts and human replay (ADR-BR-015)
internal/foundation/strictjson/   closed-structure request decoding (C7): 1 MiB, 64 levels, 100,000 values; no duplicate keys, null, floats, unknown fields
migrations/manifest/contexts.txt  order of migration directories
migrations/auth/                  brant_auth: authority guard row per scope, local authorization projection (FORCE RLS)
migrations/msg/                   brant_msg: dead letters of the Outbox relay (FORCE RLS)
deploy/postgres/                  roles (brant_migrator, brant_app) and application grants
scripts/dev-postgres.sh           development PostgreSQL 16 container brant-pg16 on 127.0.0.1:55471
tools/dependency-check/           package rules; cross-context allowlist generated from the Context Map
tools/spikes/consistency/         M0-10 spikes for ADR-BR-004 and ADR-BR-011 (integration tests)
.github/workflows/ci.yml          required check: formatting, vet, lint, package rules, migrations, PG16 tests, race detector
.github/CODEOWNERS                an owner for the kernel, the foundation and, from S1, every context directory
```

A bounded context will live in `internal/contexts/<bc>/` as `module.go`, `api/` and `internal/{domain,application,adapters,integration}`. Other code reaches it through its `api` package only; the compiler rejects imports of its `internal/`, and `tools/dependency-check` rejects every import the layering and the Context Map do not allow.

## Run locally

```
make check        # module verification, formatting, vet, golangci-lint, unit tests, package rules
make db           # start brant-pg16, create roles, migrate, grant (development passwords only)
make integration  # unit and PostgreSQL 16 integration tests; CI=true forbids skipping database tests
make race         # the same with the race detector
make spike        # consistency spikes; JSON report in bin/spike-consistency.json
make ci           # all of the above, as .github/workflows/ci.yml runs them
```

`scripts/dev-postgres.sh` pulls PostgreSQL 16.12 by the digest CI pins. Set `BRANT_PG_IMAGE` to pull the same digest from another registry and `BRANT_PG_PORT` for another port.

Against another PostgreSQL 16:

1. Run `deploy/postgres/00_roles.sql` once as a superuser (it takes the database name and both passwords as psql variables).
2. `IDP_BRANT_MIGRATOR_DSN=... go run ./cmd/brant-migrate` to inspect the plan; add `-apply` to migrate as `brant_migrator`.
3. Run `deploy/postgres/10_app_grants.sql` as `brant_migrator` after every migration run.
4. `IDP_BRANT_POSTGRES_DSN` (as `brant_app`) and `IDP_BRANT_MIGRATOR_DSN` for the integration tests; `IDP_BRANT_POSTGRES_DSN=... go run ./cmd/brant-worker -sink https://...` to relay events.

## Status of evidence

Local runs on 2026-11-07 against the dedicated container (PostgreSQL 16.12, Go 1.26.8 with the 1.26.5 toolchain floor): `make ci` passed. There is no remote repository yet, so the GitHub Actions workflow has not run and branch protection is not configured; both wait for the owner to create the repository.

- `go.sum` is the complete graph from `go mod tidy` and identical to IDP-Cargo's; the Bento `h1:` sum matches the recorded one and `go mod verify` passes.
- Kernel arithmetic reproduces the Python V0.50 vectors (BRC-001 960.00 / 660.00 / 300.00 / 31.25 % / 288.00 + 672.00 / 600.00; per-line rounding 1 + 1 = 2 minor units; 960.00 inclusive of 13 % = 849.56 + 110.44; 100 USD at 7.123456 = 712.35 CNY; payment tail 2 / 1 / 2; tiers 700.00 vs 600.00; margin floor at 880.00 vs 879.99). Two fuzz targets ran 30 s each against independent digit oracles.
- AC4: a context calling another context's transaction owner inside its own transaction gets `ErrTransactionConflict`.
- Migrations: an empty database migrates as the non-superuser `brant_migrator`, a second run applies nothing, an edited applied migration stops the run, a failed migration is not recorded.
- Authorization: AI and system actors never pass a human-only rule; a revocation that commits first denies the command; a command holding the guard row commits first and the revocation waits; FORCE RLS isolates tenants and companies (SQLSTATE 42501 on a foreign row).
- Relay: retry with backoff, abandonment after the failure budget without blocking the partition, SIGKILL crash drill with re-delivery after the lease and graceful SIGTERM.
- Spikes: see `docs/10_M0_ENGINEERING_BASELINE/` for the report. Option A of ADR-BR-004 leaves a commit window; option B (guard function in the downstream transaction) decides every commit order. Without a root lock, replace and convert both win in 200 of 200 races; with the root row locked `FOR UPDATE` first, exactly one wins in every race.

## Important boundaries

- Actor, tenant and company come from a verified token, never from a request body. The OIDC adapter arrives with the HTTP transport in S1; until then `identity.Principal` is only as trustworthy as its caller.
- FORCE RLS on `app.tenant_id` and `app.company_id` protects against a missing or wrong scope in a query. SQL injection that can run `set_config` is outside its boundary.
- Each context has its own Bento transaction owner. A context reads another context before it opens its transaction, or re-checks a qualification through a guard function the upstream context publishes, called with its own executor (ADR-BR-004 option B, pending decision).
- Lock order (ADR-BR-011, pending decision): a command locks its own aggregate root `FOR UPDATE` first, then upstream guard rows `FOR SHARE`; upstream contexts never lock downstream rows.
- The relay delivers at least once; consumers deduplicate by event ID (`Idempotency-Key`). Leases compare wall-clock readings, so worker hosts need clocks that are slewed, not stepped: a WSL2 development host stepped its wall clock during one drill and re-delivered 1.45 s into a 3 s lease.
- A delivery the relay gives up on is abandoned and stored as a dead letter in `brant_msg` in one transaction, then alerted; revocation, repudiation and risk event types alert as critical. A human holding `dead_letter.replay` replays it by event ID with the same event ID, so consumers deduplicate the replay like any redelivery.
- Applied migrations are never edited; every change is a new numbered file.

## Dependency lock qualification

`go.sum` is the complete module graph from `go mod tidy`. CI resolves it with `GOPROXY=direct` and runs `go mod verify` and `go mod tidy -diff`. Promotion to an approved supply-chain baseline still needs review.
