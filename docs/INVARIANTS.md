# Invariants

Each invariant, what enforces it, and the test that proves it. "DB" means Postgres rejects the
violation on its own, whatever the Go code does. "Code" means Go rejects it before it reaches
the database. Where both are listed, each layer has its own test.

Test names are clickable in most editors; integration tests live in `test/integration` and need
Docker (`make test-integration`).

## Money

| Invariant | Enforced by | Proven by |
|-----------|-------------|-----------|
| Amounts are int64 minor units paired with an ISO 4217 currency; no floats anywhere | Code: `money.Money` has no float API; JSON decoding rejects `12.5`, `1234.0`, `1e3`, `"1234"`, `null` | `TestJSON`, `TestPropJSONRoundTrip` |
| Each currency has the correct exponent (USD 2, JPY 0, BHD 3) | Code: fixed table | `TestParseCurrency`, `TestCurrenciesSortedAndUnique` |
| Arithmetic and comparison across currencies is an error | Code | `TestAddSub`, `TestCmp`, `TestPropCrossCurrencyAlwaysFails` |
| Add and Sub never overflow silently | Code: bounds checked before adding | `TestAddSub`, `TestPropAddSubMatchBigInt` (vs `math/big`) |
| Fee maths is exact and rounds half away from zero | Code: 128-bit intermediate | `TestMulBasisPoints`, `TestPropMulBasisPointsMatchesBigInt` |
| Decimal parsing and formatting are exact inverses | Code | `TestParseMajor`, `TestFormatMajor`, `TestPropFormatParseRoundTrip` |

## Ledger

| Invariant | Enforced by | Proven by |
|-----------|-------------|-----------|
| Every journal transaction has ≥ 2 entries | DB: deferred constraint triggers. Code: `Tx.Validate` | `TestSchemaBalanceIsCheckedAtCommit`, `TestValidate`, `TestPropLedgerPostgres` (raw malformed writes) |
| Every journal transaction sums to 0 per currency | DB: deferred constraint triggers, including entries appended to an older transaction. Code: `Tx.Validate` | `TestSchemaBalanceIsCheckedAtCommit`, `TestSchemaRejectsUnbalancedAppendToCommittedTransaction`, `TestValidate`, `TestPropLedgerMemory`, `TestPropLedgerPostgres` |
| The balance check happens at COMMIT, not per INSERT | DB: `DEFERRABLE INITIALLY DEFERRED` | `TestSchemaBalanceIsCheckedAtCommit` (fails at commit, every INSERT succeeds) |
| Global sum of all entries is 0 per currency | Follows from the row above; checked by `ledgerline audit` | Both property tests (after every step), `TestAuditPassesOnHealthyLedger` |
| An entry's currency equals its account's currency | DB: composite FK. Code: `Tx.Validate` | `TestSchemaRejectsInvalidEntries`, `TestValidate`, `TestPropLedgerPostgres` |
| No zero-amount entries | DB: CHECK. Code: `Tx.Validate` | `TestSchemaRejectsInvalidEntries`, `TestValidate` |
| Accounts, journal transactions and entries are append-only (UPDATE, DELETE, TRUNCATE) | DB: triggers | `TestSchemaIsAppendOnly` |
| Account code fixes type and ownership (chart of accounts) | DB: CHECK. Code: `AccountKey.validate`; drift guard | `TestSchemaChartOfAccounts`, `TestValidate`, `TestChartMatchesMigration` |
| Exactly one account per (merchant, code, currency), platform accounts included, even under concurrent creation | DB: `UNIQUE NULLS NOT DISTINCT`; code: `ON CONFLICT DO NOTHING` then SELECT | `TestSchemaChartOfAccounts`, `TestLedgerEnsureAccountConcurrent` (50 racing transactions) |
| An external reference (e.g. a settlement line) posts at most once | DB: `UNIQUE (external_ref)`. Code: `ledger.Memory` | `TestSchemaExternalRefAndReversalUniqueness`, `TestLedgerPostMapsSchemaRulesToErrors`, property tests ("settle again") |
| A transaction is reversed at most once; reversals are not reversed | DB: `UNIQUE (reverses_id)`. Code: `ledger.Reverse` | `TestSchemaExternalRefAndReversalUniqueness`, `TestReverse`, property tests ("reverse again") |
| Every balance equals an independent reference model after every operation | n/a (this is the check itself) | `TestPropLedgerMemory`, `TestPropLedgerPostgres` |
| A journal transaction commits or rolls back with the caller's transaction | Code: `ledger.Post` takes a `pgx.Tx` | `TestLedgerPostRollsBackWithCallerTransaction` |
| Balances are computed from entries (there is no cached balance to drift) | Design | n/a: no cache exists. A cache would need its own equality test |
| The audit finds violations in stored data, not only rejected writes | `ledgerline audit` SQL | `TestAuditDetectsCorruptionThatBypassedTriggers` (damage written with triggers disabled) |
| Documented postings match the code | Example test | `Example` in `internal/ledger/example_test.go` (the worked example in `docs/ledger.md`) |

