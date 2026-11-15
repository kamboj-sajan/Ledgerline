package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/kamboj-sajan/ledgerline/internal/acquirer"
	"github.com/kamboj-sajan/ledgerline/internal/failpoint"
	"github.com/kamboj-sajan/ledgerline/internal/idempotency"
	"github.com/kamboj-sajan/ledgerline/internal/merchants"
	"github.com/kamboj-sajan/ledgerline/internal/payments"
	"github.com/kamboj-sajan/ledgerline/internal/webhooks"
)

// authorize asks the card network to reserve the payment's amount.
//
// Unlike capture and refund it cannot be one database transaction, because
// it makes an external call that must not happen while a transaction (and
// the payment's row lock) is held. It runs as atomic phases with recovery
// points, after Brandur Leach's "Implementing Stripe-like Idempotency Keys
// in Postgres":
//
//  1. In a transaction: take the key, check the payment can be authorized,
//     record recovery point 'started' with a lock and a fencing token.
//  2. No transaction: call the acquirer with our payment ID as its
//     idempotency key.
//  3. In a transaction: re-claim the key with the token, apply the result
//     through the state machine, write the payment and its outbox event,
//     store the response, finish the key.
//
// A crash anywhere leaves either nothing (before phase 1 commits) or a
// 'started' key. A retry with the same key resumes at phase 2 once the lock is
// stale. Calling the acquirer again is safe: it is idempotent on the payment
// ID, so it returns the original decision and never authorizes twice.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, m merchants.Merchant) {
	body, apiErr := readBody(w, r)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	if apiErr := decodeStrict(body, &struct{}{}); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	req, apiErr := keyedRequest(r, m, body)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	ctx := r.Context()

	// Phase 1.
	p, token, done := s.authorizeBegin(ctx, w, r, m, req)
	if done {
		return
	}

	// Phase 2: the external call, outside any transaction.
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.AcquirerTimeout)
	result, err := s.acq.Authorize(callCtx, acquirer.Request{
		IdempotencyKey: p.ID, PaymentReference: p.ID, Amount: p.Amount,
		CardFingerprint: p.Card.Fingerprint, CardCountry: p.Card.Country,
	})
	cancel()
	failpoint.Inject("authorize-after-acquirer-call")
	if err != nil {
		// Outcome unknown or not processed. Release the key so a retry can
		// resume immediately; it will ask the acquirer again.
		if uerr := idempotency.Unlock(context.WithoutCancel(ctx), s.db, req, token); uerr != nil {
			s.log.Error("unlocking idempotency key", "error", uerr)
		}
		writeError(w, acquirerError(err))
		return
	}

	// Phase 3.
	s.authorizeFinish(ctx, w, r, m, req, token, p.ID, result)
}

// authorizeBegin is phase 1. It returns done=true once it has written a
// response (an error, a replay, or a stored rejection).
func (s *Server) authorizeBegin(ctx context.Context, w http.ResponseWriter, r *http.Request, m merchants.Merchant, req idempotency.Request) (payments.Payment, string, bool) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return payments.Payment{}, "", true
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := idempotency.Lock(ctx, tx, req); err != nil {
		s.writeIdempotencyError(w, r, err)
		return payments.Payment{}, "", true
	}
	rec, err := idempotency.Lookup(ctx, tx, req, s.cfg.AuthorizeLockTimeout)
	if err != nil {
		s.writeIdempotencyError(w, r, err)
		return payments.Payment{}, "", true
	}
	if rec.State == idempotency.StateFinished {
		writeReplay(w, rec.Response)
		return payments.Payment{}, "", true
	}

	p, err := payments.Get(ctx, tx, m.ID, r.PathValue("id"))
	if err != nil {
		s.writeDomainError(w, r, err) // not found: not stored
		return payments.Payment{}, "", true
	}
	// A fresh request for a payment that cannot be authorized has executed
	// and failed: store the 409. A resumed one (StateStarted) skips the check,
	// because the payment may already have moved on as a result of this very
	// request, and phase 3 decides.
	if rec.State == idempotency.StateNew {
		if _, ok := payments.Next(p.Status, payments.EventApprove); !ok {
			ae := fromDomain(&payments.TransitionError{Event: payments.EventApprove, From: p.Status, To: payments.StatusAuthorized})
			s.saveAndWrite(ctx, w, r, tx, req, ae.Status, errorBody{ae})
			return payments.Payment{}, "", true
		}
	}
	token, err := idempotency.Start(ctx, tx, req, p.ID)
	if err != nil {
		s.internalError(w, r, err)
		return payments.Payment{}, "", true
	}
	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, r, err)
		return payments.Payment{}, "", true
	}
	return p, token, false
}

