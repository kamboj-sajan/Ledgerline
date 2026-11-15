package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kamboj-sajan/ledgerline/internal/id"
	"github.com/kamboj-sajan/ledgerline/internal/store"
)

// ErrNotFound is returned for an endpoint or event that does not exist or
// belongs to another merchant.
var ErrNotFound = errors.New("webhooks: not found")

// RotationGrace is how long the previous secret stays valid after a rotation.
const RotationGrace = 24 * time.Hour

// Endpoint is a merchant's webhook URL.
type Endpoint struct {
	ID         string
	MerchantID string
	URL        string
	Secret     string // returned to the merchant once, at creation or rotation
	Enabled    bool
	CreatedAt  time.Time
}

func newSecret() string { return "whsec_" + rand.Text() + rand.Text() } // 260 random bits

// CreateEndpoint registers url for merchantID with a fresh signing secret.
func CreateEndpoint(ctx context.Context, tx pgx.Tx, merchantID, url string) (Endpoint, error) {
	e := Endpoint{ID: id.New("we"), MerchantID: merchantID, URL: url, Secret: newSecret(), Enabled: true}
	err := tx.QueryRow(ctx, `
		INSERT INTO webhook_endpoints (id, merchant_id, url, secret) VALUES ($1, $2, $3, $4)
		RETURNING created_at`, e.ID, merchantID, url, e.Secret).Scan(&e.CreatedAt)
	if err != nil {
		return Endpoint{}, fmt.Errorf("webhooks: creating endpoint: %w", err)
	}
	return e, nil
}

// RotateSecret gives the endpoint a new secret. The old one keeps working
// for grace, and until then deliveries are signed with both.
func RotateSecret(ctx context.Context, tx pgx.Tx, merchantID, endpointID string, grace time.Duration) (Endpoint, error) {
	e := Endpoint{ID: endpointID, MerchantID: merchantID, Secret: newSecret()}
	err := tx.QueryRow(ctx, `
		UPDATE webhook_endpoints
		SET previous_secret = secret, previous_secret_expires_at = now() + make_interval(secs => $4), secret = $3
		WHERE id = $1 AND merchant_id = $2
		RETURNING url, enabled, created_at`, endpointID, merchantID, e.Secret, grace.Seconds(),
	).Scan(&e.URL, &e.Enabled, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Endpoint{}, ErrNotFound
	}
	if err != nil {
		return Endpoint{}, fmt.Errorf("webhooks: rotating secret: %w", err)
	}
	return e, nil
}

// ListEvents returns up to limit of the merchant's events after the event
// startingAfter (or from the beginning), oldest first.
func ListEvents(ctx context.Context, db store.DB, merchantID, startingAfter string, limit int) ([]Event, error) {
	rows, err := db.Query(ctx, `
		SELECT id, merchant_id, type, COALESCE(payment_id, ''), data, created_at FROM events
		WHERE merchant_id = $1
		  AND ($2 = '' OR (created_at, id) > (SELECT created_at, id FROM events WHERE id = $2 AND merchant_id = $1))
		ORDER BY created_at, id
		LIMIT $3`, merchantID, startingAfter, limit)
	if err != nil {
		return nil, fmt.Errorf("webhooks: listing events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.MerchantID, &e.Type, &e.PaymentID, &e.Data, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("webhooks: listing events: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetEvent returns one of the merchant's events.
func GetEvent(ctx context.Context, db store.DB, merchantID, eventID string) (Event, error) {
	var e Event
	err := db.QueryRow(ctx, `
		SELECT id, merchant_id, type, COALESCE(payment_id, ''), data, created_at FROM events
		WHERE id = $1 AND merchant_id = $2`, eventID, merchantID,
	).Scan(&e.ID, &e.MerchantID, &e.Type, &e.PaymentID, &e.Data, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("webhooks: reading event: %w", err)
	}
	return e, nil
}

// Redeliver queues an event again for every enabled endpoint. It creates
// missing delivery rows (for endpoints added after the event) and resets
// existing ones, including dead and delivered ones, to pending with a fresh
// attempt budget. It returns how many deliveries are now pending.
func Redeliver(ctx context.Context, tx pgx.Tx, merchantID, eventID string) (int64, error) {
	if _, err := GetEvent(ctx, tx, merchantID, eventID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO webhook_deliveries (event_id, endpoint_id)
		SELECT $1, id FROM webhook_endpoints WHERE merchant_id = $2 AND enabled
		ON CONFLICT ON CONSTRAINT webhook_deliveries_once_per_endpoint DO NOTHING`, eventID, merchantID); err != nil {
		return 0, fmt.Errorf("webhooks: queueing redelivery: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE webhook_deliveries d SET status = 'pending', attempts = 0, next_attempt_at = now(),
			last_error = NULL, last_status = NULL, delivered_at = NULL
		FROM webhook_endpoints e
		WHERE d.event_id = $1 AND d.endpoint_id = e.id AND e.enabled`, eventID)
	if err != nil {
		return 0, fmt.Errorf("webhooks: resetting deliveries: %w", err)
	}
	return tag.RowsAffected(), nil
}

// EventJSON is the API representation of an event (the same object that is
// delivered).
func EventJSON(e Event) json.RawMessage {
	b, _ := Payload(e) // marshalling a struct of strings, a time and raw JSON cannot fail
	return b
}
