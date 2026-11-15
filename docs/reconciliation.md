# Reconciliation

Every captured payment creates a receivable: the card network owes us the captured amount. The
network pays it out a day later and sends a settlement file, one row per capture. Reconciliation
checks that file against the ledger, posts the settlements that match, and lists everything that
does not.

## Commands

```sh
banksim   -date 2026-11-15 -seed 7 -fault-rate 0.4 -unknown 1 -out settlement.csv -manifest manifest.json
reconcile -date 2026-11-15 -file settlement.csv -report report.json [-window-days 3] [-dry-run]
```

`banksim` plays the bank. It reads the day's captures from the ledger and writes the settlement
file (`reference, amount, currency, fee, settled_at`, with decimal amounts). With a seeded
probability, it gives each capture at most one fault: a missing row, a wrong amount, a wrong
currency, a duplicate row, or a row settled a day late. It adds rows with references the ledger
has never seen, shuffles the file, and writes `manifest.json`: the ground truth of every fault it
injected.

`reconcile` classifies every row and every capture, posts the matched ones, and writes a JSON
report and a readable one.

## Classification

Rows are matched to captures by `reference`, which is the payment ID. A capture is expected to
settle on T+1.

| Class | Meaning | Posted? |
|-------|---------|---------|
| `matched` | Amount and currency agree, settled by the end of T+1 | yes |
| `late` | Agrees, but settled after T+1 and within T+N (`-window-days`, default 3) | yes |
| `amount_mismatch` | Same reference, different amount | no |
| `currency_mismatch` | Same reference, different currency | no |
| `duplicate` | A second or later row for a reference that already has one | no |
| `missing_in_ledger` | A row for a reference with no capture that day, or a row settled outside the window | no |
| `missing_in_bank` | A capture with no accepted row | n/a |

For each reference, rows are taken in file order: the first is classified against the capture,
any later ones are `duplicate`. Classification is a pure function (`recon.Reconcile`), and the
result does not depend on row order (a property test checks that).

## Posting and idempotency

Each matched or late row becomes a settlement journal transaction:

```
debit  bank_cash             amount − fee
debit  network_fee_expense   fee
credit processor_receivable  amount
```

Its `external_ref` is `settlement:<reference>`, and `journal_transactions.external_ref` is
`UNIQUE`. Running reconciliation again on the same file therefore cannot post twice. The
database rejects the second insert, and `reconcile` reports it as "already posted". Mismatches
are not posted. They need a human, and fixing one means correcting the data, not the ledger.

## A real run

The real binaries on the Compose stack: 12 captures via the API, `banksim -seed 7
-fault-rate 0.4 -unknown 1`. The manifest listed 3 × `wrong_currency`, 2 × `duplicate_row` and
1 × `unknown_reference`. The first `reconcile` printed:

```
  matched                 9
  late                    0
  missing_in_bank         0
  missing_in_ledger       1
  amount_mismatch         0
  currency_mismatch       3
  duplicate               2

posted 9 settlement(s), 0 already posted earlier
```

Running it again: `posted 0 settlement(s), 9 already posted earlier`. `ledgerline audit` then
printed `AUDIT PASSED`.

## Tests

- `TestReconcileMatchesManifestAcrossSeeds`: 2,000 seeds of random captures in four currencies,
  random fault rates (45,217 injected faults covering all six kinds). The file goes through CSV
  and back, and the report must equal the manifest exactly: every fault found with the right
  class, every unfaulted capture `matched`, no false positives.
- `TestPropReconcileIndependentOfRowOrder`, `TestReconcileWindow` (T+1, T+2, T+3 and outside
  the window), `TestReadCSVRejectsBadRows`.
- `TestReconcileAgainstLedger` (Postgres): 5 seeds with real captures via the API. The report
  matches the manifest; the settlements posted equal matched + late; `bank_cash` and
  `network_fee_expense` equal the posted amounts and fees; both audits pass; a second run posts
  nothing and leaves the ledger unchanged.
- Mutation check: treating late rows as on time fails on the first seed.

## Not handled

- **Refund settlement lines.** The network nets refunds against payouts. Here refunds leave
  `processor_receivable` negative, and no file line clears them.
- **One file per capture day.** A late row from an earlier day's captures, arriving in today's
  file, is reported as `missing_in_ledger` for today rather than matched to its own day.
- **No exception workflow.** Mismatches are reported, and resolving them (contacting the
  network, posting an adjustment) is manual.
- **Partial settlements, split payouts and FX settlement** are out of scope: one row settles one
  capture in full, in the capture's currency.
