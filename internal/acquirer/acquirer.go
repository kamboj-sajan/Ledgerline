// Package acquirer is the boundary to the card network: the only external
// call in the payment flow. Authorize is idempotent on Request.IdempotencyKey,
// which Ledgerline sets to its payment ID, so a retry after a crash or a
// timeout can never authorize a payment twice.
//
// Simulator is a stand-in network with configurable latency, decline rate,
// timeouts and errors. Func adapts a plain function, for tests.
package acquirer

import (
	"context"
	"errors"

	"github.com/kamboj-sajan/ledgerline/internal/money"
)

// Errors from Authorize. Every one of them leaves the outcome for the caller
// to resolve by retrying with the same idempotency key.
var (
	// ErrUnavailable means the network refused the request before processing
	// it. Nothing was authorized.
	ErrUnavailable = errors.New("acquirer: unavailable, request not processed")
	// ErrTimeout means the call did not complete in time. The authorization
	// may or may not exist; only a retry with the same key can tell.
	ErrTimeout = errors.New("acquirer: timed out, outcome unknown")
	// ErrKeyReused means the idempotency key was already used for a request
	// with different parameters.
	ErrKeyReused = errors.New("acquirer: idempotency key reused with different parameters")
)

// Request asks the network to authorize a card payment.
type Request struct {
	IdempotencyKey   string // Ledgerline uses its payment ID
	PaymentReference string // the merchant-side reference (also the payment ID)
	Amount           money.Money
	CardFingerprint  string
	CardCountry      string
}

// Result is the network's answer to an authorization.
type Result struct {
	Approved       bool
	Reference      string // network reference, set when approved
	DeclineCode    string // set when declined, e.g. "insufficient_funds"
	DeclineMessage string
}

// Acquirer authorizes card payments.
type Acquirer interface {
	Authorize(ctx context.Context, req Request) (Result, error)
}

// Func adapts a function to the Acquirer interface.
type Func func(ctx context.Context, req Request) (Result, error)

// Authorize calls f.
func (f Func) Authorize(ctx context.Context, req Request) (Result, error) { return f(ctx, req) }

// Card fingerprints with fixed behaviour, like a real network's test card
// numbers, so tests do not depend on random rates.
const (
	CardDecline           = "tok_decline"            // always declined: card_declined
	CardInsufficientFunds = "tok_insufficient_funds" // always declined: insufficient_funds
	CardTimeout           = "tok_timeout"            // authorized, then the first call times out
	CardUnavailable       = "tok_unavailable"        // always ErrUnavailable
)
