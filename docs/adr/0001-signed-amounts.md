# ADR 0001: Signed amounts instead of debit/credit columns

**Status:** accepted (M1)

## Context

Every ledger entry is either a debit or a credit. The schema has to represent that, and the
representation decides how the core invariant ("every journal transaction balances") is written,
checked and indexed.

Options considered:

- **A. One signed `amount BIGINT` column.** A debit is positive, a credit is negative.
- **B. Two columns `debit BIGINT`, `credit BIGINT`,** with a CHECK that exactly one is non-zero.
  This is how a paper ledger looks.
- **C. A positive `amount` plus a `direction` enum** (`debit` | `credit`).

## Decision

Option A: a signed amount, debit positive, credit negative, with `CHECK (amount <> 0)`.

## Why

- **The invariant becomes one aggregate.** A journal transaction balances exactly when
  `SUM(amount) = 0` per currency. The deferred constraint trigger, the Go `Tx.Validate`, the audit
  and the property-test model all check that same expression. With B or C every check becomes
  `SUM(debit) - SUM(credit)` or a `CASE` on direction, which is one more place to get a sign wrong.
- **Balances are a plain sum.** An account's balance is `SUM(amount)`. The index
  `entries (account_id) INCLUDE (amount)` answers it with an index-only scan.
- **Reversal is negation.** `ledger.Reverse` negates every posting. Under B it would have to
  swap columns.
- **No overflow in the database.** Postgres sums `bigint` as `numeric`, so totals are exact. The
  Go side checks for int64 overflow explicitly (`money.Add`).

## Consequences

- Raw signed balances read oddly for credit-normal accounts: a merchant we owe $96.80 has a
  `merchant_payable` balance of `-9680`. `ledger.NormalBalance` converts using the account type
  (assets and expenses as-is, liabilities and revenue negated). The API will show normal balances,
  and the docs say which sign is which.
- Anyone writing SQL by hand has to know the convention. It is stated at the top of
  `migrations/00002_ledger.sql` and in `docs/ledger.md`.
- An accountant's debit/credit view can still be produced at the edge: debit = `max(amount, 0)`,
  credit = `max(-amount, 0)`.

## Rejected

- **B (two columns):** twice the columns, a CHECK just to keep them exclusive, and every query
  has to combine them. It is more familiar to accountants, but the familiarity can be recovered
  in presentation without paying for it in every invariant.
- **C (direction enum):** all the costs of B, plus an enum type to maintain.
