//go:build integration

package integration

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/payments"
)

func TestAPIPaymentLifecycle(t *testing.T) {
	e := newAPIEnv(t)
	p := e.createPayment("fp_ok", 10_000)
	if p.Status != "created" || p.Amount != 10_000 || p.Currency != "USD" || p.AmountAuthorized != 0 {
		t.Fatalf("created payment = %+v", p)
	}
	path := "/v1/payments/" + p.ID

	auth := decode[paymentResp](t, e.post(path+"/authorize", ""))
	if auth.Status != "authorized" || auth.AmountAuthorized != 10_000 {
		t.Fatalf("authorized = %+v", auth)
	}
	// Partial capture of 60.00: fee is 2.9% (1.74) + 0.30 = 2.04.
	cap := decode[paymentResp](t, e.post(path+"/capture", `{"amount": 6000}`))
	if cap.Status != "captured" || cap.AmountCaptured != 6_000 || cap.Fee != 204 {
		t.Fatalf("captured = %+v", cap)
	}
	r := e.post(path+"/refunds", `{"amount": 1000}`)
	wantStatus(t, r, http.StatusCreated)
	refund := decode[struct {
		ID      string      `json:"id"`
		Amount  int64       `json:"amount"`
		Payment paymentResp `json:"payment"`
	}](t, r)
	if !strings.HasPrefix(refund.ID, "re_") || refund.Amount != 1_000 || refund.Payment.Status != "captured" || refund.Payment.AmountRefunded != 1_000 {
		t.Fatalf("partial refund = %+v", refund)
	}
	// No amount: refund whatever is left.
	rest := decode[struct {
		Amount  int64       `json:"amount"`
		Payment paymentResp `json:"payment"`
	}](t, e.post(path+"/refunds", ""))
	if rest.Amount != 5_000 || rest.Payment.Status != "refunded" || rest.Payment.AmountRefunded != 6_000 {
		t.Fatalf("final refund = %+v", rest)
	}

	got := decode[paymentResp](t, e.do("GET", path, "", ""))
	if got.Status != "refunded" || got.AmountRefunded != 6_000 || got.Fee != 204 {
		t.Fatalf("GET = %+v", got)
	}

	ledgerResp := decode[struct {
		Data []struct {
			Kind    string `json:"kind"`
			Entries []struct {
				AccountID  string `json:"account_id"`
				Account    string `json:"account"`
				MerchantID string `json:"merchant_id"`
				Amount     int64  `json:"amount"`
			} `json:"entries"`
		} `json:"data"`
	}](t, e.do("GET", path+"/ledger", "", ""))
	if len(ledgerResp.Data) != 3 || ledgerResp.Data[0].Kind != "capture" || ledgerResp.Data[1].Kind != "refund" {
		t.Fatalf("ledger = %+v", ledgerResp)
	}
	var payableID string
	for _, en := range ledgerResp.Data[0].Entries {
		if en.MerchantID == e.merchantID {
			payableID = en.AccountID
		}
	}

	// We owed the merchant 60.00 - 2.04 and they refunded 60.00, so they
	// owe us the 2.04 fee (ADR 0002): a normal balance of -204.
	bal := decode[struct {
		Code          string `json:"code"`
		Balance       int64  `json:"balance"`
		SignedBalance int64  `json:"signed_balance"`
	}](t, e.do("GET", "/v1/accounts/"+payableID+"/balance", "", ""))
	if bal.Code != "merchant_payable" || bal.Balance != -204 || bal.SignedBalance != 204 {
		t.Fatalf("balance = %+v", bal)
	}

	types := e.count(`SELECT count(*) FROM events WHERE payment_id = $1`, p.ID)
	if types != 5 { // created, authorized, captured, refunded ×2
		t.Fatalf("events = %d, want 5", types)
	}
	e.mustAudit()
}

