GO ?= go
DATABASE_URL ?= postgres://ledgerline:ledgerline@localhost:5432/ledgerline?sslmode=disable
export DATABASE_URL

.PHONY: help build test test-integration test-concurrency prop-long chaos load audit lint fmt vet db-up db-down migrate

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

build: ## Build all binaries into ./bin
	$(GO) build -o bin/ ./cmd/...

test: ## Unit tests with the race detector (no Docker needed)
	$(GO) test -race ./...

test-integration: ## Integration tests against a real Postgres (testcontainers; needs Docker)
	$(GO) test -race -count=1 -tags=integration ./test/integration/...

CONCURRENCY_COUNT ?= 25
test-concurrency: ## Concurrency acceptance tests, $(CONCURRENCY_COUNT) runs each with -race (needs Docker)
	$(GO) test -race -count=$(CONCURRENCY_COUNT) -tags=integration -run 'TestConcurrency' -v ./test/integration/ \
		| grep -E '^(--- |ok|FAIL|PASS)' | sed -E 's/ \([0-9.]+s\)//' | sort | uniq -c

lint: ## go vet (all build tags) + golangci-lint
	$(GO) vet ./...
	$(GO) vet -tags=integration,chaos ./...
	golangci-lint run ./...

fmt: ## Format code
	gofmt -w .

vet: ## go vet, including tagged test files
	$(GO) vet ./...
	$(GO) vet -tags=integration,chaos ./...

db-up: ## Start Postgres in Docker Compose and wait until it is healthy
	docker compose up -d --wait postgres

db-down: ## Stop Compose services (keeps the data volume)
	docker compose down

migrate: ## Apply migrations to $DATABASE_URL
	$(GO) run ./cmd/ledgerline migrate up

audit: ## Check ledger invariants in $DATABASE_URL (global sum 0 per currency, every transaction balances)
	$(GO) run ./cmd/ledgerline audit

# prop-long: the two pure stateful property tests (ledger operations, and
# the payment lifecycle on top of the ledger), each with enough generated
# cases for >= 1,000,000 operations. LEDGERLINE_PROP_MIN_OPS makes each test
# fail if fewer ran, so the count is checked, not assumed. No -race here: it
# slows the run about 10x and the in-memory code is single-threaded.
PROP_CHECKS ?= 10000
PROP_STEPS ?= 150
prop-long: ## >=1,000,000 generated operations each for the ledger and the payment lifecycle (pure logic)
	LEDGERLINE_PROP_MIN_OPS=1000000 $(GO) test -count=1 -timeout=60m -run 'TestPropLedgerMemory$$' -v ./internal/ledger \
		-args -rapid.checks=$(PROP_CHECKS) -rapid.steps=$(PROP_STEPS)
	LEDGERLINE_PROP_MIN_OPS=1000000 $(GO) test -count=1 -timeout=60m -run 'TestPropPaymentsPure$$' -v ./internal/payments \
		-args -rapid.checks=$(PROP_CHECKS) -rapid.steps=$(PROP_STEPS)

CHAOS_COUNT ?= 1
chaos: ## Crash test: serve/dispatch/receiver as child processes, SIGKILLed at failpoints and at random (needs Docker)
	$(GO) test -tags chaos -count=$(CHAOS_COUNT) -timeout 30m -run TestChaos -v ./test/chaos/ | grep -E '^(=== RUN|--- |\s+chaos_test|ok|FAIL|PASS)'

FLOW_RATES ?= 50 100 200 300 400
REPLAY_RATES ?= 250 500 1000 2000
HOT_RATES ?= 25 50 100 200
load: ## k6: full flow, idempotent replays, one hot payment, each stepped through rising rates (needs Docker, k6)
	docker compose up -d --build --wait serve
	@echo "| scenario | target it/s | achieved it/s | endpoint | req/s | p50 ms | p95 ms | p99 ms | max ms | errors | dropped |"
	@echo "|---|---|---|---|---|---|---|---|---|---|---|"
	@loadtest/run.sh flow $(FLOW_RATES)
	@loadtest/run.sh replay $(REPLAY_RATES)
	@loadtest/run.sh hot $(HOT_RATES)


