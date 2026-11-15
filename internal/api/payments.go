package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kamboj-sajan/ledgerline/internal/id"
	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/merchants"
	"github.com/kamboj-sajan/ledgerline/internal/money"
	"github.com/kamboj-sajan/ledgerline/internal/payments"
	"github.com/kamboj-sajan/ledgerline/internal/webhooks"
)

var countryCode = regexp.MustCompile(`^[A-Z]{2}$`)

type createPaymentRequest struct {
	Amount     *int64 `json:"amount"`
	Currency   string `json:"currency"`
	CustomerID string `json:"customer_id"`
	Card       *struct {
		Fingerprint string `json:"fingerprint"`
		Country     string `json:"country"`
	} `json:"card"`
	IPCountry string            `json:"ip_country"`
	Metadata  map[string]string `json:"metadata"`
}

// validate checks the request before anything executes, so a bad request
// never uses up its idempotency key.
func (req createPaymentRequest) validate() (money.Money, *apiError) {
	switch {
	case req.Amount == nil:
		return money.Money{}, paramMissing("amount")
	case req.Currency == "":
		return money.Money{}, paramMissing("currency")
	case req.CustomerID == "":
		return money.Money{}, paramMissing("customer_id")
	case req.Card == nil || req.Card.Fingerprint == "":
		return money.Money{}, paramMissing("card.fingerprint")
	case req.Card.Country == "":
		return money.Money{}, paramMissing("card.country")
	case req.IPCountry == "":
		return money.Money{}, paramMissing("ip_country")
	}
	c, err := money.ParseCurrency(req.Currency)
	if err != nil {
		return money.Money{}, paramInvalid("currency", "%q is not a supported ISO 4217 code (use upper case, e.g. USD)", req.Currency)
	}
	if *req.Amount < 1 || *req.Amount > payments.MaxAmount {
		return money.Money{}, paramInvalid("amount", "amount must be between 1 and %d minor units", int64(payments.MaxAmount))
	}
	if len(req.CustomerID) > 255 || len(req.Card.Fingerprint) > 255 {
		return money.Money{}, paramInvalid("customer_id", "customer_id and card.fingerprint are limited to 255 characters")
	}
	if !countryCode.MatchString(req.Card.Country) {
		return money.Money{}, paramInvalid("card.country", "card.country must be an ISO 3166-1 alpha-2 code such as US")
	}
	if !countryCode.MatchString(req.IPCountry) {
		return money.Money{}, paramInvalid("ip_country", "ip_country must be an ISO 3166-1 alpha-2 code such as US")
	}
	if len(req.Metadata) > 20 {
		return money.Money{}, paramInvalid("metadata", "metadata is limited to 20 keys")
	}
	for k, v := range req.Metadata {
		if len(k) == 0 || len(k) > 40 || len(v) > 500 {
			return money.Money{}, paramInvalid("metadata", "metadata keys must be 1-40 characters and values at most 500")
		}
	}
	return money.New(*req.Amount, c), nil
}

func (s *Server) createPayment(w http.ResponseWriter, r *http.Request, m merchants.Merchant) {
	body, apiErr := readBody(w, r)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	var req createPaymentRequest
	if apiErr := decodeStrict(body, &req); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	amount, apiErr := req.validate()
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	s.runIdempotent(w, r, m, body, func(ctx context.Context, tx pgx.Tx) (int, any, error) {
		p, err := payments.New(id.New("pay"), m.ID, amount)
		if err != nil {
			return 0, nil, err
		}
		p.CustomerID, p.IPCountry = req.CustomerID, req.IPCountry
		p.Card = payments.Card{Fingerprint: req.Card.Fingerprint, Country: req.Card.Country}
		if req.Metadata != nil {
			p.Metadata = req.Metadata
		}
		if err := payments.Insert(ctx, tx, &p); err != nil {
			return 0, nil, err
		}
		out := toPaymentJSON(p)
		if _, err := webhooks.Insert(ctx, tx, m.ID, webhooks.EventPaymentCreated, p.ID, out); err != nil {
			return 0, nil, err
		}
		return http.StatusCreated, out, nil
	})
}

