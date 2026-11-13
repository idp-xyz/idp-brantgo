# IDP Brant · Go + DDD + Bento-Go · S1–S3 and S5 done, S4 next

**IDP Brant is one product repository and one Go module. Business code will be organized by DDD bounded context; IDP-Bento-Go stays a separate repository referenced as a Go module version.**

- Product module: `go.idp.xyz/idp-brant` (candidate; the `go.idp.xyz` vanity host serves only `idp-bento-go` so far).
- Framework module: `go.idp.xyz/idp-bento-go@v0.1.0-rc.2` (release candidate), resolved from its public repository.
- Go `1.26.5` (`toolchain` directive); PostgreSQL 16.
- Repository: `github.com/idp-xyz/idp-brantgo`, public (moved from the personal account `idpxyz` to the `idp-xyz` organization on 2026-11-11; links under the old name redirect).
- The master archive (DDD, contracts, plans, gates, ADRs) lives in `docs/` and is published with the repository. `docs/00_START_HERE/00_INDEX.html` is its entry page.

M0 built the engineering baseline. S1 (master data and inquiry) is done: EVD, PTY with party screening, trading profiles, address roles and service roles, CAT with item-code mappings, INQ, POL (all eighteen configuration kinds and ConfigGuard) and the screening lists of reference data run on PostgreSQL 16 behind an HTTP API that accepts OIDC access tokens. Role templates with separation of duties are generated from the archive, and the candidate OpenAPI document is generated from the transport. `brant-cli foundation-demo` publishes a synthetic foundation. S2 (sourcing and costing) is done too: SUP supply conditions with SupplyConditionGuard v1, and CST costing contexts, measurements, deterministic scenario evaluation, frozen cost snapshots, source denial and AdoptabilityGuard v1; T02's 660.00 USD baseline freezes on PostgreSQL 16. S3 (quotes, acceptance, orders, purchase drafts) is done as well: TRD quotes freeze with the server calculation and a business hash, pass an independent review (with margin exceptions), are formalized with a number, document bytes and commit evidence, and become contracts when a verified acceptance arrives in time under the governing law's rule; sales orders activate after an independent review acknowledges every upstream risk and move through hashed revisions by amendment; PUR prepares purchase drafts from active order lines against a demand allocation. BRC-001 runs end to end over HTTP on PostgreSQL 16 (quote 960.00, order 960.00, purchase draft 600.00). S5 (the B0 pilot extension, run before S4 by the owner's decision) is done: four new contexts, QLT (inspections and quality releases by our own QC), STL (customer credit and CreditGuard, fund records and applications, order accounts and PaymentConditionGuard, settlement statements with business receivables and payables, credit notes, reversals and cross-currency applications, charges and the order operating result, opening balances), CNX (trade compliance and TradeComplianceGuard, export licences, customs declarations, purchase invoices, tax refund cases, certificates, document bundles and external reconciliation, all as manual paths) and FUL (shipments with the order fulfilment root, milestones, export lots, the commercial release, bookings, shipment documents and insurance); PUR purchase commitments, supplier direct deliveries and commitment changes; TRD activation and amendment behind STL's credit and CNX's trade compliance guards, and order ends and amendments on FUL's and PUR's facts; CAT declaration profiles; the Inbox dispatcher (S4-06a) carries order events to STL, CNX, PUR and FUL. BRC-001 runs over HTTP from the active order to shipment, documents, settlement, write-off and China materials (operating result 300.00 USD, 31.25 %). External professional systems are manual paths and credit, compliance, licence and tax data are synthetic; G3 and G4 are NO-GO (simulated review). The connection to a real identity provider is deferred to the B0 pilot gate. S4 (business tasks and AI-native operation) is next.

## Layout

```
cmd/brant-api/                    HTTP API: S1 command routes and audited reads behind OIDC access tokens
cmd/brant-admin/                  operator-only grants and roles: authority row, permissions, data scope, role definitions and assignments
cmd/brant-cli/                    local operations and demo: foundation-demo publishes synthetic POL configurations in a fresh company
cmd/brant-migrate/                operator-only migrations: Bento schema, framework migration, then each manifest directory in order
cmd/brant-worker/                 Outbox relay: claim, publish to an HTTP sink or (-dispatch) to the contexts' subscriptions
                                  in process, settle; at least once; lag alerts and GET /metrics (S5R-D07)
internal/bootstrap/               composition root: one connection pool, a Bento transaction owner per context and for the relay
internal/contexts/evd/            EVD: text sources (content, origin, import) and business files, immutable; SourceRef lookup
                                  and file extracts; consumer of TRD's formal document events, filing them as business files (R65)
internal/contexts/pty/            PTY: parties with role sets and contacts; confirmed revisions kept; PartyRef lookup;
                                  address roles, service roles, trading profiles; party screening with positive and negative
                                  lists and PartyScreeningGuard v1; TradingProfileGuard v1 and AddressRoleGuard v1 for TRD;
                                  PartyGuard v1 for PUR
internal/contexts/cat/            CAT: SKUs and specification revisions, item-code mappings; SkuRef and ItemCodeMappingRef lookups;
                                  SkuGuard v1 for TRD
internal/contexts/inq/            INQ: customer RFQs draft, ready, cancelled, closed; RfqLineRef and RfqRef (with customer and
                                  contact) lookups, RfqGuard v1
internal/contexts/sup/            SUP: supplier RFQs; supplier offers, purchase terms and logistics offers in sealed
                                  revisions (verify/publish independently, supersede, withdraw/retire);
                                  SupplyConditionRef lookup and SupplyConditionGuard v1
internal/contexts/pol/            POL: twenty configuration kinds (S3 adds legal_terms with the tenant's default, S3-17,
                                  and order_document_template, S3-15) with independent publication, ConfigGuard v1, FX,
                                  tax and unit lookups, document numbers,
                                  foundation snapshots
internal/contexts/cst/            CST: costing contexts and measurements in sealed revisions, the calculation rules, cost
                                  scenarios with deterministic evaluations, frozen cost snapshots (once per scenario),
                                  source attestations and denials; CostSnapshotRef lookup and AdoptabilityGuard v1
internal/contexts/trd/            TRD (S3; S5 wiring): QuoteRoot working drafts, the human freeze with the server calculation
                                  (calc), its commercial document and business hash (Python protocol.py, golden vectors),
                                  internal metadata, the root's object control and control epoch fence; review requests,
                                  approvals with revocation and margin exceptions; activation and replacement with the
                                  quote-text/0.1.0 formal document, POL number and commit evidence in one transaction,
                                  withdrawal, cancellation, risk holds, the conversion slot; QuoteRevisionGuard v1;
                                  customer responses, their verification into acceptances that form the contract when
                                  they arrived before expiry, the acceptance effectivity ledger; AcceptanceGuard v1;
                                  the read side: quote_ready, paged quote, approval, document, commit and Outbox
                                  histories, dispatch preflight and target, reconciliation and replays that re-check
                                  the caller's grant, data scope and EVD sources;
                                  sales orders: preparation taking the conversion slot, review with risk
                                  acknowledgement, activation re-checking the source and upstream (STL/CNX extension
                                  points not wired), cancellation in draft; ActiveOrderRef v1 and OrderGuard v1 for PUR;
                                  active orders: amendments (prepare, submit, independent decision, apply) that move the
                                  order to its next hashed order revision, so OrderGuard v1 refuses the old binding;
                                  cancellation, short closing and closing (FUL and PUR facts not wired); the S/C and PI
                                  of an order revision (order-text/0.1.0 with the source quote's legal terms, POL
                                  template and number, bytes and commit evidence in one transaction, stale once the
                                  order is amended) and their signed and countersigned copies (EVD files)
internal/contexts/pur/            PUR (S3, S5): purchase drafts binding active order lines to chosen supply condition
                                  lines (immutable, hashed, not committed; server calculation of MOQ, multiples, tiers, tax
                                  and delivery; costing comparison), the demand allocation ledger per order line (several
                                  drafts and suppliers, never above the line's quantity), marked overbuys, purchase.draft.void;
                                  purchase commitments (S5-01): request, an independent decision that recomputes the draft and
                                  acknowledges deviations and overbuys, revocation, issue with a POL purchase order number
                                  and evidence, supplier confirmations; DownstreamCommitments v1 for TRD, InspectionSubject v1
                                  for QLT; supplier direct deliveries (S5-15): claims recorded by a human or an AI against
                                  issued commitment lines, verified by an independent human, corrections that supersede,
                                  never above what a line buys; SupplierDeliveryGuard v1 for FUL, PurchaseSettlementSource v1
                                  for STL, PurchaseLineRef v1 for CNX; commitment changes and cancellation (S5-19): the next
                                  revision prepared, decided independently (recomputed, deviations acknowledged) and applied
                                  while its binding holds, increases within the allocation, never below verified deliveries
                                  (short receipt = fulfilled), cancellation keeping what was verified delivered;
                                  S5R-04: dispositions of delivered goods (requested by the purchaser, approved by an
                                  independent order reviewer) that DownstreamCommitments v1 counts as settled, decrease-only
                                  changes without the supply condition's validity, no cancellation over a pending claim
internal/contexts/qlt/            QLT (S5): inspections of purchase commitment lines (external agency reports as evidence,
                                  re-inspection supersedes), dispositions, quality releases by our own QC and their
                                  revocation; QualityReleaseGuard v1 for FUL
internal/contexts/stl/            STL (S5): customer credit limits, reservations, decisions by tier and CreditGuard v1 for
                                  TRD; fund records with unexplained differences, applications, order accounts and
                                  PaymentConditionGuard v1 for FUL; settlement statements, receivables and payables;
                                  credit notes, reversals, cross-currency applications at a settlement rate; charges,
                                  allocations and the order operating result; opening balances; payment plan reviews;
                                  consumers of TRD's order events
internal/contexts/cnx/            CNX (S5): trade compliance checks and TradeComplianceGuard v1 for TRD and FUL; export
                                  licences and ExportLicenseGuard v1; customs declarations with receipts as recorded,
                                  purchase invoices, tax refund cases and opening refund material, certificates,
                                  document bundles and external reconciliation (manual paths, CN-M01); consumer of order
                                  amendments, regulation publications and programme imports
internal/contexts/ful/            FUL (S5): shipments, packing and the order fulfilment root; ShipmentFacts v1 for TRD;
                                  milestones and export lots (ShipmentLineRef v1, ExportLotRef v1); the commercial
                                  release and its payment-condition exception; bookings, shipment documents rendered by
                                  shipment-text/0.1.0 and reconciled before issue; insurance; shipment reviews
internal/reference/               reference data, not a context: screening lists and country programme lists imported
                                  under control (brant_ref)
internal/transport/httpapi/       HTTP transport: command envelope, ingress limits, category-to-status mapping, OpenAPI
internal/kernel/                  value objects shared by every context; standard library only
                                  Decimal (math/big), Money in minor units, largest-remainder allocation, tax splits,
                                  ExchangeRate, Quantity, Ratio, ValidityInterval, timestamps, Scope, ActorRef,
                                  Rejection (error codes and categories), AuditEntry
internal/foundation/bento/        ScopeTransactor (app.tenant_id and app.company_id per transaction), SystemClock
internal/foundation/database/     PostgreSQL 16 pool with server and application-role checks; pgtest for integration tests
internal/foundation/action/       command ledger: idempotent action results, replay, reconciliation, audit and Outbox in one transaction
internal/foundation/identity/     Principal (actor and scope from a verified token) in the Go context
internal/foundation/identity/oidc/ OIDC access-token verifier (discovery, JWKS, iss, aud, exp, tenant, company, actor kind)
internal/foundation/authorization/ command rules (human-only commands), authority guard, grants, data scope, role templates
                                  and separation of duties in brant_auth
internal/foundation/messaging/    Outbox relay, HTTP webhook publisher, dead letters with alerts and human replay (ADR-BR-015);
                                  Inbox consumer that deduplicates by event ID; subscriptions the contexts declare and
                                  the Dispatcher that delivers to them in process, each in its context's own transaction
internal/foundation/strictjson/   closed-structure request decoding (C7): 1 MiB, 64 levels, 100,000 values; no duplicate keys, null, floats, unknown fields
migrations/manifest/contexts.txt  order of migration directories
migrations/auth/                  brant_auth: authority guard row per scope, local authorization projection (FORCE RLS)
migrations/msg/                   brant_msg: dead letters of the Outbox relay (FORCE RLS)
migrations/ref/                   brant_ref: screening and country programme list versions and entries, immutable (FORCE RLS)
migrations/{evd,pty,cat,pol,inq,sup,cst,trd,pur,qlt,stl,cnx,ful}/ brant_<bc>: each context's tables and command ledger (FORCE RLS, immutability triggers)
deploy/postgres/                  roles (brant_migrator, brant_app) and application grants
scripts/dev-postgres.sh           development PostgreSQL 16 container brant-pg16 on 127.0.0.1:55471 (optional WAL archiving)
scripts/pg-prepare.sh             one database ready for the tests: roles, migrations (twice: idempotent), grants; CI and make
scripts/test-shards.sh            internal/bootstrap in shards, the other packages beside them; CI jobs and make SHARDS=N
scripts/backup-restore.sh         logical, PITR and upgrade drills with consistency checks (make drill)
tests/load/                       black-box load generator over the HTTP API with a local OIDC issuer (make load); 2 dispatch
                                  workers by default and the S5R-D07 / S5-11 threshold checks (-assert)
ops/runbook-outbox-relay.md       operations runbook: relay latency targets and alerts (S5R-D07), metrics, lag, dead letters
contracts/openapi/                candidate OpenAPI 3.1 document, generated by tools/openapigen
tools/dependency-check/           package rules; cross-context allowlist generated from the Context Map
tools/rolegen/ tools/openapigen/  generators: role templates from the archive, OpenAPI from the transport
tools/spikes/consistency/         M0-10 spikes for ADR-BR-004 and ADR-BR-011 (integration tests)
tools/testshard/                  splits a package's tests into shards by a hash of their names; proves partition and coverage
.github/workflows/ci.yml          required check brant-ci: static checks, migrations, PG16 tests and race detector in parallel jobs
.github/CODEOWNERS                an owner for the kernel, the foundation and, from S1, every context directory
```

A bounded context lives in `internal/contexts/<bc>/` as `module.go`, `api/` and `internal/{domain,application,adapters,integration}`. Other code reaches it through its `api` package only; the compiler rejects imports of its `internal/`, and `tools/dependency-check` rejects every import the layering and the Context Map do not allow.

## Run locally

```
make check        # module verification, formatting, vet, golangci-lint, unit tests, package rules
make db           # start brant-pg16, create roles, migrate, grant (development passwords only)
make integration  # unit and PostgreSQL 16 integration tests; CI=true forbids skipping database tests
make race         # the same with the race detector
make spike        # consistency spikes; JSON report in bin/spike-consistency.json
make ci           # all of the above, as .github/workflows/ci.yml runs them
```

`internal/bootstrap` holds almost all of the suite's time: about 600 top-level tests that run one after another. `SHARDS=N` splits it into N shards and runs them in parallel, each against its own database, with the other packages beside them:

```
make integration PG_PORT=55471 SHARDS=4   # 4 databases brant_shard_1..4 in the container on PG_PORT, prepared like make db
make race PG_PORT=55471 SHARDS=4          # the same with the race detector
```

The container is the one that publishes `PG_PORT` on the host (`make db PG_PORT=...` creates it); the shards' databases stay in it and are reused. The run prints one line per shard and one for the other packages (exit status, wall time, log in `bin/shards/<integration|race>/`), and then proves from the logs that every test of `internal/bootstrap` passed exactly once, in its own shard. It fails if a shard or the other packages fail, or a test is missing. `SHARDS=1`, the default, runs `go test ./...` against `brant` as before.

A test's shard is fixed by its name: of N shards, `1 + ⌊h·N / 2⁶⁴⌋`, where `h` is the first 8 bytes of the SHA-256 of the name, so a new test needs no configuration and no test moves when others are added. `tools/testshard` answers the questions around it:

```
go run ./tools/testshard -shards 4 -check -tags integration ./internal/bootstrap           # every test in exactly one shard
go run ./tools/testshard -shards 4 -shard 2 -names -tags integration ./internal/bootstrap  # the tests of shard 2
go run ./tools/testshard -shards 4 -shard 2 -tags integration ./internal/bootstrap         # its -run expression
scripts/test-shards.sh shard integration 2 4   # shard 2 of 4 against IDP_BRANT_*_DSN, as a CI job runs it
```

CI runs the same commands as parallel jobs, each with its own PostgreSQL 16 (`.github/workflows/ci.yml`): `static` (modules, formatting, vet with both tag sets, golangci-lint, package rules), `bootstrap k/N` and `bootstrap race k/N` (the shards; N is the length of each matrix's shard list), `other packages` (every other package plain and with the race detector, the load smoke test and the consistency spike report), and `brant-ci`, which needs all of them and passes only when every one succeeded. `main` requires `brant-ci`.

Serving the API locally needs an OIDC issuer whose access tokens carry `brant_tenant`, `brant_company`, `brant_actor_kind` (`human`, `ai` or `system`) and `sub`:

```
IDP_BRANT_POSTGRES_DSN=... go run ./cmd/brant-admin grant -tenant t -company c -actor alice -kind human \
    -permissions source.register,evd.read,party.save_draft,party.confirm,pty.read,rfq.save_draft,rfq.confirm_scope,inq.read
IDP_BRANT_POSTGRES_DSN=... go run ./cmd/brant-api -oidc-issuer https://issuer.example -oidc-audience brant-api
```

`-replay-sink https://...` adds `POST /api/brant/v1/msg/dead_letters/<source>/<event_id>/replay` for humans holding `dead_letter.replay`; `-replay-dispatch` replays to the contexts' own consumers instead, for a worker that runs `-dispatch`. Request bodies are limited to 1 MiB, except `file.register` (15 MiB, for a 10 MiB file in Base64).

`scripts/dev-postgres.sh` pulls PostgreSQL 16.12 by the digest CI pins. Set `BRANT_PG_IMAGE` to pull the same digest from another registry and `BRANT_PG_PORT` for another port.

Against another PostgreSQL 16:

1. Run `deploy/postgres/00_roles.sql` once as a superuser (it takes the database name and both passwords as psql variables).
2. `IDP_BRANT_MIGRATOR_DSN=... go run ./cmd/brant-migrate` to inspect the plan; add `-apply` to migrate as `brant_migrator`.
3. Run `deploy/postgres/10_app_grants.sql` as `brant_migrator` after every migration run.
4. `IDP_BRANT_POSTGRES_DSN` (as `brant_app`) and `IDP_BRANT_MIGRATOR_DSN` for the integration tests; `IDP_BRANT_POSTGRES_DSN=... go run ./cmd/brant-worker -sink https://...` to relay events, or `-dispatch` to deliver them in process to the subscriptions of the contexts (each consumer's system actor `system:inbox.<consumer>` needs `go run ./cmd/brant-admin inbox -tenant t -company c` once per company). Run at least two `-dispatch` workers; `-metrics-listen 127.0.0.1:9464` serves their relay metrics, and `ops/runbook-outbox-relay.md` covers lag and dead letters.

## Status of evidence

S5, 2026-11-12 to 11-13 (before S4, by the owner's decision): every unit passed `make check` and `make integration` (CI=true) on its own PostgreSQL 16 container; after each merge into `s5/business` the lead ran `make check`, and per merge batch the migrations, `make integration` and `make race`; GitHub Actions ran on every pushed batch. The BRC-001 chain from order to settlement ran twice with identical results after normalising IDs, hashes and instants. Load, backup, PITR and upgrade drills ran on one WSL2 host only (frozen targets: reads P95 500 ms, commands P95 800 ms at ≤ 32 concurrent command users per API instance, RPO 15 min with continuous WAL archiving, RTO 4 h by PITR). The S5 report and its evidence are in `docs/09_IMPLEMENTATION_PLAN/` (12 and 13).

S1, 2026-11-10: GitHub Actions runs the workflow on this repository (then the private `idpxyz/idp-brantgo`, public as `idp-xyz/idp-brantgo` since 2026-11-11); every branch passes it (formatting, vet, lint, package rules, migrations on an empty PostgreSQL 16.12, unit and integration tests without skips, race detector, spikes). `main` is a protected branch since 2026-11-11 (owner decision, once the repository was public): a change reaches it only through a pull request whose `brant-ci` check passed, for administrators too; force pushes and deletion are refused. Reviews and signed provenance are not required (G1 control C09 stays partial by that decision). The S1 report and its evidence are in `docs/09_IMPLEMENTATION_PLAN/`.

- The command ledger orders a command as the Python service does (contract, permission, stored result, CAS and state, references) and commits the change, the action result, the audit entry and the Outbox events together; replays re-check the current permission and visibility.
- 53 Python V0.50 tests of T01 and the shared foundation are ported (08 stage mapping); the front of BRC-001 (four lines, 120 PCS, ready) replays on PostgreSQL 16 directly and over HTTP with OIDC access tokens.
- `go.sum` adds one module for S1: `github.com/go-jose/go-jose/v4` (JWS verification of access tokens).

M0, local runs on 2026-11-07 against the dedicated container (PostgreSQL 16.12, Go 1.26.8 with the 1.26.5 toolchain floor): `make ci` passed. There is no remote repository yet, so the GitHub Actions workflow has not run and branch protection is not configured; both wait for the owner to create the repository.

- `go.sum` is the complete graph from `go mod tidy` and identical to IDP-Cargo's; the Bento `h1:` sum matches the recorded one and `go mod verify` passes.
- Kernel arithmetic reproduces the Python V0.50 vectors (BRC-001 960.00 / 660.00 / 300.00 / 31.25 % / 288.00 + 672.00 / 600.00; per-line rounding 1 + 1 = 2 minor units; 960.00 inclusive of 13 % = 849.56 + 110.44; 100 USD at 7.123456 = 712.35 CNY; payment tail 2 / 1 / 2; tiers 700.00 vs 600.00; margin floor at 880.00 vs 879.99). Two fuzz targets ran 30 s each against independent digit oracles.
- AC4: a context calling another context's transaction owner inside its own transaction gets `ErrTransactionConflict`.
- Migrations: an empty database migrates as the non-superuser `brant_migrator`, a second run applies nothing, an edited applied migration stops the run, a failed migration is not recorded.
- Authorization: AI and system actors never pass a human-only rule; a revocation that commits first denies the command; a command holding the guard row commits first and the revocation waits; FORCE RLS isolates tenants and companies (SQLSTATE 42501 on a foreign row).
- Relay: retry with backoff, abandonment after the failure budget without blocking the partition, SIGKILL crash drill with re-delivery after the lease and graceful SIGTERM.
- Spikes: see `docs/10_M0_ENGINEERING_BASELINE/` for the report. Option A of ADR-BR-004 leaves a commit window; option B (guard function in the downstream transaction) decides every commit order. Without a root lock, replace and convert both win in 200 of 200 races; with the root row locked `FOR UPDATE` first, exactly one wins in every race.

## Important boundaries

- Actor, tenant and company come from a verified OIDC access token, never from a request body. The verifier requires an explicit actor kind claim; a missing kind is never taken as human. No real identity provider has been connected yet; the tests use a local issuer.
- FORCE RLS on `app.tenant_id` and `app.company_id` protects against a missing or wrong scope in a query. SQL injection that can run `set_config` is outside its boundary.
- Each context has its own Bento transaction owner. A context reads another context before it opens its transaction, or re-checks a qualification through a guard function the upstream context publishes, called with its own executor (ADR-BR-004 option B, accepted in the simulated review). INQ reads PTY, CAT and EVD before its transaction (no guard lock on R01–R03).
- Lock order (ADR-BR-011, accepted in the simulated review): a command locks its own aggregate root `FOR UPDATE` first, then upstream guard rows `FOR SHARE`; upstream contexts never lock downstream rows.
- The relay delivers at least once; consumers deduplicate by event ID (`Idempotency-Key`). Leases compare wall-clock readings, so worker hosts need clocks that are slewed, not stepped: a WSL2 development host stepped its wall clock during one drill and re-delivered 1.45 s into a 3 s lease.
- A context subscribes only to the events of its upstreams in the Context Map (rule D1; a unit test checks every subscription against the generated allowlist). With `brant-worker -dispatch` each event reaches every subscribed consumer through the Inbox in the consumer context's own transaction, as `system:inbox.<consumer>`; the relay's partitions keep the events of one aggregate in order, a consumer's rejection dead-letters the event, and an event nobody subscribes to is settled as delivered.
- A delivery the relay gives up on is abandoned and stored as a dead letter in `brant_msg` in one transaction, then alerted; revocation, repudiation and risk event types alert as critical. A human holding `dead_letter.replay` replays it by event ID with the same event ID, so consumers deduplicate the replay like any redelivery.
- Applied migrations are never edited; every change is a new numbered file.

## Dependency lock qualification

`go.sum` is the complete module graph from `go mod tidy`; S1 added `github.com/go-jose/go-jose/v4`, so it is no longer identical to IDP-Cargo's. S4 added `github.com/digitorus/pdf` v0.3.0 (BSD-3-Clause, no dependencies of its own) for the PDF object layer of the restricted file extraction; only `internal/ai/ingest/pdftext` imports it (decision E-8, reviewed in the S4 A6 report). CI resolves it with `GOPROXY=direct` and runs `go mod verify` and `go mod tidy -diff`. Promotion to an approved supply-chain baseline still needs review.
