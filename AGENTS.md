# Working in idp-brantgo

This repository is the IDP Brant Go module (`go.idp.xyz/idp-brant`) plus its master archive in `docs/`. Read `README.md` first; the archive entry page is `docs/00_START_HERE/00_INDEX.html` and its rules are in `docs/00_START_HERE/01_README_请先阅读.md`.

## Before you hand back work

- `make check` passes (module verification, gofmt, vet, golangci-lint, unit tests, package rules).
- Code that touches PostgreSQL also passes `make db && make integration`; `CI=true` makes a missing database a failure, never a skip.
- New behaviour has a test at the level it belongs to: kernel and domain rules as unit tests, persistence and concurrency as PostgreSQL 16 integration tests.
- The live suites (`make ai-eval-live`, `make eer-live`) never run under `CI=true`; a missing model key makes the live suite report NOT EXECUTED, never a pass. On the live suite a trial the model service did not let run is NOT EXECUTED and a scripted-only family NOT APPLICABLE, never a pass and never a failure. The model service key is only the environment variable `IDP_BRANT_MODEL_API_KEY`; if it is kept in a file, that file is the git-ignored `.env`, and the key never goes into a report, a log, a trace or the archive.

## Rules the checks enforce

- Layers: `cmd/` imports only `internal/bootstrap`; `internal/kernel` imports only the standard library; `internal/foundation` imports external modules, the kernel and other foundation packages, never a context; a context's `api/` imports only the standard library and the kernel; a domain package imports only the kernel and its own domain.
- A context imports another context's `api/` only when the Context Map makes that context its upstream. After the Context Map changes, run `make allowlist`; never edit `tools/dependency-check/allowlist.txt` by hand.
- Every directory under `internal/contexts/` gets a CODEOWNERS line in the same change.
- `docs/` never holds Go code. Root `tests/` never imports `internal/`.
- `internal/foundation/database/pgtest` is imported by test files only.

## Rules the reviewers enforce

- Money is `kernel.Money` in integer minor units; arithmetic is exact until an explicit HALF_UP rounding point. No `float32`/`float64` in business values, and decimal input goes through `kernel.ParseDecimal` with the contract's format.
- Times that are stored, compared or hashed go through `kernel.Instant` (UTC, microseconds).
- Scope, actor and tenant come from `identity.Principal`; a request body never supplies them. Human-only commands use `authorization.HumanOnly`; commands record the authority epoch `Authorizer.Authorize` returns.
- Each context writes only its own schema `brant_<bc>`, through its own `bentopg.DB` and `bento.ScopeTransactor`. A transaction never calls into another context's transaction owner; it reads upstream state before it begins or through a guard function the upstream context publishes, executed with its own executor.
- Lock order inside a transaction: own aggregate root `FOR UPDATE` first, then upstream guard rows `FOR SHARE`.
- A reverse read in a bidirectional relationship (TRD reading FUL's shipments or PUR's commitments, and PUR reading FUL's shipped quantity by commitment line) is a lock-free read inside the reader's own transaction, after it locked its own root. It is correct only while every writer of those facts calls the reader's guard in its transaction: OrderGuard v1 for the order facts, and SupplierDeliveryGuard v2 (the commitment root FOR SHARE) for confirmed shipment quantity. A new FUL or PUR command that writes order facts calls OrderGuard v1 too; a new FUL command that confirms shipment quantity calls SupplierDeliveryGuard v2 too.
- An event consumer subscribes only to the events of its upstreams in the Context Map (`Subscriptions()`, checked against the allowlist), runs as its own system actor `system:inbox.<bc>.<purpose>` with the permissions `brant-admin inbox` grants, keys its command by the event ID, reads upstream state in `Prepare` before its transaction and never calls an upstream's transaction owner in the handler.
- Migrations: a new file `migrations/<dir>/NNNNNN_<dir>_<topic>.sql`, numbered without gaps, listed in `migrations/manifest/contexts.txt`; applied files are never edited. A new directory goes into the manifest after the directories whose guards its transactions call (S5: `qlt`, `stl`, `cnx`, `ful` after `pur`; S4: `tsk` after its upstreams `trd`, `evd`, `inq`, `sup`, `cst`, and `ai` last); when two branches take the same number, the later merge renumbers its file. Grants for the application role go into `deploy/postgres/10_app_grants.sql`; every table gets FORCE RLS on `app.tenant_id` and `app.company_id`.
- Python V0.50 is behaviour reference, not evidence: port its vectors into Go tests and cite the Python test they come from.
- An AI actor's commands go through the AI gateway in process; every writing HTTP route refuses actor kind `ai` at the transport (PERMISSION_DENIED) before the body is read. A new writing route refuses it too; the route walk in `internal/transport/httpapi` checks it.

## The archive

- Pages in `docs/` are generated. Edit the sources under `docs/00_START_HERE/tools/` (`pages/*.md`, `data/*.json`) and rebuild with `python3 docs/00_START_HERE/tools/build_archive.py`; the build validates links, counts, manifest and SHA-256 sums.
- Evidence from real runs goes into the archive with the date, the environment and what was not executed. From a live model run it takes the run's `summary.md` and `report.json` and says which families ran on the live model and which did not; never a `trace/` directory (local diagnostics, not evidence) or a process log.
- `brand/` is the brand kit (visual locked, production master not yet authorized). Never edit, recolor, crop or redraw its files, and never present its SVG traces as source vectors. The archive builder embeds the PNGs it uses byte for byte and checks them against `brand/05_QA/sha256_manifest.json`.
