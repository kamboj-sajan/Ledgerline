package ledger

import (
	"context"
	"fmt"
	"io"

	"github.com/kamboj-sajan/ledgerline/internal/store"
)

// auditListLimit caps how many offending IDs a report lists per check. The
// counts are always exact.
const auditListLimit = 100

// AuditReport is the result of checking ledger-wide invariants with SQL,
// directly against stored rows. The schema's triggers should make every
// problem impossible; the audit exists to prove that from the data itself, and
// to catch anything written while the triggers were bypassed (a superuser can
// disable triggers, for example).
type AuditReport struct {
	Transactions int64
	Entries      int64

	// CurrencyTotals is the sum of every entry per currency. Each must be 0:
	// every debit somewhere is a credit somewhere else.
	CurrencyTotals []CurrencyTotal

	UnbalancedCount       int64
	Unbalanced            []string // journal transactions not summing to 0 in some currency
	TooFewEntriesCount    int64
	TooFewEntries         []string // journal transactions with fewer than 2 entries
	CurrencyMismatchCount int64
	CurrencyMismatches    []int64 // entry IDs whose currency differs from their account's
}

// CurrencyTotal is the global sum of entries in one currency. Total is the
// exact numeric sum as text, so an out-of-range value is still reported.
type CurrencyTotal struct {
	Currency string
	Total    string
}

// OK reports whether every check passed.
func (r AuditReport) OK() bool {
	for _, c := range r.CurrencyTotals {
		if c.Total != "0" {
			return false
		}
	}
	return r.UnbalancedCount == 0 && r.TooFewEntriesCount == 0 && r.CurrencyMismatchCount == 0
}

// Audit runs every check. Run it in a REPEATABLE READ transaction for a
// consistent snapshot while other writes are happening; `ledgerline audit`
// does.
func Audit(ctx context.Context, db DB) (AuditReport, error) {
	var r AuditReport
	if err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM journal_transactions), (SELECT count(*) FROM entries)`).
		Scan(&r.Transactions, &r.Entries); err != nil {
		return r, fmt.Errorf("ledger: audit counts: %w", err)
	}

	rows, err := db.Query(ctx, `SELECT currency, sum(amount)::text FROM entries GROUP BY currency ORDER BY currency`)
	if err != nil {
		return r, fmt.Errorf("ledger: audit currency totals: %w", err)
	}
	for rows.Next() {
		var c CurrencyTotal
		if err := rows.Scan(&c.Currency, &c.Total); err != nil {
			rows.Close()
			return r, fmt.Errorf("ledger: audit currency totals: %w", err)
		}
		r.CurrencyTotals = append(r.CurrencyTotals, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return r, fmt.Errorf("ledger: audit currency totals: %w", err)
	}

	var err2 error
	r.UnbalancedCount, r.Unbalanced, err2 = store.CountAndList[string](ctx, db, `
		SELECT DISTINCT transaction_id FROM entries
		GROUP BY transaction_id, currency HAVING sum(amount) <> 0`, auditListLimit)
	if err2 != nil {
		return r, fmt.Errorf("ledger: audit unbalanced: %w", err2)
	}
	r.TooFewEntriesCount, r.TooFewEntries, err2 = store.CountAndList[string](ctx, db, `
		SELECT jt.id FROM journal_transactions jt LEFT JOIN entries e ON e.transaction_id = jt.id
		GROUP BY jt.id HAVING count(e.id) < 2`, auditListLimit)
	if err2 != nil {
		return r, fmt.Errorf("ledger: audit entry counts: %w", err2)
	}
	r.CurrencyMismatchCount, r.CurrencyMismatches, err2 = store.CountAndList[int64](ctx, db, `
		SELECT e.id FROM entries e JOIN accounts a ON a.id = e.account_id
		WHERE a.currency <> e.currency`, auditListLimit)
	if err2 != nil {
		return r, fmt.Errorf("ledger: audit currencies: %w", err2)
	}
	return r, nil
}

// Write prints the report for humans.
func (r AuditReport) Write(w io.Writer) {
	fmt.Fprintf(w, "journal transactions: %d\nentries:              %d\n", r.Transactions, r.Entries)
	fmt.Fprintln(w, "global sum per currency (must be 0):")
	if len(r.CurrencyTotals) == 0 {
		fmt.Fprintln(w, "  (no entries)")
	}
	for _, c := range r.CurrencyTotals {
		mark := "ok"
		if c.Total != "0" {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "  %s %s  %s\n", c.Currency, c.Total, mark)
	}
	check := func(name string, n int64, sample any) {
		if n == 0 {
			fmt.Fprintf(w, "%-44s ok\n", name+":")
			return
		}
		fmt.Fprintf(w, "%-44s FAIL (%d) e.g. %v\n", name+":", n, sample)
	}
	check("transactions balanced per currency", r.UnbalancedCount, r.Unbalanced)
	check("transactions with at least 2 entries", r.TooFewEntriesCount, r.TooFewEntries)
	check("entry currency matches account currency", r.CurrencyMismatchCount, r.CurrencyMismatches)
}
