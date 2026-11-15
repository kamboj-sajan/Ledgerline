package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kamboj-sajan/ledgerline/internal/money"
	"github.com/kamboj-sajan/ledgerline/internal/store"
)

const columns = `id, merchant_id, status, currency, amount, authorized_amount, captured_amount,
	refunded_amount, fee_amount, customer_id, card_fingerprint, card_country, ip_country, metadata,
	failure_code, failure_message, acquirer_reference, fraud_decision, fraud_rules, created_at, updated_at`

// Insert stores a new payment and fills in its timestamps.
func Insert(ctx context.Context, db store.DB, p *Payment) error {
	meta, err := json.Marshal(p.Metadata)
	if err != nil {
		return fmt.Errorf("payments: encoding metadata: %w", err)
	}
	err = db.QueryRow(ctx, `
		INSERT INTO payments (id, merchant_id, status, currency, amount, customer_id,
			card_fingerprint, card_country, ip_country, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING created_at, updated_at`,
		p.ID, p.MerchantID, string(p.Status), p.Amount.Currency().Code(), p.Amount.Amount(), p.CustomerID,
		p.Card.Fingerprint, p.Card.Country, p.IPCountry, meta,
	).Scan(&p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("payments: inserting %s: %w", p.ID, err)
	}
	return nil
}

// Get reads a payment that belongs to merchantID. Another merchant's payment
// is reported as not found, the same as a missing one.
func Get(ctx context.Context, db store.DB, merchantID, id string) (Payment, error) {
	return scanOne(db.QueryRow(ctx, `SELECT `+columns+` FROM payments WHERE id = $1 AND merchant_id = $2`, id, merchantID))
}

// GetForUpdate reads a payment and locks its row until tx ends. Every state
// change goes through this lock, so concurrent operations on one payment are
// serialised (docs/adr/0003-locking.md).
func GetForUpdate(ctx context.Context, tx pgx.Tx, merchantID, id string) (Payment, error) {
	return scanOne(tx.QueryRow(ctx, `SELECT `+columns+` FROM payments WHERE id = $1 AND merchant_id = $2 FOR UPDATE`, id, merchantID))
}

// Update writes a payment's state back. The caller must hold the row lock
// from GetForUpdate.
func Update(ctx context.Context, tx pgx.Tx, p *Payment) error {
	err := tx.QueryRow(ctx, `
		UPDATE payments SET
			status = $2, authorized_amount = $3, captured_amount = $4, refunded_amount = $5,
			fee_amount = $6, failure_code = $7, failure_message = $8, acquirer_reference = $9,
			fraud_decision = $10, fraud_rules = $11, updated_at = now()
		WHERE id = $1
		RETURNING updated_at`,
		p.ID, string(p.Status), p.Authorized.Amount(), p.Captured.Amount(), p.Refunded.Amount(),
		p.Fee.Amount(), nullIfEmpty(p.FailureCode), nullIfEmpty(p.FailureMessage), nullIfEmpty(p.AcquirerReference),
		nullIfEmpty(p.FraudDecision), nonNil(p.FraudRules),
	).Scan(&p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("payments: updating %s: %w", p.ID, err)
	}
	return nil
}

// Refund is one refund of a payment.
type Refund struct {
	ID        string
	PaymentID string
	Amount    money.Money
	CreatedAt time.Time
}

// InsertRefund stores a refund record and fills in its timestamp.
func InsertRefund(ctx context.Context, tx pgx.Tx, r *Refund) error {
	err := tx.QueryRow(ctx, `
		INSERT INTO refunds (id, payment_id, amount, currency) VALUES ($1, $2, $3, $4)
		RETURNING created_at`,
		r.ID, r.PaymentID, r.Amount.Amount(), r.Amount.Currency().Code(),
	).Scan(&r.CreatedAt)
	if err != nil {
		return fmt.Errorf("payments: inserting refund %s: %w", r.ID, err)
	}
	return nil
}

func scanOne(row pgx.Row) (Payment, error) {
	var (
		p                                     Payment
		status, currency                      string
		amount, auth, captured, refunded, fee int64
		meta                                  []byte
		failCode, failMsg, acqRef, fraudDec   *string
	)
	err := row.Scan(&p.ID, &p.MerchantID, &status, &currency, &amount, &auth, &captured, &refunded, &fee,
		&p.CustomerID, &p.Card.Fingerprint, &p.Card.Country, &p.IPCountry, &meta,
		&failCode, &failMsg, &acqRef, &fraudDec, &p.FraudRules, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("payments: reading payment: %w", err)
	}
	c, err := money.ParseCurrency(currency)
	if err != nil {
		return Payment{}, fmt.Errorf("payments: stored currency: %w", err)
	}
	p.Status = Status(status)
	p.Amount, p.Authorized, p.Captured = money.New(amount, c), money.New(auth, c), money.New(captured, c)
	p.Refunded, p.Fee = money.New(refunded, c), money.New(fee, c)
	if err := json.Unmarshal(meta, &p.Metadata); err != nil {
		return Payment{}, fmt.Errorf("payments: stored metadata: %w", err)
	}
	p.FailureCode, p.FailureMessage, p.AcquirerReference = deref(failCode), deref(failMsg), deref(acqRef)
	p.FraudDecision = deref(fraudDec)
	return p, nil
}

// RecentByCard returns payments made with a card fingerprint since a time,
// excluding one payment, across all merchants: fraud velocity is about the
// card, not the shop.
func RecentByCard(ctx context.Context, db store.DB, fingerprint, excludeID string, since time.Time) ([]Payment, error) {
	rows, err := db.Query(ctx, `SELECT `+columns+` FROM payments
		WHERE card_fingerprint = $1 AND id <> $2 AND created_at >= $3`, fingerprint, excludeID, since)
	if err != nil {
		return nil, fmt.Errorf("payments: card history: %w", err)
	}
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		p, err := scanOne(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
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
