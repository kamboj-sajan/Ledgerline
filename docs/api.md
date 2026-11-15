# API reference

JSON over HTTP. Authenticate with `Authorization: Bearer <api key>`: one key per merchant,
created with `ledgerline merchant create -name NAME` and stored only as a SHA-256 hash. Every POST
needs an `Idempotency-Key` header ([ADR 0004](adr/0004-idempotency.md)).

Amounts are JSON **integers** in the currency's minor unit (`2500` USD is $25.00; `2500` JPY is
¥2500; `2500` BHD is 2.500 BHD), always next to an upper-case ISO 4217 code. `12.5`, `1e3`,
`"100"` and `100.0` are all rejected.

## Endpoints

| Method and path | Body | Success |
|-----------------|------|---------|
| `POST /v1/payments` | `amount`, `currency`, `customer_id`, `card{fingerprint, country}`, `ip_country`, `metadata` (≤ 20 string pairs) | 201 payment (`created`) |
| `POST /v1/payments/{id}/authorize` | none | 200 payment (`authorized`), or 402 `card_declined` (payment `failed`) |
| `POST /v1/payments/{id}/capture` | optional `amount` ≤ authorized; defaults to all of it | 200 payment (`captured`) |
| `POST /v1/payments/{id}/refunds` | optional `amount` ≤ unrefunded; defaults to all of it | 201 refund, with the payment nested |
| `POST /v1/payments/{id}/void` | none | 200 payment (`failed`, `failure_code: voided`) |
| `GET /v1/payments/{id}` | | 200 payment |
| `GET /v1/payments/{id}/ledger` | | 200 list of journal transactions with signed entries and account IDs |
| `GET /v1/accounts/{id}/balance` | | 200 balance of one of *your* accounts (`balance` in the account's normal direction, `signed_balance` debit-positive) |

| `POST /v1/webhook_endpoints` | `url` (http or https) | 201 endpoint, **with its `secret` (shown only here)** |
| `POST /v1/webhook_endpoints/{id}/rotate_secret` | none | 200 endpoint with the new secret; the old one stays valid for 24 h |
| `GET /v1/events` | query `limit` (1–100, default 20), `starting_after` (event id) | 200 list, oldest first |
| `GET /v1/events/{id}` | | 200 event |
| `POST /v1/events/{id}/redeliver` | none | 200; queues the event again for every enabled endpoint |

Delivery semantics, signature verification and retries: [docs/webhooks.md](webhooks.md).

## State machine

```
created ──approve──▶ authorized ──capture──▶ captured ──refund (rest)──▶ refunded
   │                     │                    │  ▲
   └──decline──▶ failed ◀┘ void / expire      └──┘ partial refund
```

Every other operation is `409 invalid_state_transition`. The table is
`internal/payments/state.go`. Authorizations not captured within 7 days
(`-authorization-expiry`) are expired by a background sweep, through the same state machine.

## Errors

```json
{"error": {"type": "invalid_request_error", "code": "invalid_state_transition", "message": "…"}}
```

| Status | type | code | Stored under the key? |
|--------|------|------|-----------------------|
| 400 | `invalid_request_error` | `parameter_missing`, `parameter_invalid` (with `param`), `invalid_json`, `idempotency_key_missing`, `idempotency_key_invalid` | no |
| 400 | `invalid_request_error` | `amount_too_large` | yes |
| 401 | `authentication_error` | `invalid_api_key` | no |
| 402 | `card_error` | `card_declined` (with `decline_code`, `payment_id`) | yes |
| 404 | `invalid_request_error` | `resource_missing` (also for another merchant's payment), `route_not_found` | no |
| 405 | `invalid_request_error` | `method_not_allowed` (with `Allow` header) | no |
| 409 | `invalid_request_error` | `invalid_state_transition` | yes |
| 409 | `idempotency_error` | `idempotency_key_reused` | no |
| 409 | `idempotency_error` | `idempotency_key_in_use` (with `Retry-After`) | no |
| 413 | `invalid_request_error` | `request_too_large` (body over 1 MiB) | no |
| 500 | `api_error` | `internal_error` | no |
| 503 | `api_error` | `acquirer_unavailable` (with `Retry-After`) | no |
| 504 | `api_error` | `acquirer_timeout`: outcome unknown, retry with the same key (with `Retry-After`) | no |

A replayed response is byte-identical to the original and carries `Idempotent-Replayed: true`.

## Fraud

Authorization runs the rules in `internal/fraud` against the card's payments across all
merchants in the last 30 days. The verdict appears on the payment as
`"fraud": {"decision": "allow" | "review" | "block", "rules": [...]}`. `block` fails the payment
with `failure_code: fraud_suspected` and returns `402 card_error fraud_suspected`; the acquirer
is never called. `review` is recorded and the payment continues. Thresholds are a JSON file
(`ledgerline serve -fraud-config FILE`). The embedded default and its evidence are in
[fraud-eval.md](fraud-eval.md).

## Fees

Per merchant: `round_half_up(captured × fee_bps / 10000) + fee_fixed`, capped at the captured
amount. Defaults are 290 bps + 30 minor units (2.9% + $0.30 for USD). The fixed part is in the
payment currency's minor units for every currency, which is a simplification. Fees are not
returned on refunds ([ADR 0002](adr/0002-fees-on-refunds.md)).

## Simulated acquirer

`ledgerline serve` uses a simulated card network with configurable latency, jitter, decline
rate, timeout rate and error rate (`-acquirer-*` flags). These card fingerprints behave
deterministically:

| `card.fingerprint` | Behaviour |
|--------------------|-----------|
| `tok_decline` | declined, `card_declined` |
| `tok_insufficient_funds` | declined, `insufficient_funds` |
| `tok_timeout` | authorized, but the first call times out (504); a retry learns the result |
| `tok_unavailable` | always 503 |
