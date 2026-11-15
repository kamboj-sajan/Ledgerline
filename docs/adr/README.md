# Architecture decision records

Short records of decisions that someone could reasonably have made differently: the context,
the decision, why, and what was rejected. Numbered in the order they were written.

| ADR | Decision | Milestone |
|-----|----------|-----------|
| [0001](0001-signed-amounts.md) | Signed amounts (debit +, credit −) instead of debit/credit columns | M1 |
| [0002](0002-fees-on-refunds.md) | Refunds do not return the processing fee | M1 |

Still to come: locking strategy and idempotency design (M2), outbox versus dual write (M3).
