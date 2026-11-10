# Working in idp-brantgo

This repository is the IDP Brant Go module (`go.idp.xyz/idp-brant`) plus its master archive in `docs/`. Read `README.md` first; the archive entry page is `docs/00_START_HERE/00_INDEX.html` and its rules are in `docs/00_START_HERE/01_README_请先阅读.md`.

## Before you hand back work

- `make check` passes (module verification, gofmt, vet, golangci-lint, unit tests, package rules).
- Code that touches PostgreSQL also passes `make db && make integration`; `CI=true` makes a missing database a failure, never a skip.
- New behaviour has a test at the level it belongs to: kernel and domain rules as unit tests, persistence and concurrency as PostgreSQL 16 integration tests.

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
- Migrations: a new file `migrations/<dir>/NNNNNN_<dir>_<topic>.sql`, numbered without gaps, listed in `migrations/manifest/contexts.txt`; applied files are never edited. Grants for the application role go into `deploy/postgres/10_app_grants.sql`; every table gets FORCE RLS on `app.tenant_id` and `app.company_id`.
- Python V0.50 is behaviour reference, not evidence: port its vectors into Go tests and cite the Python test they come from.

## The archive

- Pages in `docs/` are generated. Edit the sources under `docs/00_START_HERE/tools/` (`pages/*.md`, `data/*.json`) and rebuild with `python3 docs/00_START_HERE/tools/build_archive.py`; the build validates links, counts, manifest and SHA-256 sums.
- Evidence from real runs goes into the archive with the date, the environment and what was not executed.
- `brand/` is the brand kit (visual locked, production master not yet authorized). Never edit, recolor, crop or redraw its files, and never present its SVG traces as source vectors. The archive builder embeds the PNGs it uses byte for byte and checks them against `brand/05_QA/sha256_manifest.json`.
