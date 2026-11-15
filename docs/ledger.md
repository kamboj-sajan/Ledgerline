# The ledger

Ledgerline records every money movement as a double-entry journal transaction. This page
describes the accounts, the postings each event produces, and which rules the database enforces
on its own.

## Sign convention

Each entry has a signed `amount` in minor units: **debit positive, credit negative**
([ADR 0001](adr/0001-signed-amounts.md)). A journal transaction balances when its entries sum to
zero in each currency, and an account's balance is the sum of its entries. To read a balance in
the account's natural direction, use `ledger.NormalBalance`: assets and expenses as-is,
liabilities and revenue negated.

## Chart of accounts

One account per (owner, code, currency), created the first time it is used.

| Code | Type | Owner | Meaning |
|------|------|-------|---------|
| `processor_receivable` | asset | platform | Owed to us by the card network for captures it hasn't settled yet |
| `bank_cash` | asset | platform | Money that has arrived in our bank account |
| `merchant_payable` | liability | each merchant | Money we owe the merchant |
| `fee_revenue` | revenue | platform | Our processing fees |
| `network_fee_expense` | expense | platform | Fees the network deducts when it settles |

The database enforces this table (`accounts_chart` CHECK). `TestChartMatchesMigration` fails
if the Go copy in `internal/ledger/ledger.go` drifts from it.

## Postings

| Event | Postings |
|-------|----------|
| Capture X, fee f | debit `processor_receivable` X · credit `merchant_payable` X−f · credit `fee_revenue` f |
| Refund R | debit `merchant_payable` R · credit `processor_receivable` R |
| Settlement, gross G, network fee n | debit `bank_cash` G−n · debit `network_fee_expense` n · credit `processor_receivable` G |
| Reversal of T | every posting of T with its sign flipped |

Lines that would be zero are left out (a capture with no fee has two lines). The database
rejects zero entries.

### Worked example

A $100.00 capture with a $3.20 fee (2.9% + 30¢), a $25.00 partial refund, then settlement of the
capture with a $0.50 network fee. Amounts are in cents.

| Account | Capture | Refund | Settlement | Balance | Reads as |
|---------|--------:|-------:|-----------:|--------:|----------|
| processor_receivable | +10000 | −2500 | −10000 | −2500 | we owe the network $25.00 for the refund |
| merchant_payable (m1) | −9680 | +2500 | | −7180 | we owe the merchant $71.80 |
| fee_revenue | −320 | | | −320 | we earned $3.20 |
| bank_cash | | | +9950 | +9950 | $99.50 arrived |
| network_fee_expense | | | +50 | +50 | the network kept $0.50 |
| **sum** | **0** | **0** | **0** | **0** | |

Assets (−25.00 + 99.50) plus expenses (0.50) equal liabilities (71.80) plus revenue (3.20):
75.00 = 75.00.

`go test` checks this table: `Example` in `internal/ledger/example_test.go` posts the same three
transactions and must print exactly these balances.

## Why authorization posts nothing

An authorization asks the cardholder's bank to *reserve* funds. No money moves: nobody owes
anybody anything yet, and the hold can expire or be voided without trace. Booking it would mean
recording a receivable that might never exist, then reversing it on every void and expiry. So
the payment row records the authorized amount (the limit for capture), and the ledger starts at
capture, when the network actually owes us money.

## Corrections

Nothing in the ledger is ever updated or deleted. A wrong transaction is undone with
`ledger.Reverse`, which posts the same lines with flipped signs and links back to the original
through `reverses_id`. A transaction can be reversed only once, and a reversal cannot itself be
reversed. If the original was partly right, post a new correct transaction after the reversal.

## What the database enforces

These hold even if the Go code is wrong, or someone writes SQL by hand:

| Rule | Mechanism |
|------|-----------|
| ≥ 2 entries per journal transaction, sum 0 per currency | Deferred constraint triggers on `journal_transactions` and `entries`, checked at COMMIT |
| Entry currency = account currency | Composite FK `(account_id, currency) → accounts (id, currency)` |
| No zero entries | `CHECK (amount <> 0)` |
| Accounts, journal transactions, entries are append-only | `BEFORE UPDATE OR DELETE` row triggers plus `BEFORE TRUNCATE` statement triggers |
| Chart of accounts | `CHECK` on (code, type, merchant ownership) |
| One account per (merchant, code, currency), platform included | `UNIQUE NULLS NOT DISTINCT` |
| An external event (e.g. a bank settlement line) posts once | `UNIQUE (external_ref)` |
| A transaction is reversed at most once | `UNIQUE (reverses_id)` |

Errors raised by the triggers set the constraint name (`journal_transaction_balanced`,
`entries_append_only`, …), so Go code and tests can tell which rule fired.

`ledgerline audit` (`make audit`) checks the stored data directly: global sum per currency,
per-transaction balance, entry counts, and currency matches. It runs in one REPEATABLE READ
snapshot.

## Known gaps

- A *balanced* set of entries can still be appended to a journal transaction committed earlier:
  the trigger re-checks the balance, but nothing seals old transactions. Sealing would need
  either a per-transaction "closed" marker or comparing creation transaction IDs, which is
  brittle under savepoints.
- Triggers do not stop a table owner or superuser who disables them. The application should
  connect as a role with only `INSERT`/`SELECT` on ledger tables. Separate roles are not set up
  yet.
- `ledger.Memory` rejects a post that would push a balance outside int64, while Postgres
  would store it and fail later, when the balance is read. Payment amounts will be capped (M2)
  far below that.
- Settlement covers captures only. Refunds leave `processor_receivable` negative until a refund
  settlement line would net them out, and that is not implemented.
