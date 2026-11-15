//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/kamboj-sajan/ledgerline/internal/acquirer"
	"github.com/kamboj-sajan/ledgerline/internal/api"
	"github.com/kamboj-sajan/ledgerline/internal/id"
	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/merchants"
	"github.com/kamboj-sajan/ledgerline/internal/money"
	"github.com/kamboj-sajan/ledgerline/internal/payments"
	"github.com/kamboj-sajan/ledgerline/internal/payments/paymenttest"
)

// TestPropPaymentsAPI runs the payment-lifecycle property test end to end:
// every operation is an HTTP request to the real API on a real Postgres,
// with a fresh database per generated case, and the full ledger and
// payments-vs-ledger audit runs after every step.
func TestPropPaymentsAPI(t *testing.T) {
	var stats paymenttest.Stats
	start := time.Now()
	rapid.Check(t, func(rt *rapid.T) {
		paymenttest.Run(rt, newHTTPSystem(rt), &stats)
	})
	t.Logf("operations: %d (%d accepted, %d rejected) in %s",
		stats.Ops, stats.Accepted, stats.Rejected, time.Since(start).Round(time.Millisecond))
}

type httpSystem struct {
	db   *pgxpool.Pool
	srv  *httptest.Server
	keys map[string]string // merchant name -> API key
	ids  map[string]string // merchant name -> merchant ID
}

