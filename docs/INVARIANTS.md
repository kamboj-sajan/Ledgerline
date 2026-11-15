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

## Schema

| Invariant | Enforced by | Proven by |
|-----------|-------------|-----------|
| Every migration can be fully rolled back and re-applied | Each file's Down section | `TestMigrationsUpDownUp` |

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

## Still to come

Payments: refunded ≤ captured ≤ authorized (DB CHECK), the state machine transition table,
idempotency, concurrency acceptance tests (M2). Outbox and webhook delivery (M3). Fraud (M4).
Reconciliation (M5).
