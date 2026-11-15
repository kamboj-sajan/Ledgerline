// Package webhooks delivers payment events to merchants. Events are written
// to an outbox table in the same database transaction as the state change
// they describe, so an event exists exactly when its change committed. A
// separate dispatcher (M3) delivers them.
package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kamboj-sajan/ledgerline/internal/id"
)

// Event types.
const (
	EventPaymentCreated    = "payment.created"
	EventPaymentAuthorized = "payment.authorized"
	EventPaymentFailed     = "payment.failed"
	EventPaymentCaptured   = "payment.captured"
	EventPaymentRefunded   = "payment.refunded" // one per refund, partial or full
)

// Event is one outbox row.
type Event struct {
	ID         string
	MerchantID string
	Type       string
	PaymentID  string
	Data       json.RawMessage
	CreatedAt  time.Time
}

// Insert writes an event in tx. It takes a pgx.Tx so the event can only be
// written together with the change it describes.
func Insert(ctx context.Context, tx pgx.Tx, merchantID, eventType, paymentID string, data any) (Event, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, fmt.Errorf("webhooks: encoding event data: %w", err)
	}
	e := Event{ID: id.New("evt"), MerchantID: merchantID, Type: eventType, PaymentID: paymentID, Data: raw}
	var pid *string
	if paymentID != "" {
		pid = &paymentID
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO events (id, merchant_id, type, payment_id, data) VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at`, e.ID, merchantID, eventType, pid, raw).Scan(&e.CreatedAt)
	if err != nil {
		return Event{}, fmt.Errorf("webhooks: inserting event: %w", err)
	}
	return e, nil
}
