# ADR 0004: Idempotency keys

**Status:** accepted (M2)

## Context

Clients retry: after timeouts, dropped connections, crashes on either side. A retried capture
must not capture twice, a retried refund must not refund twice, and a retried authorization
must never authorize twice at the card network. Every POST therefore carries an
`Idempotency-Key`.

## Decision

### Identity of a request

Keys are scoped per merchant (`PRIMARY KEY (merchant_id, key)`) and stored with
`SHA-256(method ‖ path ‖ canonical body)`. The canonical body is the JSON decoded with
`UseNumber` and re-encoded with sorted keys and no whitespace. Numbers are kept exactly as
written, never rounded through float64. Two requests that differ only in formatting are the same
request; any other difference under the same key is `409 idempotency_key_reused`. Keys expire
after 24 hours.

### What is stored

A response is stored once the request **started executing**. That includes business-rule
rejections (`409 invalid_state_transition`, `400 amount_too_large`, `402 card_declined`), which
are replayed byte for byte like successes. Nothing is stored for requests rejected *before*
execution (bad auth, invalid input, unknown payment, key in use) or for transient failures
(acquirer timeout or outage, 500s). So those never use up the key, and a retry runs again.

### Single-transaction endpoints (create, capture, refund, void)

Everything commits in **one** transaction, in this order:

1. `pg_try_advisory_xact_lock(hash(merchant, key))`. If another transaction holds it:
   `409 idempotency_key_in_use` with `Retry-After: 1`.
2. Look up the key row: replay a stored response, or reject a different request.
3. `SELECT … FOR UPDATE` the payment ([ADR 0003](0003-locking.md)), apply the state machine.
4. Insert ledger entries, update the payment, insert the outbox event.
5. Insert the key row with the response. Commit.

Because the advisory lock and the key row live in the request's own transaction, a crash or
error at any point before COMMIT leaves *nothing*: the lock is released, there is no row, and the
retry starts from scratch. There is no "stuck in progress" state to clean up.

### Authorize (external call)

The acquirer call must not happen inside a transaction (it would hold the payment's row lock
across network I/O), so authorize uses atomic phases with recovery points, after Brandur Leach's
*Implementing Stripe-like Idempotency Keys in Postgres*:

1. **Transaction:** advisory lock, look up the key, check the payment can be authorized, insert
   the key row as `started` with `locked_at = now()` and a random `lock_token`. Commit.
2. **No transaction:** call the acquirer with **our payment ID as its idempotency key**.
3. **Transaction:** `Claim` the key: it must still be `started` with our token. Lock the payment,
   apply approve/decline, update, insert the outbox event, store the response, set `finished`.
   Commit.

A second request with the key while phase 2 runs sees a fresh `locked_at`:
`409 idempotency_key_in_use`. After a crash, `locked_at` goes stale (default 30 s,
`-authorize-lock-timeout`). The retry then takes the key over with a new token and resumes at
phase 2. Calling the acquirer again is safe: it returns the decision it recorded the first time.
If the original request was only slow, it finds its token replaced when it reaches phase 3. It
then either replays the new owner's stored result or answers `409`. It never writes a second
outcome. On a transient acquirer error the key is unlocked at once, so the retry need not wait
for it to go stale.

## Why

- **One transaction where possible.** The simplest correct design is the one that has no
  partial states. Only authorize needs recovery points, because only authorize talks to the
  outside world.
- **An advisory lock for "in use"** rather than inserting the row first and checking it. A
  row that exists only after commit cannot be left half-done by a crash. A transaction-scoped
  lock disappears with its transaction.
- **The payment ID as the acquirer key** turns "did the network authorize this?" from an
  unknowable into a question a retry can simply ask again.
- **A fencing token** because a stale lock does not mean the owner is dead. It may wake up
  later, and it must not be able to overwrite the result of the request that took over.

## Consequences

- Advisory lock IDs are 64-bit hashes of (merchant, key). A collision gives a spurious
  `idempotency_key_in_use`, which the client retries through. It never causes a wrong result.
- A started authorization whose client never retries stays `created` while the network holds the
  funds, until the client retries. A background completer that drives stale `started` keys to
  completion is not implemented (README, "Not implemented").
- Replaying `POST /v1/payments` returns the original payment ID. Clients get a single payment per
  logical create without any extra lookup.

## Evidence

`test/integration/api_test.go` (replay, reuse, rejected-before-execution, stored 409, per-merchant
scope, expiry), `api_authorize_test.go` (decline replay, timeout then retry, key in use, stale
takeover, crash after the acquirer approved, concurrent different keys),
`idempotency_store_test.go` (fencing), and the concurrency acceptance tests. Mutation checks: a
fresh acquirer key per attempt is caught (two network authorizations). Dropping the fencing check
is caught by `TestIdempotencyMultiPhase`.
