# Development entry points; .github/workflows/ci.yml runs the same steps on every push.
#
#   make check        formatting, vet, lint, unit tests, package rules (no database)
#   make db           start the development PostgreSQL 16 (brant-pg16) and migrate it
#   make integration  unit and PostgreSQL 16 integration tests against brant-pg16
#   make race         the same with the race detector
#                     SHARDS=N (default 1): internal/bootstrap in N shards (tools/testshard) and the
#                     other packages, in parallel, each shard against its own database brant_shard_k
#                     in the container on PG_PORT (scripts/test-shards.sh; logs in bin/shards/)
#   make spike        the M0-10 consistency spikes, report in bin/spike-consistency.json
#   make roles        regenerate the role templates from the archive (BC-D02)
#   make load         staged load over the HTTP API against PG_PORT (tests/load; WBS S5-11), report in bin/load/
#   make drill        backup/restore (logical, PITR) and upgrade drills (scripts/backup-restore.sh; WBS S5-11)
#   make ci           everything above (not load and drill)
#   make eer-live     live runs against a running EER (decision E-2; never in CI): the EER adapter's
#                     live tests and the stop barrier of a task on PostgreSQL 16 at PG_PORT; EER and
#                     its credentials come from the environment (internal/ai/adapters/eer/eerlive)
#   make ai-eval      the Q01 evaluation suite with the scripted model (tests/ai-evaluation; WBS S4-07a, BC-AT28)
#                     against its own database AI_EVAL_DB in the container on PG_PORT; report in bin/ai-eval/
#   make ai-eval-live the same with the live model service (build tag aievallive; never in CI): the key comes from
#                     IDP_BRANT_MODEL_API_KEY; without it the run is reported NOT EXECUTED (exit 3). A trial the
#                     model service did not let run is NOT EXECUTED, and a scripted-only family is NOT
#                     APPLICABLE; neither is a pass or a failure, and a completed live run still exits 0

GO ?= go
GOLANGCI_LINT_VERSION ?= v2.15.0
CONTEXT_MAP ?= docs/03_STRATEGIC_DDD/01_IDP_Brant_Strategic_Context_Map_V0.1_Draft.json
PG_PORT ?= 55471
SHARDS ?= 1
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

.PHONY: check fmt fmt-check vet lint test deps allowlist roles modules db integration race spike build ci load drill eer-live ai-eval ai-eval-live

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

SHARD_ENV = CI=true BRANT_PG_PORT=$(PG_PORT) GO='$(GO)'

integration:
ifeq ($(SHARDS),1)
	$(DB_ENV) $(GO) test -tags integration -count=1 -timeout 45m ./...
else
	$(SHARD_ENV) scripts/test-shards.sh all integration $(SHARDS)
endif

race:
ifeq ($(SHARDS),1)
	$(DB_ENV) $(GO) test -tags integration -race -count=1 -timeout 60m ./...
else
	$(SHARD_ENV) scripts/test-shards.sh all race $(SHARDS)
endif

spike:
	@mkdir -p bin
	$(DB_ENV) BRANT_SPIKE_REPORT=$(CURDIR)/bin/spike-consistency.json $(GO) test -tags integration -count=1 -v ./tools/spikes/consistency

# The observer samples pg_stat_activity, pg_locks and the Outbox as the development superuser. Run it
# against a database of its own: its brant-worker -dispatch processes (2 by default, S5R-D07) relay
# every tenant's pending events. LOAD_ARGS=-assert fails the run on a missed target (tests/load/thresholds.go).
load: build
	IDP_BRANT_POSTGRES_DSN='postgres://brant_app:dev-app-only@127.0.0.1:$(PG_PORT)/brant?sslmode=disable' \
	BRANT_LOAD_OBSERVER_DSN='postgres://postgres:dev-superuser-only@127.0.0.1:$(PG_PORT)/brant?sslmode=disable' \
	$(GO) run ./tests/load -bin bin -stages $(LOAD_STAGES) -out bin/load/$$(date -u +%Y%m%dT%H%M%SZ) $(LOAD_ARGS)