## Payments

| Invariant | Enforced by | Proven by |
|-----------|-------------|-----------|
| Only the transitions in the event table are possible; anything else is `409 invalid_state_transition` and changes nothing | Code: event-keyed table in `internal/payments/state.go` | `TestTransitionTable` (all 25 (from, to) pairs), `TestEventTable` (all 35 (event, from) pairs), `TestOperationsFollowTheTable` (6 operations × 5 states), `TestAPIStateMachineEveryStateAndOperation` (4 HTTP operations × 5 states) |
| refunded ≤ captured ≤ authorized ≤ amount, fee ≤ captured | DB: `payments_amounts` CHECK. Code: domain methods | `TestPaymentsSchemaChecks`, `TestCapture`, `TestRefunds`, `TestAPICaptureAndRefundLimits` |
| Each status implies its amounts (e.g. `refunded` ⇒ refunded = captured > 0; `failed` ⇒ no capture and a failure code) | DB: `payments_status_amounts` CHECK | `TestPaymentsSchemaChecks` |
| Every payment's status and amounts equal a reference model after every operation, including invalid ones | n/a (this is the check) | `TestPropPaymentsPure` (≥ 1,000,000 operations in `make prop-long`), `TestPropPaymentsAPI` (over HTTP on Postgres) |
| The fee is basis points (half away from zero) + fixed, capped at the amount | Code: `Pricing.Fee` | `TestPricingFee`, and the property tests' independent fee formula |
| The payments table agrees with the ledger: no orphan journal transactions; captured, fee and refunded amounts match the postings; refunds table sums to `refunded_amount`; no entries in another merchant's accounts | `payments.Audit`, run by `ledgerline audit` | `TestPaymentsAuditCrossChecksLedger`; run after every step of `TestPropPaymentsAPI` and at the end of every API and concurrency test |
| One merchant cannot read or change another's payments or accounts | Code: every query is scoped by merchant ID | `TestAPIErrors`, `TestAPIPaymentLifecycle` (balance), property tests ("wrong merchant" ⇒ not found) |
| Refunds are append-only | DB: triggers | `TestRefundsAreAppendOnly` |

## Idempotency

| Invariant | Enforced by | Proven by |
|-----------|-------------|-----------|
| Same key + same request ⇒ the stored status and body, byte for byte, without executing again | Code: stored response replayed; DB: `(merchant_id, key)` primary key | `TestAPIIdempotency/replay…`, `TestAuthorizeDeclineIsStoredAndReplayed` |
| Same key + different request ⇒ `409 idempotency_key_reused`, nothing executes | Code: SHA-256 of method, path, canonical body | `TestAPIIdempotency/same key…`, `TestIdempotencyStoreFinishedRecord`, `TestFingerprint` |
| Formatting does not change a request's identity; numbers are never rounded | Code: `Canonicalize` with `UseNumber` | `TestCanonicalize` |
| Same key while the first request runs ⇒ `409 idempotency_key_in_use` with `Retry-After` | DB: advisory lock (single-transaction endpoints); `locked_at` on the key row (authorize) | `TestIdempotencyLockIsExclusiveAndReleasedWithTransaction`, `TestAuthorizeKeyInUseWhileRunning`, `TestConcurrencyCaptureSameKey` |
| Requests rejected before execution (auth, validation, not found, in use) do not use up the key | Code: nothing is written before execution; the lock is transaction-scoped | `TestAPIIdempotency/requests rejected…`, `TestIdempotencyLockIsExclusiveAndReleasedWithTransaction` (rollback leaves no record) |
| Capture/refund/void/create: key, state change, ledger entries, outbox event and stored response commit in one transaction | Code: `runIdempotent` | `TestAPIPaymentLifecycle` (event count), concurrency tests (exactly one journal transaction and one event), audits |
| A retried authorization never authorizes twice at the acquirer | Code: our payment ID is the acquirer's idempotency key | `TestAuthorizeTimeoutThenRetry`, `TestAuthorizeCrashAfterAcquirerApproved`, `TestAuthorizeConcurrentDifferentKeys`, `TestAcquirerConcurrentCallsWithOneKeyAuthorizeOnce` |
| A request whose lock went stale and was taken over cannot record a second outcome | Code: fencing `lock_token` checked by `Claim` | `TestIdempotencyMultiPhase`, `TestAuthorizeStaleLockTakeover` |
| Keys expire after 24 h | DB: `expires_at`; code: `Lookup` forgets, `Purge` deletes | `TestIdempotencyStoreExpiry`, `TestAPIIdempotency/an expired key…` |

