//go:build integration

package integration

import (
	"testing"
)

// These tests talk to the schema with raw SQL and bypass the Go ledger code
// entirely. They show that the database rejects bad ledger writes on its own,
// so a bug in Go (or a hand-written fix in psql) cannot corrupt the books.

func TestSchemaBalanceIsCheckedAtCommit(t *testing.T) {
	const jt = `INSERT INTO journal_transactions (id, kind) VALUES ('jt1', 'capture')`
	tests := []struct {
		name           string
		stmts          []string
		wantConstraint string // "" means the commit must succeed
	}{
		{
			name: "balanced two-entry transaction commits",
			stmts: []string{jt,
				`INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES ('jt1', 'recv_usd', 100, 'USD')`,
				`INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES ('jt1', 'pay_usd', -100, 'USD')`},
		},
		{
			name: "balanced in each of two currencies commits",
			stmts: []string{jt,
				`INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES
					('jt1', 'recv_usd', 100, 'USD'), ('jt1', 'pay_usd', -100, 'USD'),
					('jt1', 'recv_eur', 7, 'EUR'),   ('jt1', 'pay_eur', -7, 'EUR')`},
		},
		{
			name:           "transaction with no entries",
			stmts:          []string{jt},
			wantConstraint: "journal_transaction_min_entries",
		},
		{
			name: "single entry",
			stmts: []string{jt,
				`INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES ('jt1', 'recv_usd', 100, 'USD')`},
			wantConstraint: "journal_transaction_min_entries",
		},
		{
			name: "two entries off by one",
			stmts: []string{jt,
				`INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES ('jt1', 'recv_usd', 100, 'USD')`,
				`INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES ('jt1', 'pay_usd', -99, 'USD')`},
			wantConstraint: "journal_transaction_balanced",
		},
		{
			name: "sums to zero overall but not per currency",
			stmts: []string{jt,
				`INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES
					('jt1', 'recv_usd', 100, 'USD'), ('jt1', 'pay_eur', -100, 'EUR')`},
			wantConstraint: "journal_transaction_balanced",
		},
		{
			name: "huge entries whose bigint sum would overflow still balance exactly",
			stmts: []string{jt,
				`INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES
					('jt1', 'recv_usd', 9223372036854775807, 'USD'), ('jt1', 'recv_usd', 9223372036854775807, 'USD'),
					('jt1', 'pay_usd', -9223372036854775807, 'USD'), ('jt1', 'pay_usd', -9223372036854775807, 'USD')`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := pg.NewPool(t)
			seedLedgerAccounts(t, db)

			failedAt, err := inTx(t.Context(), db, tt.stmts...)
			if tt.wantConstraint == "" {
				if err != nil {
					t.Fatalf("want commit to succeed, got error at %s: %v", failedAt, err)
				}
				return
			}
			if failedAt != "commit" {
				// Every INSERT must succeed: the check is deferred to COMMIT so a
				// transaction can insert its entries one at a time.
				t.Fatalf("want failure at commit, got failedAt=%q err=%v", failedAt, err)
			}
			if got := constraintOf(err); got != tt.wantConstraint {
				t.Fatalf("constraint = %q, want %q (err: %v)", got, tt.wantConstraint, err)
			}
			if n := rowCount(t, db, "entries"); n != 0 {
				t.Fatalf("%d entries persisted after a rejected commit", n)
			}
		})
	}
}

func TestSchemaRejectsUnbalancedAppendToCommittedTransaction(t *testing.T) {
	db := pg.NewPool(t)
	seedLedgerAccounts(t, db)
	if at, err := inTx(t.Context(), db,
		`INSERT INTO journal_transactions (id, kind) VALUES ('jt1', 'capture')`,
		`INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES ('jt1', 'recv_usd', 100, 'USD'), ('jt1', 'pay_usd', -100, 'USD')`,
	); err != nil {
		t.Fatalf("seed transaction failed at %s: %v", at, err)
	}

	// A later database transaction adds one entry to the committed jt1. The
	// trigger on entries re-checks jt1 as a whole and rejects it.
	at, err := inTx(t.Context(), db,
		`INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES ('jt1', 'recv_usd', 1, 'USD')`)
	if at != "commit" || constraintOf(err) != "journal_transaction_balanced" {
		t.Fatalf("append: failedAt=%q constraint=%q err=%v; want commit / journal_transaction_balanced", at, constraintOf(err), err)
	}
}

func TestSchemaRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name           string
		sql            string
		wantConstraint string
	}{
		{
			name:           "entry currency differs from account currency",
			sql:            `INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES ('jt1', 'recv_usd', 100, 'EUR')`,
			wantConstraint: "entries_account_currency",
		},
		{
			name:           "zero amount",
			sql:            `INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES ('jt1', 'recv_usd', 0, 'USD')`,
			wantConstraint: "entries_amount_nonzero",
		},
		{
			name:           "unknown account",
			sql:            `INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES ('jt1', 'nope', 100, 'USD')`,
			wantConstraint: "entries_account_currency",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := pg.NewPool(t)
			seedLedgerAccounts(t, db)
			// These are ordinary (non-deferred) constraints, so the INSERT
			// itself fails, before COMMIT.
			at, err := inTx(t.Context(), db, `INSERT INTO journal_transactions (id, kind) VALUES ('jt1', 'capture')`, tt.sql)
			if at != "exec" || constraintOf(err) != tt.wantConstraint {
				t.Fatalf("failedAt=%q constraint=%q err=%v; want exec / %s", at, constraintOf(err), err, tt.wantConstraint)
			}
		})
	}
}