// authorizeFinish is phase 3.
func (s *Server) authorizeFinish(ctx context.Context, w http.ResponseWriter, r *http.Request, m merchants.Merchant,
	req idempotency.Request, token, paymentID string, result acquirer.Result) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rec, err := idempotency.Claim(ctx, tx, req, token)
	if err != nil {
		// Another request took the key over while we waited on the
		// acquirer. It will record the same acquirer decision.
		s.writeIdempotencyError(w, r, err)
		return
	}
	if rec.State == idempotency.StateFinished {
		writeReplay(w, rec.Response) // the request that took over finished first
		return
	}

	p, err := payments.GetForUpdate(ctx, tx, m.ID, paymentID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var (
		eventType = webhooks.EventPaymentAuthorized
		status    = http.StatusOK
		value     any
	)
	if result.Approved {
		err = p.MarkAuthorized(result.Reference)
	} else {
		err = p.MarkDeclined(result.DeclineCode, result.DeclineMessage)
		eventType = webhooks.EventPaymentFailed
	}
	if err != nil {
		// The payment moved on in the meantime, e.g. another request with a
		// different key authorized it. Store the conflict.
		ae := fromDomain(err)
		s.saveAndWrite(ctx, w, r, tx, req, ae.Status, errorBody{ae})
		return
	}
	if err := payments.Update(ctx, tx, &p); err != nil {
		s.internalError(w, r, err)
		return
	}
	out := toPaymentJSON(p)
	if _, err := webhooks.Insert(ctx, tx, m.ID, eventType, p.ID, out); err != nil {
		s.internalError(w, r, err)
		return
	}
	value = out
	if !result.Approved {
		status = http.StatusPaymentRequired
		value = errorBody{&apiError{Status: status, Type: typeCard, Code: "card_declined", DeclineCode: result.DeclineCode,
			Message: result.DeclineMessage, PaymentID: p.ID}}
	}
	s.saveAndWrite(ctx, w, r, tx, req, status, value)
}

// saveAndWrite stores the response under the key, commits tx, and sends it.
func (s *Server) saveAndWrite(ctx context.Context, w http.ResponseWriter, r *http.Request, tx pgx.Tx,
	req idempotency.Request, status int, value any) {
	resp, err := encodeResponse(status, value)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := idempotency.Save(ctx, tx, req, resp); err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeStored(w, resp)
}

func acquirerError(err error) *apiError {
	switch {
	case errors.Is(err, acquirer.ErrUnavailable):
		return &apiError{Status: http.StatusServiceUnavailable, Type: typeAPI, Code: "acquirer_unavailable",
			Message: "the card network is unavailable; retry with the same Idempotency-Key", retryAfter: 1}
	case errors.Is(err, acquirer.ErrKeyReused):
		return errInternal
	default: // timeout, cancellation, anything else: outcome unknown
		return &apiError{Status: http.StatusGatewayTimeout, Type: typeAPI, Code: "acquirer_timeout",
			Message: "the card network did not answer in time; retry with the same Idempotency-Key to learn the outcome", retryAfter: 1}
	}
}
