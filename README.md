# Ledgerline

A payments API and double-entry ledger in Go. The point is not the feature count: it is showing
that money stays correct under concurrency, crashes and retries, with tests that prove each
invariant. Amounts are integers in minor units everywhere, the database itself rejects unbalanced
journal entries, every POST is idempotent, and webhooks go out through a transactional outbox.

> **Status: M0 (scaffold).** Most of the system is not built yet. Everything that is planned but
> not yet working is listed under [Not implemented](#not-implemented) and is moved out of that
> list only when its tests pass.

## Milestones

| Milestone | Scope | Status |
|-----------|-------|--------|
| M0 | Layout, Compose, Makefile, CI, migrations, `money` package | in progress |
| M1 | Ledger schema, posting logic, DB-enforced invariants, property tests | not started |
| M2 | Payments API, auth, state machine, acquirer simulator, idempotency, concurrency tests | not started |
| M3 | Outbox, webhook dispatcher, signing, receiver, retries, chaos tests | not started |
| M4 | Fraud rules engine, evaluation harness and report | not started |
| M5 | Bank settlement simulator and reconciliation | not started |
| M6 | Load tests, benchmarks, docs | not started |

## Requirements

- Go 1.27+
- Docker (Compose v2) for Postgres and the integration tests
- golangci-lint v2 for `make lint`

## Development

```sh
make test               # unit tests with -race (no Docker)
make test-integration   # Postgres via testcontainers (needs Docker)
make lint               # go vet with every build tag + golangci-lint
make db-up migrate      # local Postgres in Compose, then apply migrations
docker compose up migrate   # same, entirely inside Docker
```

`make help` lists every target. Targets for later milestones (`prop-long`, `audit`, `chaos`,
`load`) exist already but exit with an error saying which milestone they arrive in.

## Layout

| Path | Contents |
|------|----------|
| `cmd/ledgerline` | Main binary. Today: `migrate`. Later: `serve`, `dispatch` |
| `internal/money` | `Money` (int64 minor units plus ISO 4217 currency) with checked arithmetic |
| `internal/store` | pgx connection pool, embedded goose migrations |
| `internal/store/storetest` | Disposable Postgres for integration tests (testcontainers) |
| `migrations/` | SQL migrations, embedded into the binary |
| `test/integration` | Tests against real Postgres (`integration` build tag) |
| `docs/go-notes.md` | Go idioms explained for a C++ programmer |

## Not implemented

Everything from M1 onwards: the ledger, payments API, idempotency, webhooks, fraud rules,
reconciliation and load tests. This section is kept accurate as milestones land.