func TestSchemaIsAppendOnly(t *testing.T) {
	tests := []struct {
		sql            string
		wantConstraint string
	}{
		{`UPDATE entries SET amount = amount * 2`, "entries_append_only"},
		{`DELETE FROM entries`, "entries_append_only"},
		{`TRUNCATE entries`, "entries_append_only"},
		{`UPDATE journal_transactions SET kind = 'refund'`, "journal_transactions_append_only"},
		{`DELETE FROM journal_transactions`, "journal_transactions_append_only"},
		{`TRUNCATE journal_transactions CASCADE`, "journal_transactions_append_only"},
		{`UPDATE accounts SET currency = 'EUR' WHERE id = 'recv_usd'`, "accounts_append_only"},
		{`DELETE FROM accounts WHERE id = 'recv_eur'`, "accounts_append_only"},
		{`TRUNCATE accounts CASCADE`, "accounts_append_only"},
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			db := pg.NewPool(t)
			seedLedgerAccounts(t, db)
			if at, err := inTx(t.Context(), db,
				`INSERT INTO journal_transactions (id, kind) VALUES ('jt1', 'capture')`,
				`INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES ('jt1', 'recv_usd', 100, 'USD'), ('jt1', 'pay_usd', -100, 'USD')`,
			); err != nil {
				t.Fatalf("seed failed at %s: %v", at, err)
			}

			_, err := db.Exec(t.Context(), tt.sql)
			if got := constraintOf(err); got != tt.wantConstraint {
				t.Fatalf("constraint = %q, want %q (err: %v)", got, tt.wantConstraint, err)
			}
			if n := rowCount(t, db, "entries"); n != 2 {
				t.Fatalf("entries count = %d after rejected %q, want 2", n, tt.sql)
			}
		})
	}
}

func TestSchemaChartOfAccounts(t *testing.T) {
	tests := []struct {
		name           string
		sql            string
		wantConstraint string
	}{
		{
			name:           "merchant_payable typed as an asset",
			sql:            `INSERT INTO accounts (id, merchant_id, code, type, currency) VALUES ('a', 'm1', 'merchant_payable', 'asset', 'GBP')`,
			wantConstraint: "accounts_chart",
		},
		{
			name:           "merchant_payable without a merchant",
			sql:            `INSERT INTO accounts (id, merchant_id, code, type, currency) VALUES ('a', NULL, 'merchant_payable', 'liability', 'GBP')`,
			wantConstraint: "accounts_chart",
		},
		{
			name:           "platform account owned by a merchant",
			sql:            `INSERT INTO accounts (id, merchant_id, code, type, currency) VALUES ('a', 'm1', 'fee_revenue', 'revenue', 'GBP')`,
			wantConstraint: "accounts_chart",
		},
		{
			name:           "unknown account code",
			sql:            `INSERT INTO accounts (id, merchant_id, code, type, currency) VALUES ('a', NULL, 'suspense', 'asset', 'GBP')`,
			wantConstraint: "accounts_chart",
		},
		{
			// Without NULLS NOT DISTINCT, two NULL merchant_ids would not
			// conflict and the platform could end up with two receivables.
			name:           "second platform receivable in the same currency",
			sql:            `INSERT INTO accounts (id, merchant_id, code, type, currency) VALUES ('a', NULL, 'processor_receivable', 'asset', 'USD')`,
			wantConstraint: "accounts_key",
		},
		{
			name:           "second payable for the same merchant and currency",
			sql:            `INSERT INTO accounts (id, merchant_id, code, type, currency) VALUES ('a', 'm1', 'merchant_payable', 'liability', 'USD')`,
			wantConstraint: "accounts_key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := pg.NewPool(t)
			seedLedgerAccounts(t, db)
			_, err := db.Exec(t.Context(), tt.sql)
			if got := constraintOf(err); got != tt.wantConstraint {
				t.Fatalf("constraint = %q, want %q (err: %v)", got, tt.wantConstraint, err)
			}
		})
	}
}

func TestSchemaExternalRefAndReversalUniqueness(t *testing.T) {
	db := pg.NewPool(t)
	seedLedgerAccounts(t, db)
	balanced := func(id, kind, extra string) []string {
		return []string{
			`INSERT INTO journal_transactions (id, kind, external_ref, reverses_id) VALUES ('` + id + `', '` + kind + `', ` + extra + `)`,
			`INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES ('` + id + `', 'recv_usd', 5, 'USD'), ('` + id + `', 'pay_usd', -5, 'USD')`,
		}
	}
	steps := []struct {
		name           string
		stmts          []string
		wantConstraint string
	}{
		{"first settlement", balanced("s1", "settlement", "'bank-ref-1', NULL"), ""},
		{"same external_ref again", balanced("s2", "settlement", "'bank-ref-1', NULL"), "journal_transactions_external_ref_key"},
		{"NULL external_refs never conflict (1)", balanced("c1", "capture", "NULL, NULL"), ""},
		{"NULL external_refs never conflict (2)", balanced("c2", "capture", "NULL, NULL"), ""},
		{"reversal of c1", balanced("r1", "reversal", "NULL, 'c1'"), ""},
		{"second reversal of c1", balanced("r2", "reversal", "NULL, 'c1'"), "journal_transactions_reverses_id_key"},
		{"reversal kind without a target", balanced("r3", "reversal", "NULL, NULL"), "journal_transactions_reversal_link"},
		{"reverses_id on a non-reversal", balanced("x1", "capture", "NULL, 'c2'"), "journal_transactions_reversal_link"},
	}
	for _, s := range steps {
		_, err := inTx(t.Context(), db, s.stmts...)
		if got := constraintOf(err); got != s.wantConstraint {
			t.Fatalf("%s: constraint = %q, want %q (err: %v)", s.name, got, s.wantConstraint, err)
		}
	}
}