func TestAPIErrors(t *testing.T) {
	e := newAPIEnv(t)
	other := newAPIEnv(t) // a second merchant on a separate database
	p := e.createPayment("fp_ok", 10_000)

	wantError(t, e.doWith("", "GET", "/v1/payments/"+p.ID, "", ""), 401, "invalid_api_key")
	wantError(t, e.doWith("sk_wrong", "GET", "/v1/payments/"+p.ID, "", ""), 401, "invalid_api_key")
	wantError(t, e.do("POST", "/v1/payments", "", paymentBody("fp_ok", 100)), 400, "idempotency_key_missing")
	wantError(t, e.do("POST", "/v1/payments", "has space", paymentBody("fp_ok", 100)), 400, "idempotency_key_invalid")
	wantError(t, e.post("/v1/payments", `{"amount": 100,`), 400, "invalid_json")
	wantError(t, e.post("/v1/payments", `{"amount": 100, "currency": "USD", "surprise": 1}`), 400, "invalid_json")
	for _, amount := range []string{"12.5", "1e3", `"100"`, "100.0"} {
		r := e.post("/v1/payments", strings.Replace(paymentBody("fp_ok", 1), `"amount": 1`, `"amount": `+amount, 1))
		if er := wantError(t, r, 400, "parameter_invalid"); er.Error.Param != "amount" {
			t.Errorf("amount %s: param = %q", amount, er.Error.Param)
		}
	}
	wantError(t, e.post("/v1/payments", paymentBody("fp_ok", 0)), 400, "parameter_invalid")
	wantError(t, e.post("/v1/payments", strings.Replace(paymentBody("fp_ok", 100), `"USD"`, `"usd"`, 1)), 400, "parameter_invalid")
	wantError(t, e.post("/v1/payments", `{"amount": 100, "currency": "USD"}`), 400, "parameter_missing")
	wantError(t, e.post("/v1/payments/"+p.ID+"/capture", `{"amount": -5}`), 400, "parameter_invalid")
	wantError(t, e.do("GET", "/v1/nope", "", ""), 404, "route_not_found")
	wantError(t, e.do("GET", "/v1/payments/pay_missing", "", ""), 404, "resource_missing")
	wantError(t, other.do("GET", "/v1/payments/"+p.ID, "", ""), 404, "resource_missing")
	wantError(t, other.post("/v1/payments/"+p.ID+"/authorize", ""), 404, "resource_missing")

	r := e.do("DELETE", "/v1/payments/"+p.ID, "", "")
	wantError(t, r, 405, "method_not_allowed")
	if r.Header.Get("Allow") != "GET" {
		t.Errorf("Allow = %q", r.Header.Get("Allow"))
	}
	big := `{"amount": 100, "metadata": {"x": "` + strings.Repeat("a", 1<<20) + `"}}`
	wantError(t, e.post("/v1/payments", big), 413, "request_too_large")

	// Nothing above executed, so none of it created a payment or an event.
	if n := e.count(`SELECT count(*) FROM payments`); n != 1 {
		t.Fatalf("payments = %d, want 1", n)
	}
}

// TestAPIStateMachineEveryStateAndOperation sends each operation to a payment
// in each state over HTTP. An operation allowed by the transition table must
// succeed; every other one must be 409 invalid_state_transition.
func TestAPIStateMachineEveryStateAndOperation(t *testing.T) {
	e := newAPIEnv(t)
	ops := []struct {
		name   string
		path   string
		event  payments.Event
		status int
	}{
		{"authorize", "/authorize", payments.EventApprove, 200},
		{"capture", "/capture", payments.EventCapture, 200},
		{"refund", "/refunds", payments.EventPartialRefund, 201},
		{"void", "/void", payments.EventVoid, 200},
	}
	for _, from := range payments.Statuses {
		for _, op := range ops {
			t.Run(op.name+" from "+string(from), func(t *testing.T) {
				pid := e.paymentIn(from)
				r := e.post("/v1/payments/"+pid+op.path, "")
				if _, ok := payments.Next(from, op.event); ok {
					wantStatus(t, r, op.status)
					return
				}
				wantError(t, r, 409, "invalid_state_transition")
				if got := decode[paymentResp](t, e.do("GET", "/v1/payments/"+pid, "", "")); got.Status != string(from) {
					t.Fatalf("rejected %s changed status to %s", op.name, got.Status)
				}
			})
		}
	}
	e.mustAudit()
}

func TestAPICaptureAndRefundLimits(t *testing.T) {
	e := newAPIEnv(t)
	pid := e.paymentIn(payments.StatusAuthorized)
	path := "/v1/payments/" + pid
	wantError(t, e.post(path+"/capture", `{"amount": 10001}`), 400, "amount_too_large")
	wantStatus(t, e.post(path+"/capture", `{"amount": 8000}`), 200)
	wantError(t, e.post(path+"/refunds", `{"amount": 8001}`), 400, "amount_too_large")
	wantStatus(t, e.post(path+"/refunds", `{"amount": 8000}`), 201)
	wantError(t, e.post(path+"/refunds", `{"amount": 1}`), 409, "invalid_state_transition")
	e.mustAudit()
}

