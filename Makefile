# Development entry points; .github/workflows/ci.yml runs the same steps on every push.
#
#   make check        formatting, vet, lint, unit tests, package rules (no database)
#   make db           start the development PostgreSQL 16 (brant-pg16) and migrate it
#   make integration  unit and PostgreSQL 16 integration tests against brant-pg16
#   make race         the same with the race detector
#   make spike        the M0-10 consistency spikes, report in bin/spike-consistency.json
#   make roles        regenerate the role templates from the archive (BC-D02)
#   make ci           everything above

GO ?= go
GOLANGCI_LINT_VERSION ?= v2.15.0
CONTEXT_MAP ?= docs/03_STRATEGIC_DDD/01_IDP_Brant_Strategic_Context_Map_V0.1_Draft.json
PG_PORT ?= 55471
export GOPRIVATE ?= go.idp.xyz

DB_ENV = CI=true \
	IDP_BRANT_MIGRATOR_DSN='postgres://brant_migrator:dev-migrator-only@127.0.0.1:$(PG_PORT)/brant?sslmode=disable' \
	IDP_BRANT_POSTGRES_DSN='postgres://brant_app:dev-app-only@127.0.0.1:$(PG_PORT)/brant?sslmode=disable'

.PHONY: check fmt fmt-check vet lint test deps allowlist roles modules db integration race spike build ci

check: modules fmt-check vet lint test deps

fmt:
	gofmt -w cmd internal tools

fmt-check:
	@out="$$(gofmt -l cmd internal tools)"; if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

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

ci: check db integration race spike
