# Ledgerline

A payments API and double-entry ledger in Go. The point is not the feature count: it is showing
that money stays correct under concurrency, crashes and retries, with tests that prove each
invariant. Amounts are integers in minor units everywhere, the database itself rejects unbalanced
journal entries, every POST is idempotent, and webhooks go out through a transactional outbox.

> **Status: M3 (webhooks) done.** Fraud rules and reconciliation are not built yet. Everything that is planned but not yet working is listed under
> [Not implemented](#not-implemented), and is moved out of that list only when its tests pass.

## Quickstart

```sh
docker compose up -d --build --wait serve dispatch   # Postgres, migrations, the API on :8080, the webhook dispatcher
KEY=$(docker compose run --rm -T serve merchant create -name "Demo Shop" | jq -r .api_key)
H=(-H "Authorization: Bearer $KEY" -H "Content-Type: application/json")

PID=$(curl -s "${H[@]}" -X POST localhost:8080/v1/payments -H "Idempotency-Key: order-1001" \
  -d '{"amount": 2500, "currency": "USD", "customer_id": "cus_42",
       "card": {"fingerprint": "fp_visa_4242", "country": "US"}, "ip_country": "US"}' | jq -r .id)

curl -s "${H[@]}" -X POST localhost:8080/v1/payments/$PID/authorize -H "Idempotency-Key: auth-1001"
curl -s "${H[@]}" -X POST localhost:8080/v1/payments/$PID/capture   -H "Idempotency-Key: cap-1001"   # fee 103
curl -si "${H[@]}" -X POST localhost:8080/v1/payments/$PID/capture  -H "Idempotency-Key: cap-1001"   # Idempotent-Replayed: true
curl -s "${H[@]}" -X POST localhost:8080/v1/payments/$PID/refunds   -H "Idempotency-Key: ref-1001" -d '{"amount": 1000}'
curl -s "${H[@]}" localhost:8080/v1/payments/$PID/ledger
docker compose run --rm -T serve audit                                                                 # AUDIT PASSED
```

The fee on a $25.00 capture is 2.9% (72.5, rounded half up to 73) + 30 = 103 cents. Replaying a
key returns the stored response byte for byte. Capturing again with a new key is
`409 invalid_state_transition`. Every endpoint and error code is in [docs/api.md](docs/api.md).

## Milestones

| Milestone | Scope | Status |
|-----------|-------|--------|
| M0 | Layout, Compose, Makefile, CI, migrations, `money` package | done |
| M1 | Ledger schema, posting logic, DB-enforced invariants, property tests | done |
| M2 | Payments API, auth, state machine, acquirer simulator, idempotency, concurrency tests | done |
| M3 | Webhook endpoints, dispatcher, signing, receiver, retries, chaos tests | done |
| M4 | Fraud rules engine, evaluation harness and report | not started |
| M5 | Bank settlement simulator and reconciliation | not started |
| M6 | Load tests, benchmarks, docs | not started |

## Development

```sh
make test               # unit tests with -race (no Docker)
make test-integration   # Postgres via testcontainers (needs Docker)
make test-concurrency   # the three concurrency acceptance tests, 25 runs each with -race
make chaos              # SIGKILL serve/dispatch/receiver at failpoints and at random, then check everything
make prop-long          # >= 1,000,000 generated operations each for the ledger and the payment lifecycle
make lint               # go vet with every build tag + golangci-lint
make db-up migrate      # local Postgres in Compose, then apply migrations
make audit              # ledger + payments-vs-ledger invariants in $DATABASE_URL; exits 1 on any violation
```

Requires Go 1.27+, Docker (Compose v2), and golangci-lint v2 for `make lint`. `make help` lists
every target. `load` exists already, but it exits with an error naming the milestone it arrives
in.

## Tests

The figures below are from runs on an Apple M2 (8 cores, 8 GB) with Go 1.27.4 and Postgres 17.12
(`postgres:17-alpine`). Property-test operation counts change from run to run, because rapid
picks a new random seed each time.

| Suite | Command | Result |
|-------|---------|--------|
| Unit | `go test -race -count=1 ./...` | 48 tests, 152 subtests, 0 failures |
| Integration (real Postgres) | `go test -race -count=1 -tags=integration ./test/integration/...` | 56 tests, 66 subtests, 0 failures, 55.0s |
| Chaos (SIGKILL at failpoints and at random) | `make chaos CHAOS_COUNT=3` | 3/3 passed; per run 86–99 failpoint kills, 16–19 random kills, 853–880 events, 225–330 duplicates deduped; 41.8s total |
| Concurrency acceptance | `make test-concurrency` | 25/25 passes for each of the 3 tests, 22.3s |
| Property: ledger, in-memory | `make prop-long` | 1,513,623 operations in 21.2s |
| Property: payment lifecycle, in-memory | `make prop-long` | 1,503,481 operations in 2.7s |
| Property: ledger on Postgres | integration suite (100 cases) | ~3,000 operations, ~11s |
| Property: payment lifecycle over HTTP on Postgres | integration suite (100 cases) | ~3,000 operations, ~27s |

What each test proves is in [docs/INVARIANTS.md](docs/INVARIANTS.md), together with the
deliberately broken builds each key test was shown to catch.

## Docs

- [docs/api.md](docs/api.md): endpoints, state machine, errors, fees, test card fingerprints
- [docs/webhooks.md](docs/webhooks.md): delivery guarantees, signature verification, rotation, retries
- [docs/ledger.md](docs/ledger.md): accounts, postings, a worked example (checked by
  `go test`), why authorization posts nothing, what the database enforces
- [docs/INVARIANTS.md](docs/INVARIANTS.md): invariant → how it is enforced → which test proves it
- [docs/adr/](docs/adr/): signed amounts, fees on refunds, locking, idempotency, outbox
- [docs/go-notes.md](docs/go-notes.md): Go idioms explained for a C++ programmer

## Layout

| Path | Contents |
|------|----------|
| `cmd/ledgerline` | Main binary: `serve`, `dispatch`, `merchant create`, `migrate`, `audit` |
| `cmd/receiver` | Sample merchant webhook endpoint: verifies signatures, dedupes event ids |
| `internal/api` | HTTP routing, auth, errors, idempotent transactions, the phased authorize |
| `internal/payments` | State machine, domain operations, pricing, SQL, payments-vs-ledger audit |
| `internal/payments/paymenttest` | Stateful payment-lifecycle property test (pure and over HTTP) |
| `internal/idempotency` | Request fingerprints, key store, advisory locks, fencing tokens |
| `internal/acquirer` | Card-network interface and the simulator |
| `internal/ledger` | Chart of accounts, posting builders, validation, in-memory and Postgres ledgers, audit |
| `internal/ledger/ledgertest` | Stateful ledger property test |
| `internal/merchants` | Merchant creation, API-key hashing and authentication |
| `internal/webhooks` | Outbox, endpoints, signing, dispatcher, the sample receiver's handler |
| `internal/failpoint` | Crash points, compiled in only with the `chaos` build tag |
| `internal/money`, `internal/id`, `internal/store` | Money type, prefixed IDs, pgx pool + migrations |
| `migrations/` | SQL migrations, embedded into the binary |
| `test/integration` | Tests against real Postgres (`integration` build tag) |
| `test/chaos` | Crash test with child processes and SIGKILL (`chaos` build tag) |

## Not implemented

Planned, but not built yet:

- Fraud rules (M4), reconciliation (M5), load tests (M6).

Known gaps in what is built:

- **No completer for abandoned authorizations.** If a client never retries after a crash in the
  middle of an authorization, the payment stays `created` while the network holds the funds. A
  background worker should drive stale `started` keys to completion.
- **One capture per payment.** Multiple partial captures are not supported. Voiding an
  authorization does not call the acquirer to release the hold.
- **Fixed fee in minor units for every currency** (30 = $0.30, ¥30, 0.030 BHD). Real pricing
  would set it per currency.
- **No production hardening:** no rate limiting, no API-key rotation, and no request IDs or
  metrics.
- **Webhooks:** signing secrets are stored in plaintext (production would encrypt them with a
  KMS key). Endpoints cannot be disabled or deleted through the API. Delivered events are never
  archived. Dispatchers poll (500 ms by default) rather than being woken by `LISTEN/NOTIFY`.
  Endpoint URLs are not checked against private address ranges (SSRF).
- **Ledger** (details in [docs/ledger.md](docs/ledger.md#known-gaps)): journal transactions are
  not sealed against a balanced late append; no separate database roles; no refund settlement
  lines; nothing collects from a merchant whose balance is negative.
- The currency table is a curated subset of ISO 4217 (28 codes).
