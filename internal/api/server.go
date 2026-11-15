// Package api is Ledgerline's HTTP API: routing, authentication, request
// parsing, error responses, and the orchestration that runs each POST
// idempotently in one database transaction.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kamboj-sajan/ledgerline/internal/acquirer"
	"github.com/kamboj-sajan/ledgerline/internal/failpoint"
	"github.com/kamboj-sajan/ledgerline/internal/idempotency"
	"github.com/kamboj-sajan/ledgerline/internal/merchants"
)

// maxBodyBytes caps request bodies; larger ones get 413.
const maxBodyBytes = 1 << 20

// Config tunes the server.
type Config struct {
	// AuthorizeLockTimeout is how long an authorize request may hold its
	// idempotency key between transactions before another request with the
	// same key may take over (for example after the first one crashed).
	AuthorizeLockTimeout time.Duration
	// AcquirerTimeout bounds each call to the acquirer.
	AcquirerTimeout time.Duration
}

// DefaultConfig returns production-style defaults.
func DefaultConfig() Config {
	return Config{AuthorizeLockTimeout: 30 * time.Second, AcquirerTimeout: 10 * time.Second}
}

// Server serves the API.
type Server struct {
	db  *pgxpool.Pool
	acq acquirer.Acquirer
	cfg Config
	log *slog.Logger
}

// New returns a Server.
func New(db *pgxpool.Pool, acq acquirer.Acquirer, cfg Config, log *slog.Logger) *Server {
	return &Server{db: db, acq: acq, cfg: cfg, log: log}
}

type authedHandler func(w http.ResponseWriter, r *http.Request, m merchants.Merchant)

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	routes := []struct {
		method, path string
		h            authedHandler
	}{
		{"POST", "/v1/payments", s.createPayment},
		{"GET", "/v1/payments/{id}", s.getPayment},
		{"POST", "/v1/payments/{id}/authorize", s.authorize},
		{"POST", "/v1/payments/{id}/capture", s.capture},
		{"POST", "/v1/payments/{id}/refunds", s.refund},
		{"POST", "/v1/payments/{id}/void", s.void},
		{"GET", "/v1/payments/{id}/ledger", s.paymentLedger},
		{"GET", "/v1/accounts/{id}/balance", s.balance},
		{"POST", "/v1/webhook_endpoints", s.createWebhookEndpoint},
		{"POST", "/v1/webhook_endpoints/{id}/rotate_secret", s.rotateWebhookSecret},
		{"GET", "/v1/events", s.listEvents},
		{"GET", "/v1/events/{id}", s.getEvent},
		{"POST", "/v1/events/{id}/redeliver", s.redeliverEvent},
	}
	allowed := map[string][]string{}
	for _, rt := range routes {
		mux.Handle(rt.method+" "+rt.path, s.authenticate(rt.h))
		allowed[rt.path] = append(allowed[rt.path], rt.method)
	}
	// A pattern without a method is less specific than one with a method, so
	// these only see requests whose method no route accepts: answer with a
	// JSON 405 instead of the mux's plain-text one.
	for path, methods := range allowed {
		allow := strings.Join(methods, ", ")
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Allow", allow)
			writeError(w, &apiError{Status: http.StatusMethodNotAllowed, Type: typeInvalidRequest, Code: "method_not_allowed",
				Message: "this endpoint accepts " + allow})
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, &apiError{Status: http.StatusNotFound, Type: typeInvalidRequest, Code: "route_not_found", Message: "no such endpoint"})
	})
	return s.recoverPanics(s.logRequests(mux))
}

// authenticate resolves the bearer API key to a merchant. Failure is a 401
// and nothing else happens: an unauthenticated request never touches an
// idempotency key.
func (s *Server) authenticate(h authedHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || key == "" {
			writeError(w, errUnauthenticated)
			return
		}
		m, err := merchants.Authenticate(r.Context(), s.db, key)
		if errors.Is(err, merchants.ErrUnauthenticated) {
			writeError(w, errUnauthenticated)
			return
		}
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		h(w, r, m)
	})
}

// readBody reads the request body (at most maxBodyBytes).
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, *apiError) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return nil, &apiError{Status: http.StatusRequestEntityTooLarge, Type: typeInvalidRequest, Code: "request_too_large",
			Message: "request body is larger than 1 MiB"}
	}
	if err != nil {
		return nil, badRequest("invalid_body", "", "could not read the request body")
	}
	return body, nil
}

