// Package payments holds the payment domain: the state machine, the
// operations that move a payment between states, pricing, and the SQL that
// stores payments.
//
// The domain methods are pure: they check the transition table and the
// amounts, change the Payment value, and return the ledger transaction the
// change requires. They never touch the database. The API layer runs them
// inside one database transaction together with the ledger posting, the
// outbox event and the stored idempotent response.
package payments

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/money"
)

// MaxAmount is the largest payment amount in minor units, matching the CHECK
// on payments.amount. It keeps every sum far from the int64 limit.
const MaxAmount = 999_999_999_999

// Errors from domain operations. The API maps each one to a status code.
var (
	ErrInvalidTransition = errors.New("payments: invalid state transition")
	ErrAmountTooLarge    = errors.New("payments: amount too large")
	ErrInvalidAmount     = errors.New("payments: invalid amount")
	ErrNotFound          = errors.New("payments: payment not found")
)

// TransitionError reports an event that is not allowed in the payment's
// current state. It wraps ErrInvalidTransition.
type TransitionError struct {
	Event    Event
	From, To Status
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("cannot %s a payment that is %s (%s → %s is not allowed)",
		strings.ReplaceAll(string(e.Event), "_", " "), e.From, e.From, e.To)
}

func (e *TransitionError) Unwrap() error { return ErrInvalidTransition }

// Failure codes stored on failed payments.
const (
	FailureVoided  = "voided"
	FailureExpired = "authorization_expired"
)

// Card is the card used for a payment. Ledgerline never sees card numbers,
// only a fingerprint (a stable token for the same card) and its country.
type Card struct {
	Fingerprint string
	Country     string // ISO 3166-1 alpha-2
}

