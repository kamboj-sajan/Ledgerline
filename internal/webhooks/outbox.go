// Package webhooks delivers payment events to merchants.
//
// Events are written to an outbox table in the same database transaction as
// the state change they describe, together with one delivery row per enabled
// endpoint, so an event and its deliveries exist exactly when the change
// committed (docs/adr/0005-outbox.md). The Dispatcher sends due deliveries,
// signed with HMAC-SHA256, and retries with capped exponential backoff and
// full jitter. Delivery is at-least-once and unordered; receivers dedupe on
// the event id.
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

// Insert writes an event and its delivery rows in tx. It takes a pgx.Tx so
// an event can only be written together with the change it describes.
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
	if _, err := tx.Exec(ctx, `
		INSERT INTO webhook_deliveries (event_id, endpoint_id)
		SELECT $1, id FROM webhook_endpoints WHERE merchant_id = $2 AND enabled`, e.ID, merchantID); err != nil {
		return Event{}, fmt.Errorf("webhooks: queueing deliveries: %w", err)
	}
	return e, nil
}

// payload is the JSON body sent to endpoints.
type payload struct {
	ID        string          `json:"id"`
	Object    string          `json:"object"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

// Payload renders the body delivered for an event. It is deterministic, so
// every redelivery sends the same bytes.
func Payload(e Event) ([]byte, error) {
	return json.Marshal(payload{ID: e.ID, Object: "event", Type: e.Type, CreatedAt: e.CreatedAt.UTC(), Data: e.Data})
}
