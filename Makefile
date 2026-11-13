# Development entry points; .github/workflows/ci.yml runs the same steps on every push.
#
#   make check        formatting, vet, lint, unit tests, package rules (no database)
#   make db           start the development PostgreSQL 16 (brant-pg16) and migrate it
#   make integration  unit and PostgreSQL 16 integration tests against brant-pg16
#   make race         the same with the race detector
#   make spike        the M0-10 consistency spikes, report in bin/spike-consistency.json
#   make roles        regenerate the role templates from the archive (BC-D02)
#   make load         staged load over the HTTP API against PG_PORT (tests/load; WBS S5-11), report in bin/load/
#   make drill        backup/restore (logical, PITR) and upgrade drills (scripts/backup-restore.sh; WBS S5-11)
#   make ci           everything above (not load and drill)

GO ?= go
GOLANGCI_LINT_VERSION ?= v2.15.0
CONTEXT_MAP ?= docs/03_STRATEGIC_DDD/01_IDP_Brant_Strategic_Context_Map_V0.1_Draft.json
PG_PORT ?= 55471
export GOPRIVATE ?= go.idp.xyz

# make load and make drill: stages, extra flags of tests/load, and the drill's restore target.
LOAD_STAGES ?= 1,2,4,8,16,32
LOAD_ARGS ?=
RESTORE_PORT ?= $(shell expr $(PG_PORT) + 1)
PG_CONTAINER ?= brant-pg16
RESTORE_CONTAINER ?= $(PG_CONTAINER)-restore

DB_ENV = CI=true \
	IDP_BRANT_MIGRATOR_DSN='postgres://brant_migrator:dev-migrator-only@127.0.0.1:$(PG_PORT)/brant?sslmode=disable' \
	IDP_BRANT_POSTGRES_DSN='postgres://brant_app:dev-app-only@127.0.0.1:$(PG_PORT)/brant?sslmode=disable'

.PHONY: check fmt fmt-check vet lint test deps allowlist roles modules db integration race spike build ci load drill

check: modules fmt-check vet lint test deps

fmt:
	gofmt -w cmd internal tools tests

fmt-check:
	@out="$$(gofmt -l cmd internal tools tests)"; if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...
	$(GO) vet -tags integration ./...

lint:
	GOFLAGS= $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

test:
	$(GO) test -count=1 ./...

deps:
	$(GO) run ./tools/dependency-check

allowlist:
	$(GO) run ./tools/dependency-check -generate $(CONTEXT_MAP)

roles:
	$(GO) run ./tools/rolegen

modules:
	$(GO) mod verify
	$(GO) mod tidy -diff

build:
	$(GO) build -o bin/ ./cmd/...

db:
	BRANT_PG_PORT=$(PG_PORT) scripts/dev-postgres.sh

integration:
	$(DB_ENV) $(GO) test -tags integration -count=1 -timeout 45m ./...

race:
	$(DB_ENV) $(GO) test -tags integration -race -count=1 -timeout 60m ./...

spike:
	@mkdir -p bin
	$(DB_ENV) BRANT_SPIKE_REPORT=$(CURDIR)/bin/spike-consistency.json $(GO) test -tags integration -count=1 -v ./tools/spikes/consistency

# The observer samples pg_stat_activity, pg_locks and the Outbox as the development superuser. Run it
# against a database of its own: its brant-worker -dispatch relays every tenant's pending events.
load: build
	IDP_BRANT_POSTGRES_DSN='postgres://brant_app:dev-app-only@127.0.0.1:$(PG_PORT)/brant?sslmode=disable' \
	BRANT_LOAD_OBSERVER_DSN='postgres://postgres:dev-superuser-only@127.0.0.1:$(PG_PORT)/brant?sslmode=disable' \
	$(GO) run ./tests/load -bin bin -stages $(LOAD_STAGES) -out bin/load/$$(date -u +%Y%m%dT%H%M%SZ) $(LOAD_ARGS)

# Needs the source container with WAL archiving (BRANT_PG_WAL_ARCHIVE=1 scripts/dev-postgres.sh).
drill: build
	BRANT_PG_CONTAINER=$(PG_CONTAINER) BRANT_PG_PORT=$(PG_PORT) BRANT_RESTORE_CONTAINER=$(RESTORE_CONTAINER) \
	BRANT_RESTORE_PORT=$(RESTORE_PORT) scripts/backup-restore.sh drill

ci: check db integration race spike