## Concurrency (acceptance tests, `make test-concurrency`: 25 runs each under `-race`)

| Invariant | Proven by |
|-----------|-----------|
| 100 captures with different keys ⇒ exactly 1 × 200, 99 × `409 invalid_state_transition`, one capture journal transaction, one event | `TestConcurrencyCaptureDifferentKeys` |
| 100 identical requests with one key, retrying on `in_use` ⇒ all responses byte-identical, one capture | `TestConcurrencyCaptureSameKey` |
| 50 parallel partial refunds totalling more than the capture ⇒ refunded total ≤ captured, and payment, refunds table and ledger agree | `TestConcurrencyOverRefund` |

Mechanism: `SELECT … FOR UPDATE` on the payment row ([ADR 0003](adr/0003-locking.md)).

## Webhooks and the outbox

| Invariant | Enforced by | Proven by |
|-----------|-------------|-----------|
| Exactly one outbox event per committed state change, none on rollback or rejection | Code: `webhooks.Insert` takes the change's `pgx.Tx`; DB: events append-only | `TestOutboxEventOnlyWithCommit`, `TestWebhookEndpointsAndOutbox`, concurrency tests, `TestChaos` (per payment, after SIGKILLs) |
| Deliveries are queued in the same transaction, for the endpoints that existed then | Code | `TestWebhookEndpointsAndOutbox` |
| Signatures: tampered body, wrong secret, stale or future timestamp, re-dated timestamp are all rejected | Code: `webhooks.Verify` (`hmac.Equal`, signed timestamp, 5 min window) | `TestSignatureVerification`, `TestSignMatchesSpec` (against openssl), `TestReceiverRejectsAndDedupes` |
| During rotation, receivers on the old or the new secret both accept | Code: one `v1` per active secret | `TestSignatureDuringRotation`, `TestDispatchSignsWithBothSecretsDuringRotation` |
| Several dispatchers never send one delivery at the same time | DB: `FOR UPDATE SKIP LOCKED` claim plus lease | `TestDispatchManyDispatchersNoDoubleSend` (4 dispatchers, 120 events, each sent once) |
| Every event is delivered at least once, through crashes | Lease expiry, attempts counted at claim | `TestDispatchExpiredLeaseResendsAndReceiverDedupes`, `TestChaos` |
| The receiver processes each event id exactly once, across retries, replays and restarts | Receiver: dedupe file synced to disk before 200 | `TestReceiverRejectsAndDedupes`, `TestFileStoreDedupesAcrossRestarts`, `TestChaos` |
| Failed attempts back off with full jitter, capped; dead after N attempts; redelivery resets the budget | Code: `webhooks.Backoff`, dispatcher | `TestBackoffCeilings`, `TestBackoffIsSpreadOverTheInterval`, `TestDispatchRetriesAfter500ThenSucceeds`, `TestDispatchReceiverTimeout`, `TestDispatchGivesUpThenRedelivers` |

## Crash safety (`make chaos`)