func newHTTPSystem(rt *rapid.T) *httpSystem {
	db := pg.NewPool(rt)
	s := &httpSystem{db: db, keys: map[string]string{}, ids: map[string]string{}}
	for _, name := range []string{"m1", "m2"} {
		m, key, err := merchants.Create(rt.Context(), db, name)
		if err != nil {
			rt.Fatalf("creating merchant: %v", err)
		}
		if m.Pricing != paymenttest.Pricing {
			rt.Fatalf("default pricing %+v differs from the model's %+v", m.Pricing, paymenttest.Pricing)
		}
		s.keys[name], s.ids[name] = key, m.ID
	}
	srv := api.New(db, acquirer.NewSimulator(db, acquirer.Config{Seed: 11}), api.DefaultConfig(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.srv = httptest.NewServer(srv.Handler())
	rt.Cleanup(s.srv.Close)
	return s
}

func (s *httpSystem) MerchantID(name string) string { return s.ids[name] }

func (s *httpSystem) send(rt *rapid.T, merchant, method, path, body string) (int, []byte) {
	req, err := http.NewRequestWithContext(rt.Context(), method, s.srv.URL+path, bytes.NewBufferString(body))
	if err != nil {
		rt.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.keys[merchant])
	if method == "POST" {
		req.Header.Set("Idempotency-Key", id.New("idem"))
	}
	resp, err := s.srv.Client().Do(req)
	if err != nil {
		rt.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		rt.Fatalf("reading response: %v", err)
	}
	return resp.StatusCode, b
}

// op sends a request and maps the response to an Outcome.
func (s *httpSystem) op(rt *rapid.T, merchant, method, path, reqBody string) paymenttest.Outcome {
	status, body := s.send(rt, merchant, method, path, reqBody)
	if status >= 200 && status < 300 {
		return paymenttest.OK
	}
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	switch e.Error.Code {
	case "card_declined":
		return paymenttest.Declined
	case "invalid_state_transition":
		return paymenttest.InvalidTransition
	case "amount_too_large":
		return paymenttest.AmountTooLarge
	case "resource_missing":
		return paymenttest.NotFound
	}
	rt.Fatalf("unexpected response %d %s", status, body)
	return ""
}

func amountBody(amount *int64) string {
	if amount == nil {
		return ""
	}
	return `{"amount": ` + itoa(*amount) + `}`
}

func (s *httpSystem) Create(rt *rapid.T, merchant string, amount money.Money, declines bool) string {
	card := "fp_ok"
	if declines {
		card = acquirer.CardDecline
	}
	body := `{"amount": ` + itoa(amount.Amount()) + `, "currency": "` + amount.Currency().Code() + `", "customer_id": "cus",
		"card": {"fingerprint": "` + card + `", "country": "US"}, "ip_country": "US"}`
	status, b := s.send(rt, merchant, "POST", "/v1/payments", body)
	if status != http.StatusCreated {
		rt.Fatalf("create: %d %s", status, b)
	}
	var p struct{ ID string }
	if err := json.Unmarshal(b, &p); err != nil {
		rt.Fatalf("create: %v", err)
	}
	return p.ID
}

func (s *httpSystem) Authorize(rt *rapid.T, merchant, pid string) paymenttest.Outcome {
	return s.op(rt, merchant, "POST", "/v1/payments/"+pid+"/authorize", "")
}

func (s *httpSystem) Capture(rt *rapid.T, merchant, pid string, amount *int64) paymenttest.Outcome {
	return s.op(rt, merchant, "POST", "/v1/payments/"+pid+"/capture", amountBody(amount))
}

func (s *httpSystem) Refund(rt *rapid.T, merchant, pid string, amount *int64) paymenttest.Outcome {
	return s.op(rt, merchant, "POST", "/v1/payments/"+pid+"/refunds", amountBody(amount))
}

func (s *httpSystem) Void(rt *rapid.T, merchant, pid string) paymenttest.Outcome {
	return s.op(rt, merchant, "POST", "/v1/payments/"+pid+"/void", "")
}

// Settle posts directly to the ledger: settlement arrives from the bank file
// (reconciliation, M5), not through the merchant API.
func (s *httpSystem) Settle(rt *rapid.T, pid string, gross money.Money) paymenttest.Outcome {
	lt, err := ledger.Settlement(pid, "stl_"+pid, gross, money.Zero(gross.Currency()))
	if err != nil {
		rt.Fatalf("settlement: %v", err)
	}
	ctx := rt.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		rt.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := ledger.Post(ctx, tx, lt); errors.Is(err, ledger.ErrDuplicateExternalRef) {
		return paymenttest.DuplicateSettlement
	} else if err != nil {
		rt.Fatalf("settlement: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		rt.Fatalf("commit: %v", err)
	}
	return paymenttest.OK
}

func (s *httpSystem) Payment(rt *rapid.T, merchant, pid string) paymenttest.Snapshot {
	status, b := s.send(rt, merchant, "GET", "/v1/payments/"+pid, "")
	if status != http.StatusOK {
		rt.Fatalf("GET payment: %d %s", status, b)
	}
	var raw struct {
		Status           string `json:"status"`
		AmountAuthorized int64  `json:"amount_authorized"`
		AmountCaptured   int64  `json:"amount_captured"`
		AmountRefunded   int64  `json:"amount_refunded"`
		Fee              int64  `json:"fee"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		rt.Fatalf("GET payment: %v", err)
	}
	return paymenttest.Snapshot{Status: payments.Status(raw.Status), Authorized: raw.AmountAuthorized,
		Captured: raw.AmountCaptured, Refunded: raw.AmountRefunded, Fee: raw.Fee}
}

func (s *httpSystem) Balances(rt *rapid.T) map[ledger.AccountKey]money.Money {
	return (&pgSystem{db: s.db}).Balances(rt)
}

func (s *httpSystem) CheckGlobal(rt *rapid.T) {
	lr, err := ledger.Audit(rt.Context(), s.db)
	if err != nil {
		rt.Fatalf("ledger audit: %v", err)
	}
	pr, err := payments.Audit(rt.Context(), s.db)
	if err != nil {
		rt.Fatalf("payments audit: %v", err)
	}
	if !lr.OK() || !pr.OK() {
		rt.Fatalf("audit failed:\nledger %+v\npayments %+v", lr, pr)
	}
}

var _ paymenttest.System = (*httpSystem)(nil)
