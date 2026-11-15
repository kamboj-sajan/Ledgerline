GO ?= go

.PHONY: help build test fmt vet

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

build: ## Build all binaries into ./bin
	$(GO) build -o bin/ ./cmd/...

test: ## Unit tests with the race detector
	$(GO) test -race ./...

fmt: ## Format code
	gofmt -w .

vet: ## go vet
	$(GO) vet ./...
