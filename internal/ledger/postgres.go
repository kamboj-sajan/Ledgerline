package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kamboj-sajan/ledgerline/internal/id"
	"github.com/kamboj-sajan/ledgerline/internal/money"
)

// DB is the query interface the read functions need. *pgxpool.Pool,
// *pgx.Conn and pgx.Tx all satisfy it, so a read can run inside or outside a
// transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Account is a stored account.
type Account struct {
	ID  string
	Key AccountKey
}

// StoredTx is a journal transaction as read back from the database.
type StoredTx struct {
	ID        string
	CreatedAt time.Time
	Tx
}

// EnsureAccount returns the account for k, creating it on first use.
//
// It is safe to call concurrently for the same key. If two transactions try
// to create the same account, the second INSERT waits for the first; once the
// first commits, ON CONFLICT DO NOTHING skips the insert and the SELECT (a new
// statement, so a new snapshot under READ COMMITTED) finds the winner's row.
func EnsureAccount(ctx context.Context, db DB, k AccountKey) (Account, error) {
	if err := k.validate(); err != nil {
		return Account{}, err
	}
	var merchant *string // NULL for platform accounts
	if k.MerchantID != "" {
		merchant = &k.MerchantID
	}
	_, err := db.Exec(ctx, `
		INSERT INTO accounts (id, merchant_id, code, type, currency)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT ON CONSTRAINT accounts_key DO NOTHING`,
		id.New("acct"), merchant, string(k.Code), string(k.Type()), k.Currency.Code())
	if err != nil {
		return Account{}, fmt.Errorf("ledger: creating account %s: %w", k, err)
	}

	// Two queries instead of "merchant_id IS NOT DISTINCT FROM $1", which
	// Postgres cannot answer from the accounts_key index.
	var acctID string
	if merchant == nil {
		err = db.QueryRow(ctx, `SELECT id FROM accounts WHERE merchant_id IS NULL AND code = $1 AND currency = $2`,
			string(k.Code), k.Currency.Code()).Scan(&acctID)
	} else {
		err = db.QueryRow(ctx, `SELECT id FROM accounts WHERE merchant_id = $1 AND code = $2 AND currency = $3`,
			*merchant, string(k.Code), k.Currency.Code()).Scan(&acctID)
	}
	if err != nil {
		return Account{}, fmt.Errorf("ledger: looking up account %s: %w", k, err)
	}
	return Account{ID: acctID, Key: k}, nil
}

// Post validates t and writes it inside tx, returning the new transaction's ID.
//
// It takes a pgx.Tx rather than a pool on purpose. A journal transaction must
// commit together with whatever caused it (a payment's state change, its
// outbox event), and the database's balance check runs at that COMMIT.
//
// Errors wrap ErrDuplicateExternalRef, ErrAlreadyReversed or
// ErrUnknownTransaction when the schema's uniqueness and reference rules
// reject the write, or a validation error from Tx.Validate. After a database
// error the transaction is aborted and the caller must roll it back.
func Post(ctx context.Context, tx pgx.Tx, t Tx) (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}
	jtID := id.New("jtx")
	if _, err := tx.Exec(ctx, `
		INSERT INTO journal_transactions (id, kind, payment_id, external_ref, reverses_id)
		VALUES ($1, $2, $3, $4, $5)`,
		jtID, string(t.Kind), nullIfEmpty(t.PaymentID), nullIfEmpty(t.ExternalRef), nullIfEmpty(t.ReversesID),
	); err != nil {
		return "", mapPostError(err)
	}

	accountIDs := make([]string, len(t.Postings))
	amounts := make([]int64, len(t.Postings))
	currencies := make([]string, len(t.Postings))
	seen := make(map[AccountKey]string, len(t.Postings))
	for i, p := range t.Postings {
		acctID, ok := seen[p.Account]
		if !ok {
			acct, err := EnsureAccount(ctx, tx, p.Account)
			if err != nil {
				return "", err
			}
			acctID = acct.ID
			seen[p.Account] = acctID
		}
		accountIDs[i], amounts[i], currencies[i] = acctID, p.Amount.Amount(), p.Amount.Currency().Code()
	}
	// One INSERT for all postings, keeping their order (WITH ORDINALITY), so
	// LoadTransaction returns them in the order they were posted.
	if _, err := tx.Exec(ctx, `
		INSERT INTO entries (transaction_id, account_id, amount, currency)
		SELECT $1, u.account_id, u.amount, u.currency
		FROM unnest($2::text[], $3::bigint[], $4::text[]) WITH ORDINALITY AS u(account_id, amount, currency, n)
		ORDER BY u.n`,
		jtID, accountIDs, amounts, currencies,
	); err != nil {
		return "", mapPostError(err)
	}
	return jtID, nil
}

func mapPostError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.ConstraintName {
		case "journal_transactions_external_ref_key":
			return fmt.Errorf("%w: %s", ErrDuplicateExternalRef, pgErr.Detail)
		case "journal_transactions_reverses_id_key":
			return fmt.Errorf("%w: %s", ErrAlreadyReversed, pgErr.Detail)
		case "journal_transactions_reverses_id_fkey":
			return fmt.Errorf("%w: %s", ErrUnknownTransaction, pgErr.Detail)
		}
	}
	return fmt.Errorf("ledger: posting: %w", err)
}

