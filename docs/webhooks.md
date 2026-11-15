# Webhooks

Ledgerline tells merchants about every payment state change by POSTing a signed event to
their webhook endpoints.

## Guarantees

- **One event per committed state change.** The event is written in the same database
  transaction as the change ([ADR 0005](adr/0005-outbox.md)). A change that rolls back has no
  event, and a rejected operation (for example a 409) is not a change.
- **At-least-once delivery.** An event is retried until the endpoint answers 2xx, or until it
  goes dead after 10 attempts. It may be delivered more than once (for example when a
  dispatcher crashes after you answered). **Dedupe on the event `id`.**
- **No ordering.** Events can arrive out of order. If you need the latest state, fetch
  `GET /v1/payments/{id}`.
- **Every event has a unique id** (`evt_…`), the same across every redelivery.

## Event types

| Type | When |
|------|------|
| `payment.created` | `POST /v1/payments` |
| `payment.authorized` | the acquirer approved |
| `payment.failed` | declined, voided, or the authorization expired (`failure_code` says which) |
| `payment.captured` | capture |
| `payment.refunded` | each refund, partial or full; `data` is the refund with the payment nested |

The body:

```json
{"id": "evt_…", "object": "event", "type": "payment.captured", "created_at": "2026-11-15T10:00:00Z",
 "data": { …the payment, as GET /v1/payments/{id} returns it… }}
```

Headers: `Ledgerline-Signature`, `Ledgerline-Event-Id`, `Ledgerline-Delivery-Attempt`.

## Verifying signatures

```
Ledgerline-Signature: t=1763200800,v1=5f2c…,v1=9a1b…
```

`v1` is `hex(HMAC-SHA256(secret, "<t>.<raw request body>"))`. To verify:

1. Take `t` and every `v1` from the header.
2. Compute the HMAC over `t`, a `.`, and the **raw body bytes** (before any JSON parsing).
3. Accept if any `v1` equals it, compared in constant time (`hmac.Equal`).
4. Reject if `t` is more than 5 minutes from now. The timestamp is signed, so an attacker
   cannot re-date a captured request.
5. Dedupe on `id`: a replay inside the 5-minute window passes the signature check, so
   dedupe is what stops it from having an effect twice.

`webhooks.Verify` implements steps 1–4. `cmd/receiver` is a complete example that also does
step 5, using a processed-ids file it syncs to disk before answering 200, so dedupe survives
restarts and crashes.

## Secret rotation

`POST /v1/webhook_endpoints/{id}/rotate_secret` returns a new secret. For 24 hours the old one
stays valid and every delivery carries **two** `v1` signatures, one per secret, so you can
deploy the new secret whenever suits you.

## Retries

Failed attempts (non-2xx, connection error, or no answer within 10 s) are retried after a delay
drawn uniformly from `[0, min(1h, 1s × 2^(attempt-1))]` ("full jitter"). After 10 attempts the
delivery is `dead`. `POST /v1/events/{id}/redeliver` queues it again with a fresh budget, and
also queues it for endpoints created after the event. All of these are
`ledgerline dispatch` flags: `-retry-base`, `-retry-max`, `-max-attempts`, `-timeout`, `-lease`.

## Running

```sh
ledgerline dispatch                         # any number of these may run at once
receiver -addr :9090 -secrets "$SECRET" -store processed.log
```