# Needs the source container with WAL archiving (BRANT_PG_WAL_ARCHIVE=1 scripts/dev-postgres.sh).
drill: build
	BRANT_PG_CONTAINER=$(PG_CONTAINER) BRANT_PG_PORT=$(PG_PORT) BRANT_RESTORE_CONTAINER=$(RESTORE_CONTAINER) \
	BRANT_RESTORE_PORT=$(RESTORE_PORT) scripts/backup-restore.sh drill

ci: check db integration race spike

# Live EER runs: refused under CI=true; a missing EER setting fails them (never a skip).
eer-live:
	@if [ "$$CI" = "true" ]; then echo "make eer-live never runs under CI=true"; exit 2; fi
	$(GO) vet -tags eerlive ./internal/ai/adapters/eer/...
	$(GO) vet -tags 'integration eerlive' ./internal/bootstrap/
	$(GO) test -tags eerlive -count=1 -v ./internal/ai/adapters/eer/
	IDP_BRANT_MIGRATOR_DSN='postgres://brant_migrator:dev-migrator-only@127.0.0.1:$(PG_PORT)/brant?sslmode=disable' \
	IDP_BRANT_POSTGRES_DSN='postgres://brant_app:dev-app-only@127.0.0.1:$(PG_PORT)/brant?sslmode=disable' \
	$(GO) test -tags 'integration eerlive' -count=1 -v -run '^TestEERLive' ./internal/bootstrap/

# The AI evaluation runs against a database of its own (its brant-worker -dispatch relays every tenant's
# events there), prepared by scripts/pg-prepare.sh in the container that publishes PG_PORT. AI_EVAL_ARGS:
# extra flags of tests/ai-evaluation (-families, -trials, -parallel, -phase-timeout, -trace). The live
# suite writes the agent workers' local diagnostic trace under each trial's directory (-trace auto);
# the scripted suite writes it only with AI_EVAL_ARGS='-trace on'. A trace is not evidence.
AI_EVAL_DB ?= brant_aieval
AI_EVAL_ARGS ?=
AI_EVAL_PREPARE = BRANT_PG_CONTAINER="$$(docker ps --filter publish=$(PG_PORT) --format '{{.Names}}')" BRANT_PG_PORT=$(PG_PORT) \
	BRANT_PG_DB=$(AI_EVAL_DB) scripts/pg-prepare.sh
AI_EVAL_ENV = IDP_BRANT_MIGRATOR_DSN='postgres://brant_migrator:dev-migrator-only@127.0.0.1:$(PG_PORT)/$(AI_EVAL_DB)?sslmode=disable' \
	IDP_BRANT_POSTGRES_DSN='postgres://brant_app:dev-app-only@127.0.0.1:$(PG_PORT)/$(AI_EVAL_DB)?sslmode=disable'

ai-eval: build
	$(GO) build -o bin/ai-evaluation ./tests/ai-evaluation
	$(AI_EVAL_PREPARE)
	$(AI_EVAL_ENV) bin/ai-evaluation -suite scripted -bin bin -out bin/ai-eval/$$(date -u +%Y%m%dT%H%M%SZ) $(AI_EVAL_ARGS)

ai-eval-live: build
	@if [ "$$CI" = "true" ]; then echo "make ai-eval-live never runs under CI=true"; exit 2; fi
	$(GO) build -tags aievallive -o bin/ai-evaluation-live ./tests/ai-evaluation
	$(AI_EVAL_PREPARE)
	$(AI_EVAL_ENV) bin/ai-evaluation-live -suite live -bin bin -out bin/ai-eval/live-$$(date -u +%Y%m%dT%H%M%SZ) $(AI_EVAL_ARGS)