// Balance returns the signed (debit-positive) balance of the account with the
// given ID, computed from its entries. Ledgerline keeps no cached balances.
func Balance(ctx context.Context, db DB, accountID string) (Account, money.Money, error) {
	var (
		merchant       *string
		code, currency string
		total          int64
	)
	// sum() of bigint is numeric. The ::bigint cast raises an error rather
	// than wrapping if a balance ever leaves int64.
	err := db.QueryRow(ctx, `
		SELECT a.merchant_id, a.code, a.currency, COALESCE(sum(e.amount), 0)::bigint
		FROM accounts a LEFT JOIN entries e ON e.account_id = a.id
		WHERE a.id = $1
		GROUP BY a.id`, accountID).Scan(&merchant, &code, &currency, &total)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, money.Money{}, fmt.Errorf("%w: account %s", ErrUnknownAccount, accountID)
	}
	if err != nil {
		return Account{}, money.Money{}, fmt.Errorf("ledger: balance of %s: %w", accountID, err)
	}
	key, err := keyFromRow(merchant, code, currency)
	if err != nil {
		return Account{}, money.Money{}, err
	}
	return Account{ID: accountID, Key: key}, money.New(total, key.Currency), nil
}

// BalanceOf returns the signed balance of the account k. An account that has
// never been used has a zero balance.
func BalanceOf(ctx context.Context, db DB, k AccountKey) (money.Money, error) {
	if err := k.validate(); err != nil {
		return money.Money{}, err
	}
	var total int64
	var err error
	if k.MerchantID == "" {
		err = db.QueryRow(ctx, `
			SELECT COALESCE(sum(e.amount), 0)::bigint FROM accounts a JOIN entries e ON e.account_id = a.id
			WHERE a.merchant_id IS NULL AND a.code = $1 AND a.currency = $2`,
			string(k.Code), k.Currency.Code()).Scan(&total)
	} else {
		err = db.QueryRow(ctx, `
			SELECT COALESCE(sum(e.amount), 0)::bigint FROM accounts a JOIN entries e ON e.account_id = a.id
			WHERE a.merchant_id = $1 AND a.code = $2 AND a.currency = $3`,
			k.MerchantID, string(k.Code), k.Currency.Code()).Scan(&total)
	}
	if err != nil {
		return money.Money{}, fmt.Errorf("ledger: balance of %s: %w", k, err)
	}
	return money.New(total, k.Currency), nil
}

// LoadTransaction reads one journal transaction and its postings.
func LoadTransaction(ctx context.Context, db DB, txID string) (StoredTx, error) {
	txs, err := loadTransactions(ctx, db, `jt.id = $1`, txID)
	if err != nil {
		return StoredTx{}, err
	}
	if len(txs) == 0 {
		return StoredTx{}, fmt.Errorf("%w: %s", ErrUnknownTransaction, txID)
	}
	return txs[0], nil
}

// PaymentTransactions reads every journal transaction for a payment, oldest first.
func PaymentTransactions(ctx context.Context, db DB, paymentID string) ([]StoredTx, error) {
	return loadTransactions(ctx, db, `jt.payment_id = $1`, paymentID)
}

// loadTransactions runs one query joining transactions, entries and accounts,
// then groups the rows into transactions. where is a fixed SQL fragment chosen
// by the callers above, never user input.
func loadTransactions(ctx context.Context, db DB, where string, arg any) ([]StoredTx, error) {
	rows, err := db.Query(ctx, `
		SELECT jt.id, jt.kind, jt.payment_id, jt.external_ref, jt.reverses_id, jt.created_at,
		       a.merchant_id, a.code, a.currency, e.amount
		FROM journal_transactions jt
		JOIN entries e  ON e.transaction_id = jt.id
		JOIN accounts a ON a.id = e.account_id
		WHERE `+where+`
		ORDER BY jt.created_at, jt.id, e.id`, arg)
	if err != nil {
		return nil, fmt.Errorf("ledger: loading transactions: %w", err)
	}
	defer rows.Close()

	var out []StoredTx
	for rows.Next() {
		var (
			jtID, kind                       string
			paymentID, extRef, reverses, mID *string
			createdAt                        time.Time
			code, currency                   string
			amount                           int64
		)
		if err := rows.Scan(&jtID, &kind, &paymentID, &extRef, &reverses, &createdAt, &mID, &code, &currency, &amount); err != nil {
			return nil, fmt.Errorf("ledger: scanning transaction: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].ID != jtID {
			out = append(out, StoredTx{ID: jtID, CreatedAt: createdAt, Tx: Tx{
				Kind: Kind(kind), PaymentID: deref(paymentID), ExternalRef: deref(extRef), ReversesID: deref(reverses),
			}})
		}
		key, err := keyFromRow(mID, code, currency)
		if err != nil {
			return nil, err
		}
		cur := &out[len(out)-1]
		cur.Postings = append(cur.Postings, Posting{Account: key, Amount: money.New(amount, key.Currency)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: reading transactions: %w", err)
	}
	return out, nil
}

func keyFromRow(merchant *string, code, currency string) (AccountKey, error) {
	c, err := money.ParseCurrency(currency)
	if err != nil {
		return AccountKey{}, fmt.Errorf("ledger: stored account currency: %w", err)
	}
	return AccountKey{MerchantID: deref(merchant), Code: AccountCode(code), Currency: c}, nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