func TestAPIIdempotency(t *testing.T) {
	e := newAPIEnv(t)
	body := paymentBody("fp_ok", 10_000)

	t.Run("replay returns the stored response byte for byte without executing again", func(t *testing.T) {
		key := newKey()
		first := e.do("POST", "/v1/payments", key, body)
		wantStatus(t, first, 201)
		// Same request, formatted differently: still the same request.
		again := e.do("POST", "/v1/payments", key, strings.Join(strings.Fields(body), " "))
		if again.Status != first.Status || !bytes.Equal(again.Body, first.Body) {
			t.Fatalf("replay differs:\n%s\n%s", first, again)
		}
		if again.Header.Get("Idempotent-Replayed") != "true" || first.Header.Get("Idempotent-Replayed") != "" {
			t.Fatal("Idempotent-Replayed header wrong")
		}
		pid := decode[paymentResp](t, first).ID
		if n := e.count(`SELECT count(*) FROM events WHERE payment_id = $1`, pid); n != 1 {
			t.Fatalf("events = %d, want 1 (the replay must not execute)", n)
		}
	})

	t.Run("same key, different request is rejected and not executed", func(t *testing.T) {
		key := newKey()
		wantStatus(t, e.do("POST", "/v1/payments", key, body), 201)
		before := e.count(`SELECT count(*) FROM payments`)
		wantError(t, e.do("POST", "/v1/payments", key, paymentBody("fp_ok", 9_999)), 409, "idempotency_key_reused")
		if after := e.count(`SELECT count(*) FROM payments`); after != before {
			t.Fatalf("payments %d → %d", before, after)
		}
	})

	t.Run("requests rejected before executing do not use up the key", func(t *testing.T) {
		key := newKey()
		wantError(t, e.doWith("sk_wrong", "POST", "/v1/payments", key, body), 401, "invalid_api_key")
		wantError(t, e.do("POST", "/v1/payments", key, `{"amount": 1.5}`), 400, "parameter_invalid")
		wantError(t, e.do("POST", "/v1/payments/pay_missing/capture", key, ""), 404, "resource_missing")
		// The key is still fresh: a valid request with it executes.
		wantStatus(t, e.do("POST", "/v1/payments", key, body), 201)
	})

	t.Run("business-rule rejections are stored and replayed", func(t *testing.T) {
		pid := e.createPayment("fp_ok", 10_000).ID
		key := newKey()
		first := e.do("POST", "/v1/payments/"+pid+"/capture", key, "")
		wantError(t, first, 409, "invalid_state_transition")
		wantStatus(t, e.post("/v1/payments/"+pid+"/authorize", ""), 200)
		// Capturing would now succeed, but this key already has its answer.
		again := e.do("POST", "/v1/payments/"+pid+"/capture", key, "")
		if again.Status != 409 || !bytes.Equal(again.Body, first.Body) {
			t.Fatalf("replay = %s, want the stored 409", again)
		}
	})

	t.Run("keys are scoped per merchant", func(t *testing.T) {
		// Another merchant in the same database may use the same key.
		key := newKey()
		wantStatus(t, e.do("POST", "/v1/payments", key, body), 201)
		_, otherKey := createMerchant(t, e)
		r := e.doWith(otherKey, "POST", "/v1/payments", key, body)
		wantStatus(t, r, 201)
		if r.Header.Get("Idempotent-Replayed") != "" {
			t.Fatal("a different merchant got a replay")
		}
	})

	t.Run("an expired key is forgotten", func(t *testing.T) {
		key := newKey()
		first := decode[paymentResp](t, e.do("POST", "/v1/payments", key, body))
		mustExec(t, e.db, `UPDATE idempotency_keys SET expires_at = now() - interval '1 second' WHERE key = $1`, key)
		second := decode[paymentResp](t, e.do("POST", "/v1/payments", key, body))
		if second.ID == first.ID {
			t.Fatal("expired key replayed instead of executing")
		}
	})
}

func createMerchant(t *testing.T, e *apiEnv) (string, string) {
	t.Helper()
	m, key, err := merchantsCreate(t, e)
	if err != nil {
		t.Fatal(err)
	}
	return m, key
}

func TestAPIExpireAuthorizations(t *testing.T) {
	e := newAPIEnv(t)
	old := e.paymentIn(payments.StatusAuthorized)
	fresh := e.paymentIn(payments.StatusAuthorized)
	mustExec(t, e.db, `UPDATE payments SET updated_at = now() - interval '8 days' WHERE id = $1`, old)

	n, err := e.server.ExpireAuthorizations(t.Context(), 7*24*time.Hour, 100)
	if err != nil || n != 1 {
		t.Fatalf("ExpireAuthorizations = %d, %v; want 1", n, err)
	}
	got := decode[paymentResp](t, e.do("GET", "/v1/payments/"+old, "", ""))
	if got.Status != "failed" || got.FailureCode == nil || *got.FailureCode != payments.FailureExpired {
		t.Fatalf("expired payment = %+v", got)
	}
	if s := decode[paymentResp](t, e.do("GET", "/v1/payments/"+fresh, "", "")).Status; s != "authorized" {
		t.Fatalf("fresh authorization became %s", s)
	}
	wantError(t, e.post("/v1/payments/"+old+"/capture", ""), 409, "invalid_state_transition")
	if n := e.count(`SELECT count(*) FROM events WHERE payment_id = $1 AND type = 'payment.failed'`, old); n != 1 {
		t.Fatalf("payment.failed events = %d, want 1", n)
	}
}
