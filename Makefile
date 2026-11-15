GO ?= go
DATABASE_URL ?= postgres://ledgerline:ledgerline@localhost:5432/ledgerline?sslmode=disable
export DATABASE_URL

.PHONY: help build test test-integration fmt vet db-up db-down migrate

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

build: ## Build all binaries into ./bin
	$(GO) build -o bin/ ./cmd/...

test: ## Unit tests with the race detector (no Docker needed)
	$(GO) test -race ./...

test-integration: ## Integration tests against a real Postgres (testcontainers; needs Docker)
	$(GO) test -race -count=1 -tags=integration ./test/integration/...

fmt: ## Format code
	gofmt -w .

vet: ## go vet, including tagged test files
	$(GO) vet ./...
	$(GO) vet -tags=integration ./...

db-up: ## Start Postgres in Docker Compose and wait until it is healthy
	docker compose up -d --wait postgres

db-down: ## Stop Compose services (keeps the data volume)
	docker compose down

migrate: ## Apply migrations to $$DATABASE_URL
	$(GO) run ./cmd/ledgerline migrate up
