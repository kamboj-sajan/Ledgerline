//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kamboj-sajan/ledgerline/internal/acquirer"
	"github.com/kamboj-sajan/ledgerline/internal/api"
	"github.com/kamboj-sajan/ledgerline/internal/id"
	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/merchants"
	"github.com/kamboj-sajan/ledgerline/internal/payments"
)

// apiEnv is a running API server on its own database, with one merchant.
type apiEnv struct {
	t          *testing.T
	db         *pgxpool.Pool
	server     *api.Server
	http       *httptest.Server
	apiKey     string
	merchantID string
}

type envOption func(*envConfig)

type envConfig struct {
	cfg     api.Config
	makeAcq func(db *pgxpool.Pool) acquirer.Acquirer
}

func withConfig(c api.Config) envOption { return func(e *envConfig) { e.cfg = c } }
func withAcquirer(f func(db *pgxpool.Pool) acquirer.Acquirer) envOption {
	return func(e *envConfig) { e.makeAcq = f }
}

func newAPIEnv(t *testing.T, opts ...envOption) *apiEnv {
	t.Helper()
	ec := envConfig{
		cfg: api.Config{AuthorizeLockTimeout: 30 * time.Second, AcquirerTimeout: 5 * time.Second},
		makeAcq: func(db *pgxpool.Pool) acquirer.Acquirer {
			return acquirer.NewSimulator(db, acquirer.Config{Seed: 42})
		},
	}
	for _, o := range opts {
		o(&ec)
	}
	db := pg.NewPool(t)
	m, key, err := merchants.Create(t.Context(), db, "Test Merchant")
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(db, ec.makeAcq(db), ec.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return &apiEnv{t: t, db: db, server: srv, http: hs, apiKey: key, merchantID: m.ID}
}

type response struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r response) String() string { return http.StatusText(r.Status) + " " + string(r.Body) }

// do sends a request as the env's merchant. idemKey "" sends no
// Idempotency-Key header.
func (e *apiEnv) do(method, path, idemKey, body string) response {
	e.t.Helper()
	return e.doWith(e.apiKey, method, path, idemKey, body)
}

func (e *apiEnv) doWith(apiKey, method, path, idemKey, body string) response {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), method, e.http.URL+path, bytes.NewBufferString(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.http.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return response{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

// post sends a POST with a fresh idempotency key.
func (e *apiEnv) post(path, body string) response {
	e.t.Helper()
	return e.do("POST", path, newKey(), body)
}

func newKey() string { return id.New("idem") }

func decode[T any](t *testing.T, r response) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.Body, &v); err != nil {
		t.Fatalf("decoding %s: %v", r, err)
	}
	return v
}

type paymentResp struct {
	ID               string  `json:"id"`
	Status           string  `json:"status"`
	Amount           int64   `json:"amount"`
	Currency         string  `json:"currency"`
	AmountAuthorized int64   `json:"amount_authorized"`
	AmountCaptured   int64   `json:"amount_captured"`
	AmountRefunded   int64   `json:"amount_refunded"`
	Fee              int64   `json:"fee"`
	FailureCode      *string `json:"failure_code"`
}

type errorResp struct {
	Error struct {
		Type        string `json:"type"`
		Code        string `json:"code"`
		Message     string `json:"message"`
		Param       string `json:"param"`
		DeclineCode string `json:"decline_code"`
	} `json:"error"`
}

func wantStatus(t *testing.T, r response, status int) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("status %d, want %d: %s", r.Status, status, r.Body)
	}
}

// wantError checks status and error code.
func wantError(t *testing.T, r response, status int, code string) errorResp {
	t.Helper()
	wantStatus(t, r, status)
	e := decode[errorResp](t, r)
	if e.Error.Code != code || e.Error.Type == "" || e.Error.Message == "" {
		t.Fatalf("error = %+v, want code %q with a type and a message", e.Error, code)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	return e
}

func paymentBody(card string, amount int64) string {
	return `{"amount": ` + itoa(amount) + `, "currency": "USD", "customer_id": "cus_1",
		"card": {"fingerprint": "` + card + `", "country": "US"}, "ip_country": "US", "metadata": {"order": "42"}}`
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func (e *apiEnv) createPayment(card string, amount int64) paymentResp {
	e.t.Helper()
	r := e.post("/v1/payments", paymentBody(card, amount))
	wantStatus(e.t, r, http.StatusCreated)
	return decode[paymentResp](e.t, r)
}

// paymentIn creates a 100.00 USD payment and drives it to status s through the API.
func (e *apiEnv) paymentIn(s payments.Status) string {
	e.t.Helper()
	card := "fp_ok"
	if s == payments.StatusFailed {
		card = acquirer.CardDecline
	}
	p := e.createPayment(card, 10_000)
	path := "/v1/payments/" + p.ID
	switch s {
	case payments.StatusCreated:
	case payments.StatusAuthorized:
		wantStatus(e.t, e.post(path+"/authorize", ""), 200)
	case payments.StatusCaptured:
		wantStatus(e.t, e.post(path+"/authorize", ""), 200)
		wantStatus(e.t, e.post(path+"/capture", ""), 200)
	case payments.StatusRefunded:
		wantStatus(e.t, e.post(path+"/authorize", ""), 200)
		wantStatus(e.t, e.post(path+"/capture", ""), 200)
		wantStatus(e.t, e.post(path+"/refunds", ""), 201)
	case payments.StatusFailed:
		wantStatus(e.t, e.post(path+"/authorize", ""), 402)
	}
	got := decode[paymentResp](e.t, e.do("GET", path, "", ""))
	if got.Status != string(s) {
		e.t.Fatalf("set-up reached %s, want %s", got.Status, s)
	}
	return p.ID
}

func (e *apiEnv) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(e.t.Context(), query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// mustAudit runs both audits and fails the test if either finds a problem.
func (e *apiEnv) mustAudit() {
	e.t.Helper()
	lr, err := ledger.Audit(e.t.Context(), e.db)
	if err != nil {
		e.t.Fatal(err)
	}
	pr, err := payments.Audit(e.t.Context(), e.db)
	if err != nil {
		e.t.Fatal(err)
	}
	if !lr.OK() || !pr.OK() {
		e.t.Fatalf("audit failed:\nledger %+v\npayments %+v", lr, pr)
	}
}

func merchantsCreate(t *testing.T, e *apiEnv) (string, string, error) {
	t.Helper()
	m, key, err := merchants.Create(t.Context(), e.db, "Other Merchant")
	return m.ID, key, err
}