`TestChaos` runs serve, dispatch and receiver as child processes. It SIGKILLs them at three
failpoints (after the acquirer call and before recording it, after a commit and before the
response, after a webhook send and before marking it delivered) and at random every
200–800 ms, while 8 clients run 200 payment flows and retry with the same keys. Each run must
see random kills and every failpoint fire. Then:

| Assertion | Result (3 consecutive runs) |
|-----------|-----------------------------|
| Ledger and payments-vs-ledger audits pass; one payment per flow, in the state its client saw | passed |
| Exactly one outbox event per committed state change | passed |
| Every event delivered at least once | passed |
| Receiver processed each event id exactly once | passed; 225–330 duplicate deliveries deduped per run |
| No payment authorized twice at the acquirer; acquirer and our records agree | passed |

## Fraud

| Invariant | Enforced by | Proven by |
|-----------|-------------|-----------|
| Rules are pure: the same (transaction, history) gives the same verdict, whatever the history order | Code: `fraud.Config.Signals`/`Decide` take no clock and no database | `TestPropOrderDoesNotMatter`, `TestSignals` |
| Raising a threshold never makes a rule fire more (what makes sweeps meaningful) | Code | `TestPropLooserThresholdsNeverAddHits` |
| A blocked payment fails with `fraud_suspected` and never reaches the acquirer | Code: phase 1 of authorize; DB: `payments_fraud_failure` CHECK | `TestFraudBlockFailsPaymentWithoutCallingAcquirer` |
| The verdict and fired rules are stored on the payment; review still authorizes | Code | `TestFraudReviewStillAuthorizes`, `TestFraudAllowRecordsDecision` |
| Velocity is per card across all merchants | Code: `payments.RecentByCard` | `TestFraudVelocityAcrossMerchants` |
| The evaluation tunes on the earlier split only, and the API and the harness run the same rule code | `internal/fraud/eval` | `TestSweepChoosesOnTuneOnly`, `TestLoadSparkovUsesTrainHistoryForTest`; report in [fraud-eval.md](fraud-eval.md) |

## Schema

| Invariant | Enforced by | Proven by |
|-----------|-------------|-----------|
| Every migration can be fully rolled back and re-applied, including the `acquirer` schema | Each file's Down section | `TestMigrationsUpDownUp` |

## Evidence that the tests can fail

A test that cannot fail proves nothing, so each key test was run once against a deliberately
broken build. The real code was left unchanged; the bug was swapped in with `go test -overlay`
or in a scratch copy of the repo.

| Injected bug | Caught by |
|--------------|-----------|
| `Money.Add` without its overflow guard | `TestPropAddSubMatchBigInt`, after 2 generated cases |
| A migration whose Down section leaves a table behind | `TestMigrationsUpDownUp` |
| Capture that credits the fee to the merchant (balanced, but wrong) | `TestPropLedgerMemory`, first case; shrunk to 8 EUR captured with an 8 EUR fee |
| `ledger.Memory` without external-reference uniqueness | `TestPropLedgerMemory`, first case |
| Schema without the balance triggers | `TestPropLedgerPostgres`, after 4 cases (a single-entry transaction was accepted) |
| Fee without its fixed part | `TestPropPaymentsPure`, after 4 cases; shrunk to a 1-unit capture whose fee should be 1 |
| Authorize sends the acquirer a fresh key per attempt instead of the payment ID | `TestAuthorizeTimeoutThenRetry` (2 authorizations at the network) |
| `Claim` ignores the fencing token | `TestIdempotencyMultiPhase`. The HTTP-level takeover test does not catch it: there the new owner finishes first, so the old one simply replays. |
| `FOR UPDATE SKIP LOCKED` removed from the dispatcher's claim | `TestDispatchManyDispatchersNoDoubleSend` in 3/3 runs (events sent 2–3 times) |
| Authorize uses a fresh acquirer key per attempt (end to end) | `TestChaos`: 29 payments authorized more than once, 3 where the acquirer and our records disagree |
| Receiver without dedupe | `TestChaos`: 1,158 events processed for 877 unique ids |
| `FOR UPDATE` removed from `GetForUpdate` | `TestConcurrencyOverRefund` in 3/3 runs (23,425 refunded of 10,000; the CHECK constraint did not catch it), `TestConcurrencyCaptureDifferentKeys` in 2/3 |

## Still to come

Reconciliation (M5).