func (s *Server) getPayment(w http.ResponseWriter, r *http.Request, m merchants.Merchant) {
	p, err := payments.Get(r.Context(), s.db, m.ID, r.PathValue("id"))
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toPaymentJSON(p))
}

type amountRequest struct {
	Amount *int64 `json:"amount"`
}

// optionalAmount parses {"amount": n}; nil means "use the default".
func optionalAmount(w http.ResponseWriter, r *http.Request) ([]byte, *int64, *apiError) {
	body, apiErr := readBody(w, r)
	if apiErr != nil {
		return nil, nil, apiErr
	}
	var req amountRequest
	if apiErr := decodeStrict(body, &req); apiErr != nil {
		return nil, nil, apiErr
	}
	if req.Amount != nil && (*req.Amount < 1 || *req.Amount > payments.MaxAmount) {
		return nil, nil, paramInvalid("amount", "amount must be between 1 and %d minor units", int64(payments.MaxAmount))
	}
	return body, req.Amount, nil
}

// capture takes the authorized amount (or less) in one transaction: lock the
// payment row, apply the state machine, post the ledger transaction, write
// the payment, insert the outbox event, store the response.
func (s *Server) capture(w http.ResponseWriter, r *http.Request, m merchants.Merchant) {
	body, amount, apiErr := optionalAmount(w, r)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	s.runIdempotent(w, r, m, body, func(ctx context.Context, tx pgx.Tx) (int, any, error) {
		p, err := payments.GetForUpdate(ctx, tx, m.ID, r.PathValue("id"))
		if err != nil {
			return 0, nil, err
		}
		take := p.Authorized // default: capture everything authorized
		if amount != nil {
			take = money.New(*amount, p.Amount.Currency())
		}
		lt, err := p.Capture(take, m.Pricing)
		if err != nil {
			return 0, nil, err
		}
		if _, err := ledger.Post(ctx, tx, lt); err != nil {
			return 0, nil, err
		}
		if err := payments.Update(ctx, tx, &p); err != nil {
			return 0, nil, err
		}
		out := toPaymentJSON(p)
		if _, err := webhooks.Insert(ctx, tx, m.ID, webhooks.EventPaymentCaptured, p.ID, out); err != nil {
			return 0, nil, err
		}
		return http.StatusOK, out, nil
	})
}

// refund returns part or all of the remaining captured amount.
func (s *Server) refund(w http.ResponseWriter, r *http.Request, m merchants.Merchant) {
	body, amount, apiErr := optionalAmount(w, r)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	s.runIdempotent(w, r, m, body, func(ctx context.Context, tx pgx.Tx) (int, any, error) {
		p, err := payments.GetForUpdate(ctx, tx, m.ID, r.PathValue("id"))
		if err != nil {
			return 0, nil, err
		}
		give := p.Remaining() // default: refund whatever is left
		if amount != nil {
			give = money.New(*amount, p.Amount.Currency())
		}
		lt, err := p.Refund(give)
		if err != nil {
			return 0, nil, err
		}
		if _, err := ledger.Post(ctx, tx, lt); err != nil {
			return 0, nil, err
		}
		rf := payments.Refund{ID: id.New("re"), PaymentID: p.ID, Amount: give}
		if err := payments.InsertRefund(ctx, tx, &rf); err != nil {
			return 0, nil, err
		}
		if err := payments.Update(ctx, tx, &p); err != nil {
			return 0, nil, err
		}
		out := toRefundJSON(rf, p)
		if _, err := webhooks.Insert(ctx, tx, m.ID, webhooks.EventPaymentRefunded, p.ID, out); err != nil {
			return 0, nil, err
		}
		return http.StatusCreated, out, nil
	})
}

