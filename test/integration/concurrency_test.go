//go:build integration

package integration

import (
	"bytes"
	"math/rand/v2"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/payments"
)

// These are the concurrency acceptance tests. Run them many times with the
// race detector: make test-concurrency (-count=20 -race).

// fire runs n requests at once (all goroutines released together) and
// returns the responses in goroutine order.
func fire(n int, send func(i int) response) []response {
	out := make([]response, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			out[i] = send(i)
		})
	}
	close(start)
	wg.Wait()
	return out
}

func (e *apiEnv) journalCount(paymentID, kind string) int {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM journal_transactions WHERE payment_id = $1 AND kind = $2`, paymentID, kind)
}

// (a) 100 goroutines capture one authorized payment, each with a different
// idempotency key: exactly one succeeds, 99 get 409 invalid_state_transition,
// and exactly one capture journal transaction exists.
func TestConcurrencyCaptureDifferentKeys(t *testing.T) {
	e := newAPIEnv(t)
	pid := e.paymentIn(payments.StatusAuthorized)
	const n = 100

	results := fire(n, func(int) response { return e.post("/v1/payments/"+pid+"/capture", "") })

	ok, conflicts := 0, 0
	for _, r := range results {
		switch r.Status {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			if er := decode[errorResp](t, r); er.Error.Code != "invalid_state_transition" {
				t.Fatalf("409 with code %q", er.Error.Code)
			}
			conflicts++
		default:
			t.Fatalf("unexpected response: %s", r)
		}
	}
	if ok != 1 || conflicts != n-1 {
		t.Fatalf("%d × 200 and %d × 409, want 1 and %d", ok, conflicts, n-1)
	}
	if c := e.journalCount(pid, "capture"); c != 1 {
		t.Fatalf("capture journal transactions = %d, want 1", c)
	}
	if c := e.count(`SELECT count(*) FROM events WHERE payment_id = $1 AND type = 'payment.captured'`, pid); c != 1 {
		t.Fatalf("payment.captured events = %d, want 1", c)
	}
	e.mustAudit()
}

// (b) 100 goroutines send the same key and body, retrying while the key is in
// use. All final responses are byte-identical and the capture happens once.
func TestConcurrencyCaptureSameKey(t *testing.T) {
	e := newAPIEnv(t)
	pid := e.paymentIn(payments.StatusAuthorized)
	key := newKey()
	const n = 100

	inUse := make([]int, n)
	results := fire(n, func(i int) response {
		for attempt := 0; ; attempt++ {
			r := e.do("POST", "/v1/payments/"+pid+"/capture", key, `{"amount": 7500}`)
			if r.Status != http.StatusConflict || decode[errorResp](t, r).Error.Code != "idempotency_key_in_use" {
				return r
			}
			if r.Header.Get("Retry-After") == "" {
				t.Errorf("idempotency_key_in_use without Retry-After")
			}
			inUse[i]++
			if attempt > 2000 {
				t.Errorf("goroutine %d still in use after %d attempts", i, attempt)
				return r
			}
			// Retry-After says 1s; a test client may poll faster. Jitter
			// spreads the retries out.
			time.Sleep(time.Duration(1+rand.IntN(5)) * time.Millisecond)
		}
	})

	first := results[0]
	wantStatus(t, first, http.StatusOK)
	for i, r := range results {
		if r.Status != first.Status || !bytes.Equal(r.Body, first.Body) {
			t.Fatalf("response %d differs:\n%s\n%s", i, r, first)
		}
	}
	if c := e.journalCount(pid, "capture"); c != 1 {
		t.Fatalf("capture journal transactions = %d, want 1", c)
	}
	if c := e.count(`SELECT count(*) FROM events WHERE payment_id = $1 AND type = 'payment.captured'`, pid); c != 1 {
		t.Fatalf("payment.captured events = %d, want 1", c)
	}
	total := 0
	for _, c := range inUse {
		total += c
	}
	t.Logf("%d idempotency_key_in_use responses were retried", total)
	e.mustAudit()
}

// (c) 50 parallel partial refunds whose total exceeds the captured amount:
// the refunded total never exceeds what was captured, and every number
// (payment row, refunds table, ledger) agrees.
func TestConcurrencyOverRefund(t *testing.T) {
	e := newAPIEnv(t)
	pid := e.paymentIn(payments.StatusCaptured) // 10,000 captured
	const n = 50
	amounts := make([]int64, n)
	var requested int64
	for i := range amounts {
		amounts[i] = int64(150 + 13*i) // 150 .. 787, all different, sum 23,425
		requested += amounts[i]
	}
	if requested <= 10_000 {
		t.Fatalf("test setup: requested %d must exceed the captured 10000", requested)
	}

	results := fire(n, func(i int) response {
		return e.post("/v1/payments/"+pid+"/refunds", `{"amount": `+itoa(amounts[i])+`}`)
	})

	var refunded int64
	succeeded := 0
	for i, r := range results {
		switch r.Status {
		case http.StatusCreated:
			refunded += amounts[i]
			succeeded++
		case http.StatusBadRequest:
			wantError(t, r, http.StatusBadRequest, "amount_too_large")
		case http.StatusConflict: // fully refunded before this one ran
			wantError(t, r, http.StatusConflict, "invalid_state_transition")
		default:
			t.Fatalf("unexpected response: %s", r)
		}
	}
	got := decode[paymentResp](t, e.do("GET", "/v1/payments/"+pid, "", ""))
	if refunded > got.AmountCaptured {
		t.Fatalf("refunded %d of %d captured", refunded, got.AmountCaptured)
	}
	if got.AmountRefunded != refunded {
		t.Fatalf("payment says %d refunded, successful responses add up to %d", got.AmountRefunded, refunded)
	}
	if c := e.journalCount(pid, "refund"); c != succeeded {
		t.Fatalf("refund journal transactions = %d, successful refunds = %d", c, succeeded)
	}
	wantStatusName := "captured"
	if refunded == got.AmountCaptured {
		wantStatusName = "refunded"
	}
	if got.Status != wantStatusName {
		t.Fatalf("status = %s with %d/%d refunded", got.Status, refunded, got.AmountCaptured)
	}
	t.Logf("%d of %d refunds succeeded, %d of %d refunded", succeeded, n, refunded, got.AmountCaptured)
	e.mustAudit() // includes refunds-table and ledger agreement
}

// TestConcurrencyReplaysOfFinishedKey: once a key's response is stored, any
// number of concurrent replays must all get it. "In use" is only for a
// request that is still running. Found by the k6 replay scenario: before the
// fix, replays took the key's advisory lock first, and about 40% of 200
// concurrent replays of a finished key got 409 idempotency_key_in_use.
func TestConcurrencyReplaysOfFinishedKey(t *testing.T) {
	e := newAPIEnv(t)
	key := newKey()
	body := paymentBody("fp_ok", 1_000)
	first := e.do("POST", "/v1/payments", key, body)
	wantStatus(t, first, http.StatusCreated)

	results := fire(100, func(int) response { return e.do("POST", "/v1/payments", key, body) })
	for i, r := range results {
		if r.Status != first.Status || !bytes.Equal(r.Body, first.Body) || r.Header.Get("Idempotent-Replayed") != "true" {
			t.Fatalf("replay %d = %s, want the stored %s", i, r, first)
		}
	}
	// A different request under the same key is still rejected on the fast path.
	wantError(t, e.do("POST", "/v1/payments", key, paymentBody("fp_ok", 999)), 409, "idempotency_key_reused")
}
