# ADR 0005: Transactional outbox for webhooks, not dual writes

**Status:** accepted (M3)

## Context

Every payment state change must tell the merchant, by webhook. The state change lives in
Postgres; the webhook is an HTTP request to someone else's server. Nothing makes those two
atomic.

- **Dual write:** commit the change, then send the webhook (or send, then commit). A crash
  between the two loses the webhook or sends one for a change that never committed. Sending
  inside the transaction is no better: the receiver may see an event the transaction then rolls
  back, and the payment row lock is held across network I/O.
- **Change data capture** (logical replication, Debezium): reliable, but it needs an extra
  pipeline to operate, and the event shape follows the table layout instead of the API.
- **Transactional outbox:** in the same transaction as the change, insert an `events` row and
  one `webhook_deliveries` row per endpoint. A separate dispatcher process reads due deliveries
  and sends them.

## Decision

Transactional outbox. `webhooks.Insert` takes a `pgx.Tx`, so the compiler stops anyone writing an
event outside the transaction that makes the change.

The dispatcher claims due deliveries in one statement (`FOR UPDATE SKIP LOCKED` plus an `UPDATE`
that pushes `next_attempt_at` one lease ahead and counts the attempt) and commits. It then sends
the request *outside* any transaction, and records the outcome only on rows still `pending`.

## Why

- **"Exactly one event per committed state change" becomes a property of COMMIT.** The event
  and the change are one transaction: both exist or neither does. The chaos test checks this
  for every payment after hundreds of SIGKILLs.
- **SKIP LOCKED lets any number of dispatchers run** without coordinating. A row locked by one
  dispatcher is invisible to the others' claim queries, so they take the next rows instead of
  waiting.
- **A lease instead of holding the lock during the POST.** Holding a transaction open over a
  10-second HTTP timeout would pin a pool connection per in-flight webhook, and would block
  on slow receivers. The lease gives the same "nobody else takes it" guarantee, and it expires
  on its own when the dispatcher dies.

## Consequences

- **Delivery is at-least-once.** If a dispatcher crashes after the receiver answered 2xx but
  before it recorded `delivered`, the lease expires and the event is sent again. The receiver
  must dedupe on the event id, and `cmd/receiver` shows how. The chaos test counts 225–330
  such duplicates per run, all deduped.
- **Delivery is unordered.** Retries with backoff, several dispatchers and expired leases all
  reorder events. A `payment.refunded` may arrive before the `payment.captured` it follows.
  Receivers should treat each event as "this happened", and fetch the payment if they need the
  current state.
- **The outbox grows.** Delivered rows are kept (they are the delivery history the
  redelivery API uses). Archiving or partitioning old events is not implemented.
- **Latency:** an event is sent on the dispatcher's next poll (default 500 ms), not
  immediately. `LISTEN/NOTIFY` could wake dispatchers sooner; it is not implemented.

## Evidence

`TestOutboxEventOnlyWithCommit` (rollback leaves nothing), `TestWebhookEndpointsAndOutbox` (no
event for a rejected operation), `TestDispatchManyDispatchersNoDoubleSend` (4 dispatchers, each
event sent once; without `SKIP LOCKED`, sent 2–3 times),
`TestDispatchExpiredLeaseResendsAndReceiverDedupes`, and `TestChaos` assertions 2–4.
