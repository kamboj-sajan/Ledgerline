package recon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/money"
	"github.com/kamboj-sajan/ledgerline/internal/store"
)

// Captures reads the captures posted to the ledger in [from, to): the
// processor_receivable debit of each capture journal transaction that has
// not been reversed. That is what the network owes us, and what the bank
// should settle.
func Captures(ctx context.Context, db store.DB, from, to time.Time) ([]Capture, error) {
	rows, err := db.Query(ctx, `
		SELECT jt.payment_id, e.amount, e.currency, jt.created_at
		FROM journal_transactions jt
		JOIN entries e  ON e.transaction_id = jt.id
		JOIN accounts a ON a.id = e.account_id
		WHERE jt.kind = 'capture' AND a.code = 'processor_receivable'
		  AND jt.created_at >= $1 AND jt.created_at < $2
		  AND NOT EXISTS (SELECT 1 FROM journal_transactions r WHERE r.reverses_id = jt.id)
		ORDER BY jt.created_at, jt.id`, from, to)
	if err != nil {
		return nil, fmt.Errorf("recon: reading captures: %w", err)
	}
	defer rows.Close()
	var out []Capture
	for rows.Next() {
		var (
			c        Capture
			amount   int64
			currency string
		)
		if err := rows.Scan(&c.PaymentID, &amount, &currency, &c.CapturedAt); err != nil {
			return nil, fmt.Errorf("recon: reading captures: %w", err)
		}
		cur, err := money.ParseCurrency(currency)
		if err != nil {
			return nil, err
		}
		c.Amount = money.New(amount, cur)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DayCaptures is Captures for one UTC calendar day.
func DayCaptures(ctx context.Context, db store.DB, day time.Time) ([]Capture, error) {
	return Captures(ctx, db, day, day.AddDate(0, 0, 1))
}

// ExternalRef is the ledger's external_ref for a bank settlement line.
func ExternalRef(reference string) string { return "settlement:" + reference }

// Post writes a settlement journal transaction for every matched or late
// item: debit bank_cash (amount - fee), debit network_fee_expense (fee),
// credit processor_receivable (amount). Each goes in its own transaction.
// The settlement's external_ref is unique in the ledger, so an item posted
// by an earlier run is rejected by the database and counted as already
// posted. Running reconciliation again therefore never posts twice.
func Post(ctx context.Context, db *pgxpool.Pool, rep *Report) error {
	rep.Posted, rep.AlreadyPosted = 0, 0
	for _, it := range rep.Items {
		if !it.Class.Posts() {
			continue
		}
		lt, err := ledger.Settlement(it.Reference, ExternalRef(it.Reference), it.row.Amount, it.row.Fee)
		if err != nil {
			return fmt.Errorf("recon: settlement for %s: %w", it.Reference, err)
		}
		posted, err := postOne(ctx, db, lt)
		if err != nil {
			return fmt.Errorf("recon: posting %s: %w", it.Reference, err)
		}
		if posted {
			rep.Posted++
		} else {
			rep.AlreadyPosted++
		}
	}
	return nil
}

func postOne(ctx context.Context, db *pgxpool.Pool, lt ledger.Tx) (bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := ledger.Post(ctx, tx, lt); errors.Is(err, ledger.ErrDuplicateExternalRef) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