// Payment is one payment and its money state.
type Payment struct {
	ID         string
	MerchantID string
	Status     Status

	Amount     money.Money // requested
	Authorized money.Money // reserved at the issuer; the capture limit
	Captured   money.Money
	Refunded   money.Money
	Fee        money.Money // our fee, charged at capture

	CustomerID string
	Card       Card
	IPCountry  string
	Metadata   map[string]string

	FailureCode       string
	FailureMessage    string
	AcquirerReference string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// New returns a payment in StatusCreated with every tracked amount at zero.
func New(id, merchantID string, amount money.Money) (Payment, error) {
	if amount.Currency().IsZero() || amount.Sign() <= 0 || amount.Amount() > MaxAmount {
		return Payment{}, fmt.Errorf("%w: amount must be between 1 and %d minor units", ErrInvalidAmount, int64(MaxAmount))
	}
	zero := money.Zero(amount.Currency())
	return Payment{
		ID: id, MerchantID: merchantID, Status: StatusCreated,
		Amount: amount, Authorized: zero, Captured: zero, Refunded: zero, Fee: zero,
		Metadata: map[string]string{},
	}, nil
}

// apply moves the payment along the transition table, or returns a
// *TransitionError if e is not allowed in the current state.
func (p *Payment) apply(e Event) error {
	to, ok := Next(p.Status, e)
	if !ok {
		return &TransitionError{Event: e, From: p.Status, To: e.target()}
	}
	p.Status = to
	return nil
}

// check is apply without the state change, so an operation can reject a bad
// state before it validates amounts.
func (p *Payment) check(e Event) error {
	if _, ok := Next(p.Status, e); !ok {
		return &TransitionError{Event: e, From: p.Status, To: e.target()}
	}
	return nil
}

// MarkAuthorized records an approval from the acquirer: created → authorized.
// The full requested amount is reserved.
func (p *Payment) MarkAuthorized(acquirerReference string) error {
	if err := p.apply(EventApprove); err != nil {
		return err
	}
	p.Authorized = p.Amount
	p.AcquirerReference = acquirerReference
	return nil
}

// MarkDeclined records a decline (by the acquirer, or by fraud rules):
// created → failed.
func (p *Payment) MarkDeclined(code, message string) error {
	if err := p.apply(EventDecline); err != nil {
		return err
	}
	p.FailureCode, p.FailureMessage = code, message
	return nil
}

// Void releases an authorization the merchant no longer wants:
// authorized → failed.
func (p *Payment) Void() error {
	return p.failAuthorized(EventVoid, FailureVoided, "the authorization was voided")
}

// Expire releases an authorization that was never captured in time:
// authorized → failed.
func (p *Payment) Expire() error {
	return p.failAuthorized(EventExpire, FailureExpired, "the authorization expired before it was captured")
}

func (p *Payment) failAuthorized(e Event, code, message string) error {
	if err := p.apply(e); err != nil {
		return err
	}
	p.FailureCode, p.FailureMessage = code, message
	return nil
}

// Capture takes amount (at most the authorized amount) from the
// authorization: authorized → captured. A partial capture releases the rest of
// the authorization; there is one capture per payment. It returns the ledger
// transaction to post.
func (p *Payment) Capture(amount money.Money, pricing Pricing) (ledger.Tx, error) {
	if err := p.check(EventCapture); err != nil {
		return ledger.Tx{}, err
	}
	if err := checkAmount(amount, p.Authorized, "capture", "authorized"); err != nil {
		return ledger.Tx{}, err
	}
	fee, err := pricing.Fee(amount)
	if err != nil {
		return ledger.Tx{}, err
	}
	tx, err := ledger.Capture(p.ID, p.MerchantID, amount, fee)
	if err != nil {
		return ledger.Tx{}, err
	}
	if err := p.apply(EventCapture); err != nil {
		return ledger.Tx{}, err
	}
	p.Captured, p.Fee = amount, fee
	return tx, nil
}

// Refund returns amount to the cardholder. Partial refunds keep the payment
// captured. The refund that brings the refunded total to the captured
// amount moves it to refunded. It returns the ledger transaction to post.
func (p *Payment) Refund(amount money.Money) (ledger.Tx, error) {
	// Partial and full refunds are allowed in exactly the same states, so
	// checking one before the amount is known is enough.
	if err := p.check(EventPartialRefund); err != nil {
		return ledger.Tx{}, err
	}
	remaining, err := p.Captured.Sub(p.Refunded)
	if err != nil {
		return ledger.Tx{}, err
	}
	if err := checkAmount(amount, remaining, "refund", "unrefunded"); err != nil {
		return ledger.Tx{}, err
	}
	tx, err := ledger.Refund(p.ID, p.MerchantID, amount)
	if err != nil {
		return ledger.Tx{}, err
	}
	refunded, err := p.Refunded.Add(amount)
	if err != nil {
		return ledger.Tx{}, err
	}
	event := EventPartialRefund
	if refunded == p.Captured {
		event = EventFullRefund
	}
	if err := p.apply(event); err != nil {
		return ledger.Tx{}, err
	}
	p.Refunded = refunded
	return tx, nil
}

// Remaining returns the amount that can still be refunded.
func (p Payment) Remaining() money.Money {
	r, _ := p.Captured.Sub(p.Refunded) // 0 <= refunded <= captured
	return r
}

// checkAmount requires 0 < amount <= limit in the payment's currency.
func checkAmount(amount, limit money.Money, op, limitName string) error {
	cmp, err := amount.Cmp(limit)
	if err != nil {
		return fmt.Errorf("%w: %s currency must match the payment: %w", ErrInvalidAmount, op, err)
	}
	if amount.Sign() <= 0 {
		return fmt.Errorf("%w: %s amount must be positive", ErrInvalidAmount, op)
	}
	if cmp > 0 {
		return fmt.Errorf("%w: %s amount %d is more than the %s amount %d", ErrAmountTooLarge, op, amount.Amount(), limitName, limit.Amount())
	}
	return nil
}

// Pricing is a merchant's fee schedule.
type Pricing struct {
	BasisPoints int64 // percentage part, 290 = 2.9%
	Fixed       int64 // fixed part, in the payment currency's minor units
}

// Fee returns the fee on a captured amount: basis points (rounded half away
// from zero) plus the fixed part, capped at the amount itself so a tiny
// payment can never cost the merchant more than it brought in.
func (pr Pricing) Fee(amount money.Money) (money.Money, error) {
	pct, err := amount.MulBasisPoints(pr.BasisPoints)
	if err != nil {
		return money.Money{}, err
	}
	fee, err := pct.Add(money.New(pr.Fixed, amount.Currency()))
	if err != nil {
		return money.Money{}, err
	}
	if cmp, _ := fee.Cmp(amount); cmp > 0 {
		return amount, nil
	}
	return fee, nil
}
