package payments

import (
	"context"
	"fmt"
	"io"

	"github.com/kamboj-sajan/ledgerline/internal/store"
)

const auditListLimit = 100

// AuditReport cross-checks payments against the ledger. ledger.Audit proves
// the books balance. This proves the books agree with what the payments
// table says happened.
type AuditReport struct {
	Payments int64

	// Journal transactions whose payment_id names no payment.
	OrphanTransactionsCount int64
	OrphanTransactions      []string
	// Payments whose captured, fee or refunded amount differs from the
	// ledger, or that have more than one capture.
	LedgerMismatchCount int64
	LedgerMismatches    []string
	// Payments whose refunded_amount differs from the sum of their refunds.
	RefundMismatchCount int64
	RefundMismatches    []string
	// Payments with an entry in another merchant's account, or in another currency.
	ForeignEntryCount int64
	ForeignEntries    []string
}

// OK reports whether every check passed.
func (r AuditReport) OK() bool {
	return r.OrphanTransactionsCount == 0 && r.LedgerMismatchCount == 0 &&
		r.RefundMismatchCount == 0 && r.ForeignEntryCount == 0
}

// Audit runs the cross-checks. Like ledger.Audit, run it in one REPEATABLE
// READ transaction for a consistent snapshot.
func Audit(ctx context.Context, db store.DB) (AuditReport, error) {
	var r AuditReport
	if err := db.QueryRow(ctx, `SELECT count(*) FROM payments`).Scan(&r.Payments); err != nil {
		return r, fmt.Errorf("payments: audit: %w", err)
	}
	checks := []struct {
		count *int64
		ids   *[]string
		query string
	}{
		{&r.OrphanTransactionsCount, &r.OrphanTransactions, `
			SELECT jt.id FROM journal_transactions jt
			WHERE jt.payment_id IS NOT NULL
			  AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.id = jt.payment_id)`},
		// The ledger's view of each payment: the receivable debit of its capture
		// is the captured amount, the fee_revenue credit is the fee, and the
		// receivable credits of its refunds add up to the refunded amount.
		{&r.LedgerMismatchCount, &r.LedgerMismatches, `
			WITH l AS (
				SELECT jt.payment_id,
				       COALESCE(sum(e.amount)  FILTER (WHERE jt.kind = 'capture' AND a.code = 'processor_receivable'), 0) AS captured,
				       COALESCE(-sum(e.amount) FILTER (WHERE jt.kind = 'capture' AND a.code = 'fee_revenue'), 0)          AS fee,
				       COALESCE(-sum(e.amount) FILTER (WHERE jt.kind = 'refund'  AND a.code = 'processor_receivable'), 0) AS refunded,
				       count(DISTINCT jt.id)   FILTER (WHERE jt.kind = 'capture')                                         AS captures
				FROM journal_transactions jt
				JOIN entries e  ON e.transaction_id = jt.id
				JOIN accounts a ON a.id = e.account_id
				WHERE jt.payment_id IS NOT NULL
				GROUP BY jt.payment_id)
			SELECT p.id FROM payments p LEFT JOIN l ON l.payment_id = p.id
			WHERE p.captured_amount <> COALESCE(l.captured, 0)
			   OR p.fee_amount      <> COALESCE(l.fee, 0)
			   OR p.refunded_amount <> COALESCE(l.refunded, 0)
			   OR COALESCE(l.captures, 0) > 1`},
		{&r.RefundMismatchCount, &r.RefundMismatches, `
			SELECT p.id FROM payments p
			LEFT JOIN (SELECT payment_id, sum(amount) AS total FROM refunds GROUP BY payment_id) rf ON rf.payment_id = p.id
			WHERE p.refunded_amount <> COALESCE(rf.total, 0)`},
		{&r.ForeignEntryCount, &r.ForeignEntries, `
			SELECT DISTINCT p.id FROM payments p
			JOIN journal_transactions jt ON jt.payment_id = p.id
			JOIN entries e  ON e.transaction_id = jt.id
			JOIN accounts a ON a.id = e.account_id
			WHERE (a.merchant_id IS NOT NULL AND a.merchant_id <> p.merchant_id)
			   OR e.currency <> p.currency`},
	}
	for _, c := range checks {
		var err error
		if *c.count, *c.ids, err = store.CountAndList[string](ctx, db, c.query, auditListLimit); err != nil {
			return r, fmt.Errorf("payments: audit: %w", err)
		}
	}
	return r, nil
}

// Write prints the report for humans.
func (r AuditReport) Write(w io.Writer) {
	fmt.Fprintf(w, "payments:             %d\n", r.Payments)
	check := func(name string, n int64, sample []string) {
		if n == 0 {
			fmt.Fprintf(w, "%-44s ok\n", name+":")
			return
		}
		fmt.Fprintf(w, "%-44s FAIL (%d) e.g. %v\n", name+":", n, sample)
	}
	check("journal transactions name a real payment", r.OrphanTransactionsCount, r.OrphanTransactions)
	check("payment amounts agree with the ledger", r.LedgerMismatchCount, r.LedgerMismatches)
	check("refunded amount = sum of refunds", r.RefundMismatchCount, r.RefundMismatches)
	check("entries in the payment's merchant/currency", r.ForeignEntryCount, r.ForeignEntries)
}