// decodeStrict decodes body into v, rejecting unknown fields, trailing data
// and non-integer numbers for integer fields. An empty body decodes as {}.
func decodeStrict(body []byte, v any) *apiError {
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return paramInvalid(typeErr.Field, "%q must be a JSON %s (amounts are integers in minor units)", typeErr.Field, typeErr.Type)
		}
		return badRequest("invalid_json", "", "request body is not valid JSON for this endpoint: %v", err)
	}
	if dec.More() {
		return badRequest("invalid_json", "", "unexpected data after the JSON body")
	}
	return nil
}

// txOp is the body of an idempotent request. It runs inside the request's
// database transaction and returns the status and value to respond with. An
// error is mapped by fromDomain; a stored error becomes the saved response,
// any other error rolls the transaction back. Because a stored error commits
// the transaction, an op must return business-rule errors before it writes
// anything (the domain methods check state and amounts first, so they do).
type txOp func(ctx context.Context, tx pgx.Tx) (int, any, error)

// runIdempotent executes a single-transaction POST under its Idempotency-Key.
//
// Everything happens in one database transaction: take the key's advisory
// lock (or 409 idempotency_key_in_use), look up a stored response (replay it,
// or 409 idempotency_key_reused for a different request), run op, store the
// response, commit. A crash or error before COMMIT leaves no trace, so the
// key is not used up and a retry runs the request from scratch.
func (s *Server) runIdempotent(w http.ResponseWriter, r *http.Request, m merchants.Merchant, body []byte, op txOp) {
	req, apiErr := keyedRequest(r, m, body)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	if err := idempotency.Lock(ctx, tx, req); err != nil {
		s.writeIdempotencyError(w, r, err)
		return
	}
	rec, err := idempotency.Lookup(ctx, tx, req, s.cfg.AuthorizeLockTimeout)
	if err != nil {
		s.writeIdempotencyError(w, r, err)
		return
	}
	switch rec.State {
	case idempotency.StateFinished:
		writeReplay(w, rec.Response)
		return
	case idempotency.StateStarted:
		// Only authorize leaves started records, and its path is part of
		// the fingerprint, so a match here means a bug.
		s.internalError(w, r, errors.New("api: started idempotency record on a single-transaction endpoint"))
		return
	}

	status, value, err := op(ctx, tx)
	if err != nil {
		ae := fromDomain(err)
		if !ae.stored {
			if ae == errInternal {
				s.internalError(w, r, err)
				return
			}
			writeError(w, ae)
			return
		}
		status, value = ae.Status, errorBody{ae}
	}
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
	failpoint.Inject("api-after-commit")
	writeStored(w, resp)
}

// keyedRequest validates the Idempotency-Key header and fingerprints the request.
func keyedRequest(r *http.Request, m merchants.Merchant, body []byte) (idempotency.Request, *apiError) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return idempotency.Request{}, badRequest("idempotency_key_missing", "", "every POST needs an Idempotency-Key header")
	}
	if err := idempotency.ValidateKey(key); err != nil {
		return idempotency.Request{}, badRequest("idempotency_key_invalid", "", "%v", err)
	}
	hash, err := idempotency.Fingerprint(r.Method, r.URL.Path, body)
	if err != nil {
		return idempotency.Request{}, badRequest("invalid_json", "", "request body is not valid JSON")
	}
	return idempotency.Request{MerchantID: m.ID, Key: key, Method: r.Method, Path: r.URL.Path, Hash: hash}, nil
}

func (s *Server) writeIdempotencyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, idempotency.ErrInUse):
		writeError(w, errKeyInUse)
	case errors.Is(err, idempotency.ErrReused):
		writeError(w, errKeyReused)
	default:
		s.internalError(w, r, err)
	}
}

func encodeResponse(status int, value any) (idempotency.Response, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return idempotency.Response{}, err
	}
	return idempotency.Response{Status: status, Body: b}, nil
}

func writeStored(w http.ResponseWriter, resp idempotency.Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// writeReplay sends a stored response exactly as first sent.
func writeReplay(w http.ResponseWriter, resp idempotency.Response) {
	w.Header().Set("Idempotent-Replayed", "true")
	writeStored(w, resp)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	resp, err := encodeResponse(status, value)
	if err != nil {
		writeError(w, errInternal)
		return
	}
	writeStored(w, resp)
}

func writeError(w http.ResponseWriter, e *apiError) {
	if e.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.retryAfter))
	}
	writeJSON(w, e.Status, errorBody{e})
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("internal error", "method", r.Method, "path", r.URL.Path, "error", err)
	writeError(w, errInternal)
}

// statusRecorder captures the status code for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start))
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
					panic(v)
				}
				s.log.Error("panic", "method", r.Method, "path", r.URL.Path, "panic", v)
				writeError(w, errInternal)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
