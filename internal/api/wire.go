package api

import (
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/payments"
)

// JSON shapes of API resources. Every amount is a JSON integer in the
// currency's minor unit, next to its ISO 4217 code. Timestamps are RFC 3339
// in UTC.

type cardJSON struct {
	Fingerprint string `json:"fingerprint"`
	Country     string `json:"country"`
}

type paymentJSON struct {
	ID               string            `json:"id"`
	Object           string            `json:"object"`
	Status           payments.Status   `json:"status"`
	Amount           int64             `json:"amount"`
	Currency         string            `json:"currency"`
	AmountAuthorized int64             `json:"amount_authorized"`
	AmountCaptured   int64             `json:"amount_captured"`
	AmountRefunded   int64             `json:"amount_refunded"`
	Fee              int64             `json:"fee"`
	CustomerID       string            `json:"customer_id"`
	Card             cardJSON          `json:"card"`
	IPCountry        string            `json:"ip_country"`
	Metadata         map[string]string `json:"metadata"`
	FailureCode      *string           `json:"failure_code"`
	FailureMessage   *string           `json:"failure_message"`
	Fraud            *fraudJSON        `json:"fraud"` // null until authorization
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

type fraudJSON struct {
	Decision string   `json:"decision"`
	Rules    []string `json:"rules"`
}

func toPaymentJSON(p payments.Payment) paymentJSON {
	meta := p.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	var fr *fraudJSON
	if p.FraudDecision != "" {
		rules := p.FraudRules
		if rules == nil {
			rules = []string{}
		}
		fr = &fraudJSON{Decision: p.FraudDecision, Rules: rules}
	}
	return paymentJSON{
		Fraud: fr,
		ID:    p.ID, Object: "payment", Status: p.Status,
		Amount: p.Amount.Amount(), Currency: p.Amount.Currency().Code(),
		AmountAuthorized: p.Authorized.Amount(), AmountCaptured: p.Captured.Amount(),
		AmountRefunded: p.Refunded.Amount(), Fee: p.Fee.Amount(),
		CustomerID: p.CustomerID, Card: cardJSON{p.Card.Fingerprint, p.Card.Country}, IPCountry: p.IPCountry,
		Metadata: meta, FailureCode: optional(p.FailureCode), FailureMessage: optional(p.FailureMessage),
		CreatedAt: p.CreatedAt.UTC(), UpdatedAt: p.UpdatedAt.UTC(),
	}
}

type refundJSON struct {
	ID        string      `json:"id"`
	Object    string      `json:"object"`
	PaymentID string      `json:"payment_id"`
	Amount    int64       `json:"amount"`
	Currency  string      `json:"currency"`
	CreatedAt time.Time   `json:"created_at"`
	Payment   paymentJSON `json:"payment"`
}

func toRefundJSON(r payments.Refund, p payments.Payment) refundJSON {
	return refundJSON{
		ID: r.ID, Object: "refund", PaymentID: r.PaymentID, Amount: r.Amount.Amount(),
		Currency: r.Amount.Currency().Code(), CreatedAt: r.CreatedAt.UTC(), Payment: toPaymentJSON(p),
	}
}

type entryJSON struct {
	AccountID  string `json:"account_id"`
	Account    string `json:"account"` // owner/code/currency, for humans
	MerchantID string `json:"merchant_id,omitempty"`
	Amount     int64  `json:"amount"` // signed: debit +, credit -
	Currency   string `json:"currency"`
}

type journalJSON struct {
	ID        string      `json:"id"`
	Object    string      `json:"object"`
	Kind      ledger.Kind `json:"kind"`
	PaymentID string      `json:"payment_id"`
	CreatedAt time.Time   `json:"created_at"`
	Entries   []entryJSON `json:"entries"`
}

func toJournalJSON(t ledger.StoredTx) journalJSON {
	out := journalJSON{ID: t.ID, Object: "journal_transaction", Kind: t.Kind, PaymentID: t.PaymentID, CreatedAt: t.CreatedAt.UTC()}
	for i, p := range t.Postings {
		out.Entries = append(out.Entries, entryJSON{
			AccountID: t.AccountIDs[i], Account: p.Account.String(), MerchantID: p.Account.MerchantID,
			Amount: p.Amount.Amount(), Currency: p.Amount.Currency().Code(),
		})
	}
	return out
}

type listJSON[T any] struct {
	Object string `json:"object"`
	Data   []T    `json:"data"`
}

type balanceJSON struct {
	Object        string             `json:"object"`
	AccountID     string             `json:"account_id"`
	Code          ledger.AccountCode `json:"code"`
	Type          ledger.AccountType `json:"type"`
	Currency      string             `json:"currency"`
	Balance       int64              `json:"balance"`        // in the account's normal direction
	SignedBalance int64              `json:"signed_balance"` // debit-positive
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
