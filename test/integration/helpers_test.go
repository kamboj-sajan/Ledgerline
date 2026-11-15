//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// constraintOf returns the name of the constraint a Postgres error reports,
// or "" if err is not a constraint error. The ledger's triggers set this
// field too (RAISE … USING CONSTRAINT), so tests can tell exactly which rule
// rejected a write.
func constraintOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

func mustExec(t *testing.T, db *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// inTx runs statements in one transaction and returns the first error,
// whether it came from a statement or from COMMIT. failedAt says which:
// "exec" for a statement error, "commit" for a COMMIT error, "" for success.
// Deferred constraint triggers only fail at commit, so tests use failedAt to
// show that the check really is deferred.
func inTx(ctx context.Context, db *pgxpool.Pool, stmts ...string) (failedAt string, err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return "begin", err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return "exec", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "commit", err
	}
	return "", nil
}

// seedLedgerAccounts creates merchant m1 and a small set of accounts with
// fixed IDs for the raw-SQL schema tests.
func seedLedgerAccounts(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	mustExec(t, db, `INSERT INTO merchants (id, name, api_key_hash) VALUES ('m1', 'Merchant One', sha256('k1'))`)
	mustExec(t, db, `INSERT INTO accounts (id, merchant_id, code, type, currency) VALUES
		('recv_usd', NULL, 'processor_receivable', 'asset', 'USD'),
		('pay_usd',  'm1', 'merchant_payable',     'liability', 'USD'),
		('recv_eur', NULL, 'processor_receivable', 'asset', 'EUR'),
		('pay_eur',  'm1', 'merchant_payable',     'liability', 'EUR')`)
}

func rowCount(t *testing.T, db *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
