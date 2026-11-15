// Package merchants creates merchants and authenticates their API keys.
package merchants

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/kamboj-sajan/ledgerline/internal/id"
	"github.com/kamboj-sajan/ledgerline/internal/payments"
	"github.com/kamboj-sajan/ledgerline/internal/store"
)

// ErrUnauthenticated is returned for an unknown API key.
var ErrUnauthenticated = errors.New("merchants: invalid API key")

// Merchant is an API tenant.
type Merchant struct {
	ID      string
	Name    string
	Pricing payments.Pricing
}

// NewAPIKey returns a fresh key: "sk_" plus 256 random bits, base64url.
func NewAPIKey() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return "sk_" + base64.RawURLEncoding.EncodeToString(b)
}

// HashKey is the only form of an API key that is stored. A single SHA-256
// is enough because the key is 256 random bits: there is no dictionary to
// guess from, so a slow password hash (bcrypt, argon2) would only add tens of
// milliseconds to every request without making anything safer.
func HashKey(key string) []byte {
	h := sha256.Sum256([]byte(key))
	return h[:]
}

// Create adds a merchant with default pricing and returns it with its API
// key. The key is not stored and cannot be recovered later.
func Create(ctx context.Context, db store.DB, name string) (Merchant, string, error) {
	key := NewAPIKey()
	m := Merchant{ID: id.New("mch"), Name: name}
	err := db.QueryRow(ctx, `
		INSERT INTO merchants (id, name, api_key_hash) VALUES ($1, $2, $3)
		RETURNING fee_bps, fee_fixed`, m.ID, name, HashKey(key),
	).Scan(&m.Pricing.BasisPoints, &m.Pricing.Fixed)
	if err != nil {
		return Merchant{}, "", fmt.Errorf("merchants: creating %q: %w", name, err)
	}
	return m, key, nil
}

// Authenticate finds the merchant that owns key.
//
// The lookup is by hash through a unique index. Comparing in constant time
// is unnecessary here: an attacker who measured the index lookup would learn
// about a SHA-256 output, not about the key.
func Authenticate(ctx context.Context, db store.DB, key string) (Merchant, error) {
	var m Merchant
	err := db.QueryRow(ctx, `SELECT id, name, fee_bps, fee_fixed FROM merchants WHERE api_key_hash = $1`, HashKey(key)).
		Scan(&m.ID, &m.Name, &m.Pricing.BasisPoints, &m.Pricing.Fixed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Merchant{}, ErrUnauthenticated
	}
	if err != nil {
		return Merchant{}, fmt.Errorf("merchants: authenticating: %w", err)
	}
	return m, nil
}
