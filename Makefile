GO ?= go
DATABASE_URL ?= postgres://ledgerline:ledgerline@localhost:5432/ledgerline?sslmode=disable
export DATABASE_URL

.PHONY: help build test test-integration prop-long chaos load audit lint fmt vet db-up db-down migrate

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

build: ## Build all binaries into ./bin
	$(GO) build -o bin/ ./cmd/...

test: ## Unit tests with the race detector (no Docker needed)
	$(GO) test -race ./...

test-integration: ## Integration tests against a real Postgres (testcontainers; needs Docker)
	$(GO) test -race -count=1 -tags=integration ./test/integration/...

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

migrate: ## Apply migrations to $$DATABASE_URL
	$(GO) run ./cmd/ledgerline migrate up

# The targets below belong to later milestones. Until then they fail loudly
# instead of passing with nothing to run.

prop-long: ## >=1,000,000 generated operations against the pure posting logic (M1)
	@echo "prop-long: not implemented yet (lands in M1, ledger)" >&2; exit 1

audit: ## Ledger audit SQL: global sum is 0 per currency, every transaction balances (M1)
	@echo "audit: not implemented yet (lands in M1, ledger)" >&2; exit 1

chaos: ## Fault-injection tests with SIGKILL/restart and failpoints (M3)
	@echo "chaos: not implemented yet (lands in M3, webhooks)" >&2; exit 1

load: ## k6 load test scenarios (M6)
	@echo "load: not implemented yet (lands in M6, load tests)" >&2; exit 1
