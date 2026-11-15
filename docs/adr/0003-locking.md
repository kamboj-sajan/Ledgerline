# ADR 0003: Pessimistic row locks (SELECT … FOR UPDATE) for payment state changes

**Status:** accepted (M2)

## Context

Capture, refund, void, authorize (phase 3) and the expiry sweep all read a payment, decide
whether the change is allowed, and write it back. They also post to the ledger and write an
outbox event. Concurrent requests on one payment must never both act on the same old state: two
captures, or two refunds that each fit on their own but not together.

The spec allows two strategies:

- **A. Pessimistic:** `SELECT … FOR UPDATE` inside the transaction, then decide in Go, then write.
- **B. Optimistic / conditional:** `UPDATE payments SET … WHERE id = $1 AND status = $expected
  [AND refunded_amount + $r <= captured_amount] RETURNING …`, and treat "0 rows" as a conflict.

## Decision

A: every state change goes through `payments.GetForUpdate`, which locks the payment row until
the transaction ends. The decision is made by the Go transition table
(`internal/payments/state.go`) on the locked row. The database CHECKs are a backstop.

## Why

- **One transition table.** The rules live in one place in Go and are tested exhaustively
  (25 (from, to) pairs and 35 (event, from) pairs). With B, each rule would be re-encoded as a
  `WHERE` clause per operation, giving a second copy of the state machine in SQL that can drift
  from the first.
- **The decision needs more than the row.** A refund computes the remaining amount, a capture
  computes the fee from merchant pricing, and both build a ledger transaction from the values
  they read. Under A those values cannot change between the read and the write. Under B the
  ledger posting would be computed from a read that the conditional UPDATE then has to
  re-validate.
- **Contention costs about the same.** In Postgres a conditional UPDATE also takes the row lock
  and holds it until commit, so B does not let more work run in parallel on one payment. It only
  moves the waiting from the SELECT to the UPDATE.
- **Deadlock-free by construction.** Each transaction locks at most one payment row, always
  after its idempotency key (advisory lock, then key row, then payment row). Ledger writes are
  inserts and lock no account rows.

## Evidence

`TestConcurrencyOverRefund` and `TestConcurrencyCaptureDifferentKeys` (100 captures and 50
refunds racing on one payment, 25 runs under `-race`) pass. With `FOR UPDATE` removed from
`GetForUpdate` (mutation check), the refund test fails every time: 23,425 refunded against
10,000 captured. The `refunded_amount <= captured_amount` CHECK did **not** catch it. Each
transaction read `refunded = 0` and wrote back only its own amount, so every row it wrote was
valid on its own. That is a lost update, which only the lock prevents. The payments-vs-ledger
audit does detect the result afterwards, because the ledger recorded every refund.

## Consequences

- Requests on the same payment are serialised. A hot payment is a throughput limit, measured in
  M6.
- The lock is held for the whole transaction, so no network I/O may happen inside it. That is
  why authorize calls the acquirer between transactions ([ADR 0004](0004-idempotency.md)).
- A transaction waiting on the row lock holds a pool connection. Under heavy contention on one
  payment the pool, not the CPU, is the limit.

## Rejected

- **B, conditional UPDATE:** described above. It would be the better choice for a single-column
  counter with no derived writes. Here every change also writes ledger entries computed from the
  state.
- **SERIALIZABLE isolation with retries:** correct, but every conflicting transaction is
  aborted and retried, including its ledger and outbox writes, and that retry loop would need
  its own tests. Explicit locks make the waiting visible and deterministic.
