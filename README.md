# Ledgerline

A payments API and double-entry ledger in Go. The point is not the feature count: it is showing
that money stays correct under concurrency, crashes and retries, with tests that prove each
invariant. Amounts are integers in minor units everywhere, the database itself rejects unbalanced
journal entries, every POST is idempotent, and webhooks go out through a transactional outbox.

> **Status: M1 (ledger) done.** The payments API is not built yet. Everything that is planned but
> not yet working is listed under [Not implemented](#not-implemented), and is moved out of that
> list only when its tests pass.

## Milestones

| Milestone | Scope | Status |
|-----------|-------|--------|
| M0 | Layout, Compose, Makefile, CI, migrations, `money` package | done |
| M1 | Ledger schema, posting logic, DB-enforced invariants, property tests | done |
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
make prop-long          # >= 1,000,000 generated ledger operations (fails if fewer ran)
make lint               # go vet with every build tag + golangci-lint
make db-up migrate      # local Postgres in Compose, then apply migrations
make audit              # check ledger invariants in $DATABASE_URL; exits 1 on any violation
docker compose up migrate   # migrate entirely inside Docker
```

`make help` lists every target. `chaos` and `load` exist already, but they exit with an error
naming the milestone they arrive in.

## Tests

The figures below are from runs on an Apple M2 (8 cores, 8 GB) with Go 1.27.4 and Postgres 17.12
(`postgres:17-alpine`). Property-test operation counts change from run to run, because rapid
picks a new random seed each time.

| Suite | Command | Result |
|-------|---------|--------|
| Unit | `go test -race -count=1 ./...` | 30 tests, 96 subtests, 0 failures |
| Integration | `go test -race -count=1 -tags=integration ./test/integration/...` | 15 tests, 27 subtests, 0 failures |
| Ledger property test, in-memory | `make prop-long` (10,000 cases, ~150 steps each) | 1,490,989 operations (730,901 posted, 760,088 rejected) in 20.9s |
| Ledger property test, Postgres | part of the integration suite (100 cases, `-race`) | 2,860 to 3,039 operations per run, ~10.4s |

What each test proves is in [docs/INVARIANTS.md](docs/INVARIANTS.md), which also lists the
deliberately broken builds each key test was shown to catch.

## Docs

- [docs/ledger.md](docs/ledger.md): accounts, postings, a worked example (checked by
  `go test`), why authorization posts nothing, and what the database enforces
- [docs/INVARIANTS.md](docs/INVARIANTS.md): invariant → how it is enforced → which test proves it
- [docs/adr/](docs/adr/): architecture decision records
- [docs/go-notes.md](docs/go-notes.md): Go idioms explained for a C++ programmer

## Layout

| Path | Contents |
|------|----------|
| `cmd/ledgerline` | Main binary: `migrate`, `audit`. Later: `serve`, `dispatch` |
| `internal/money` | `Money` (int64 minor units plus ISO 4217 currency) with checked arithmetic |
| `internal/ledger` | Chart of accounts, posting builders, validation, in-memory and Postgres ledgers, audit |
| `internal/ledger/ledgertest` | Stateful property test (rapid) shared by the in-memory and Postgres runs |
| `internal/id` | Prefixed random IDs (`jtx_…`, `acct_…`) |
| `internal/store` | pgx connection pool, embedded goose migrations |
| `internal/store/storetest` | Disposable Postgres for integration tests (testcontainers) |
| `migrations/` | SQL migrations, embedded into the binary |
| `test/integration` | Tests against real Postgres (`integration` build tag) |

## Not implemented

Planned, but not built yet:

- **Payments API (M2):** endpoints, API-key auth, the state machine, the acquirer simulator,
  idempotency keys, concurrency acceptance tests. The ledger property test covers ledger
  operations today. The payment-lifecycle operations (create, authorize, fail) are added to it
  in M2, together with the payments table and its `refunded ≤ captured ≤ authorized` CHECK.
- Webhooks and the outbox (M3), fraud rules (M4), reconciliation (M5), load tests (M6).

Known gaps in what is built (details in [docs/ledger.md](docs/ledger.md#known-gaps)):

- Journal transactions are not sealed: a *balanced* pair of entries can still be appended to an
  older transaction. An unbalanced append is rejected.
- No separate database roles yet. Triggers protect the ledger from application bugs, but not
  from a table owner or superuser who disables them.
- Refunds after capture leave `processor_receivable` negative; there are no refund settlement
  lines to net them out.
- A merchant whose refunds exceed their balance (fees are kept, see
  [ADR 0002](docs/adr/0002-fees-on-refunds.md)) ends up owing us money. Nothing collects it.
- The currency table is a curated subset of ISO 4217 (28 codes).
