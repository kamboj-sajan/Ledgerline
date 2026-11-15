# Architecture decision records

Short records of decisions that someone could reasonably have made differently: the context,
the decision, why, and what was rejected. Numbered in the order they were written.

| ADR | Decision | Milestone |
|-----|----------|-----------|
| [0001](0001-signed-amounts.md) | Signed amounts (debit +, credit −) instead of debit/credit columns | M1 |
| [0002](0002-fees-on-refunds.md) | Refunds do not return the processing fee | M1 |
| [0003](0003-locking.md) | `SELECT … FOR UPDATE` on the payment row rather than conditional UPDATEs | M2 |
| [0004](0004-idempotency.md) | Idempotency keys: one transaction where possible, recovery points for authorize | M2 |

Still to come: outbox versus dual write (M3).
