# ADR 0002: Refunds do not return the processing fee

**Status:** accepted (M1)

## Context

When a captured payment is refunded, in full or in part, somebody has to bear the processing fee
we charged at capture. Either the refund also reverses (part of) our `fee_revenue`, or it comes
entirely out of what we owe the merchant.

## Decision

A refund of R posts exactly two lines and never touches `fee_revenue`:

```
debit  merchant_payable      R
credit processor_receivable  R
```

The fee charged at capture is kept, whether the refund is partial or full.

## Why

- **The cost we pay is not returned to us either.** The work of authorizing, capturing and
  refunding happened. Card networks generally do not return their own fees on a refund, so
  returning ours would mean the platform pays for the merchant's refunds.
- **It is how mainstream processors price refunds.** Stripe, for example, documents that its fee
  on the original payment is not returned when the payment is refunded.
- **No rounding problem.** A proportional fee refund has to allocate an integer fee across
  several partial refunds so that the parts sum exactly to the original fee. Floor, ceiling and
  largest-remainder rules all give different answers, and every one of them needs its own tests.
  Not refunding fees avoids the question.

## Consequences

- After a full refund the merchant's balance is negative by the fee. For a $100.00 capture with
  a $3.20 fee, `merchant_payable` goes from -9680 (we owe $96.80) to +320 (they owe us $3.20).
  Collecting that, by netting it against future payouts or by invoicing, is out of scope (see
  README, "Not implemented").
- The refundable amount is limited by `captured_amount`, not by the merchant's balance. A
  merchant with no balance left can still refund; their payable just goes further negative.
  This rule belongs to payments (M2), not the ledger.

## Rejected

- **Proportional fee refund** (refund R also returns `fee × R / captured`): the allocation
  problem above, and the platform loses money on every refund.
- **Return the fee only on a full refund:** it makes the last partial refund special, and the
  result depends on whether a merchant refunds in one step or several.
