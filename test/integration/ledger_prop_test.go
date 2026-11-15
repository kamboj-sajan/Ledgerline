//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/kamboj-sajan/ledgerline/internal/id"
	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/ledger/ledgertest"
	"github.com/kamboj-sajan/ledgerline/internal/money"
)

// TestPropLedgerPostgres runs the same stateful property test as
// TestPropLedgerMemory against a real Postgres. Each generated case gets its
// own freshly cloned database.
//
// Valid transactions go through ledger.Post. Malformed ones (those that fail
// Tx.Validate) skip Go validation and are written with raw SQL, so the
// schema's constraints and triggers, and nothing else, must reject them.
func TestPropLedgerPostgres(t *testing.T) {
	var stats ledgertest.Stats
	start := time.Now()
	rapid.Check(t, func(rt *rapid.T) {
		db := pg.NewPool(rt)
		for _, m := range ledgertest.Merchants() {
			if _, err := db.Exec(rt.Context(), `INSERT INTO merchants (id, name, api_key_hash) VALUES ($1, $1, sha256(convert_to($1, 'UTF8')))`, m); err != nil {
				rt.Fatalf("seeding merchant: %v", err)
			}
		}
		ledgertest.Run(rt, &pgSystem{db: db}, &stats)
	})
	t.Logf("operations: %d (%d posted, %d rejected) in %s",
		stats.Ops, stats.Posted, stats.Rejected, time.Since(start).Round(time.Millisecond))
}

type pgSystem struct{ db *pgxpool.Pool }

func (s *pgSystem) Post(t *rapid.T, lt ledger.Tx) (string, error) {
	ctx := t.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var jtID string
	if lt.Validate() != nil {
		jtID, err = postRaw(ctx, tx, lt)
	} else {
		jtID, err = ledger.Post(ctx, tx, lt)
	}
	if err != nil {
		return "", err
	}
	return jtID, tx.Commit(ctx)
}

// postRaw writes lt with plain INSERTs and no Go-side checks.
func postRaw(ctx context.Context, tx pgx.Tx, lt ledger.Tx) (string, error) {
	jtID := id.New("jtx")
	if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, kind, payment_id) VALUES ($1, $2, $3)`,
		jtID, string(lt.Kind), lt.PaymentID); err != nil {
		return "", err
	}
	for _, p := range lt.Postings {
		var merchant *string
		if p.Account.MerchantID != "" {
			merchant = &p.Account.MerchantID
		}
		// Raw account creation too: the chart-of-accounts CHECK must reject
		// unknown codes and merchant/platform mix-ups.
		var acctID string
		err := tx.QueryRow(ctx, `
			WITH ins AS (
				INSERT INTO accounts (id, merchant_id, code, type, currency)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT ON CONSTRAINT accounts_key DO NOTHING
				RETURNING id)
			SELECT id FROM ins
			UNION ALL
			SELECT id FROM accounts WHERE merchant_id IS NOT DISTINCT FROM $2 AND code = $3 AND currency = $5
			LIMIT 1`,
			id.New("acct"), merchant, string(p.Account.Code), string(p.Account.Code.Type()), p.Account.Currency.Code(),
		).Scan(&acctID)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES ($1, $2, $3, $4)`,
			jtID, acctID, p.Amount.Amount(), p.Amount.Currency().Code()); err != nil {
			return "", err
		}
	}
	return jtID, nil
}

func (s *pgSystem) Balances(t *rapid.T) map[ledger.AccountKey]money.Money {
	rows, err := s.db.Query(t.Context(), `
		SELECT a.merchant_id, a.code, a.currency, COALESCE(sum(e.amount), 0)::bigint
		FROM accounts a LEFT JOIN entries e ON e.account_id = a.id
		GROUP BY a.id`)
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	defer rows.Close()
	out := make(map[ledger.AccountKey]money.Money)
	for rows.Next() {
		var (
			merchant       *string
			code, currency string
			total          int64
		)
		if err := rows.Scan(&merchant, &code, &currency, &total); err != nil {
			t.Fatalf("balances: %v", err)
		}
		c, err := money.ParseCurrency(currency)
		if err != nil {
			t.Fatalf("balances: %v", err)
		}
		k := ledger.AccountKey{Code: ledger.AccountCode(code), Currency: c}
		if merchant != nil {
			k.MerchantID = *merchant
		}
		out[k] = money.New(total, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("balances: %v", err)
	}
	return out
}

// CheckGlobal runs the full SQL audit after every step: every stored
// transaction balances, has at least 2 entries and matches account
// currencies, and the global sum per currency is zero.
func (s *pgSystem) CheckGlobal(t *rapid.T) {
	r, err := ledger.Audit(t.Context(), s.db)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !r.OK() {
		t.Fatalf("audit failed: %+v", r)
	}
}

// Compile-time check that pgSystem implements the interface.
var _ ledgertest.System = (*pgSystem)(nil)