// void releases an authorization without capturing it.
func (s *Server) void(w http.ResponseWriter, r *http.Request, m merchants.Merchant) {
	body, apiErr := readBody(w, r)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	if apiErr := decodeStrict(body, &struct{}{}); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	s.runIdempotent(w, r, m, body, func(ctx context.Context, tx pgx.Tx) (int, any, error) {
		p, err := payments.GetForUpdate(ctx, tx, m.ID, r.PathValue("id"))
		if err != nil {
			return 0, nil, err
		}
		if err := p.Void(); err != nil {
			return 0, nil, err
		}
		if err := payments.Update(ctx, tx, &p); err != nil {
			return 0, nil, err
		}
		out := toPaymentJSON(p)
		if _, err := webhooks.Insert(ctx, tx, m.ID, webhooks.EventPaymentFailed, p.ID, out); err != nil {
			return 0, nil, err
		}
		return http.StatusOK, out, nil
	})
}

func (s *Server) paymentLedger(w http.ResponseWriter, r *http.Request, m merchants.Merchant) {
	ctx := r.Context()
	p, err := payments.Get(ctx, s.db, m.ID, r.PathValue("id"))
	if err != nil {
		s.writeDomainError(w, r, err)
		return
	}
	txs, err := ledger.PaymentTransactions(ctx, s.db, p.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := listJSON[journalJSON]{Object: "list", Data: []journalJSON{}}
	for _, t := range txs {
		out.Data = append(out.Data, toJournalJSON(t))
	}
	writeJSON(w, http.StatusOK, out)
}

// balance returns a merchant's own account balance. Platform accounts and
// other merchants' accounts are reported as not found.
func (s *Server) balance(w http.ResponseWriter, r *http.Request, m merchants.Merchant) {
	acct, signed, err := ledger.Balance(r.Context(), s.db, r.PathValue("id"))
	if errors.Is(err, ledger.ErrUnknownAccount) || (err == nil && acct.Key.MerchantID != m.ID) {
		writeError(w, errNotFound)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	normal, err := ledger.NormalBalance(acct.Key.Type(), signed)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, balanceJSON{
		Object: "balance", AccountID: acct.ID, Code: acct.Key.Code, Type: acct.Key.Type(),
		Currency: acct.Key.Currency.Code(), Balance: normal.Amount(), SignedBalance: signed.Amount(),
	})
}

func (s *Server) writeDomainError(w http.ResponseWriter, r *http.Request, err error) {
	ae := fromDomain(err)
	if ae == errInternal {
		s.internalError(w, r, err)
		return
	}
	writeError(w, ae)
}

// ExpireAuthorizations fails authorizations older than maxAge that were never
// captured, through the same state machine and outbox as the API. It
// processes at most limit payments and reports how many it expired. Several
// servers can run it at once: SKIP LOCKED makes each pick different rows.
func (s *Server) ExpireAuthorizations(ctx context.Context, maxAge time.Duration, limit int) (int, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT id, merchant_id FROM payments
		WHERE status = 'authorized' AND updated_at < now() - make_interval(secs => $1)
		ORDER BY updated_at LIMIT $2
		FOR UPDATE SKIP LOCKED`, maxAge.Seconds(), limit)
	if err != nil {
		return 0, err
	}
	type ref struct{ id, merchant string }
	var due []ref
	for rows.Next() {
		var x ref
		if err := rows.Scan(&x.id, &x.merchant); err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, x := range due {
		p, err := payments.GetForUpdate(ctx, tx, x.merchant, x.id) // already locked by this tx
		if err != nil {
			return 0, err
		}
		if err := p.Expire(); err != nil {
			return 0, err
		}
		if err := payments.Update(ctx, tx, &p); err != nil {
			return 0, err
		}
		if _, err := webhooks.Insert(ctx, tx, x.merchant, webhooks.EventPaymentFailed, p.ID, toPaymentJSON(p)); err != nil {
			return 0, err
		}
	}
	return len(due), tx.Commit(ctx)
}
