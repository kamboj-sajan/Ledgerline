//go:build integration

package integration

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kamboj-sajan/ledgerline/internal/acquirer"
	"github.com/kamboj-sajan/ledgerline/internal/api"
	"github.com/kamboj-sajan/ledgerline/internal/idempotency"
)

func (e *apiEnv) acquirerAuthorizations(paymentID string) int {
	e.t.Helper()
	n, err := acquirer.Authorizations(e.t.Context(), e.db, paymentID)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestAuthorizeDeclineIsStoredAndReplayed(t *testing.T) {
	e := newAPIEnv(t)
	pid := e.createPayment(acquirer.CardInsufficientFunds, 10_000).ID
	key := newKey()
	first := e.do("POST", "/v1/payments/"+pid+"/authorize", key, "")
	er := wantError(t, first, http.StatusPaymentRequired, "card_declined")
	if er.Error.Type != "card_error" || er.Error.DeclineCode != "insufficient_funds" {
		t.Fatalf("decline error = %+v", er.Error)
	}
	got := decode[paymentResp](t, e.do("GET", "/v1/payments/"+pid, "", ""))
	if got.Status != "failed" || got.FailureCode == nil || *got.FailureCode != "insufficient_funds" {
		t.Fatalf("payment after decline = %+v", got)
	}
	again := e.do("POST", "/v1/payments/"+pid+"/authorize", key, "")
	if again.Status != first.Status || !bytes.Equal(again.Body, first.Body) {
		t.Fatalf("replay = %s, want %s", again, first)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE payment_id = $1 AND type = 'payment.failed'`, pid); n != 1 {
		t.Fatalf("payment.failed events = %d, want 1", n)
	}
	if n := e.acquirerAuthorizations(pid); n != 1 {
		t.Fatalf("acquirer authorizations = %d, want 1", n)
	}
}

// TestAuthorizeTimeoutThenRetry: the network authorizes but our call times
// out. We do not know the outcome, so we answer 504 and store nothing. The
// retry with the same key asks the acquirer again under the same payment ID,
// learns about the existing authorization, and records it. One authorization
// at the network, one event.
func TestAuthorizeTimeoutThenRetry(t *testing.T) {
	e := newAPIEnv(t)
	pid := e.createPayment(acquirer.CardTimeout, 10_000).ID
	key := newKey()

	r := e.do("POST", "/v1/payments/"+pid+"/authorize", key, "")
	wantError(t, r, http.StatusGatewayTimeout, "acquirer_timeout")
	if r.Header.Get("Retry-After") == "" {
		t.Fatal("504 without Retry-After")
	}
	if s := decode[paymentResp](t, e.do("GET", "/v1/payments/"+pid, "", "")).Status; s != "created" {
		t.Fatalf("status after timeout = %s, want created", s)
	}
	if n := e.acquirerAuthorizations(pid); n != 1 {
		t.Fatalf("after timeout, acquirer authorizations = %d, want 1 (it did authorize)", n)
	}

	retry := e.do("POST", "/v1/payments/"+pid+"/authorize", key, "")
	wantStatus(t, retry, 200)
	if s := decode[paymentResp](t, retry).Status; s != "authorized" {
		t.Fatalf("status after retry = %s", s)
	}
	if n := e.acquirerAuthorizations(pid); n != 1 {
		t.Fatalf("after retry, acquirer authorizations = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE payment_id = $1 AND type = 'payment.authorized'`, pid); n != 1 {
		t.Fatalf("payment.authorized events = %d, want 1", n)
	}
	// A third call replays.
	if third := e.do("POST", "/v1/payments/"+pid+"/authorize", key, ""); !bytes.Equal(third.Body, retry.Body) {
		t.Fatalf("third call = %s, want replay of %s", third, retry)
	}
}

func TestAuthorizeAcquirerUnavailable(t *testing.T) {
	e := newAPIEnv(t)
	pid := e.createPayment(acquirer.CardUnavailable, 10_000).ID
	key := newKey()
	for range 2 { // not stored, so the retry executes again (and fails again)
		r := e.do("POST", "/v1/payments/"+pid+"/authorize", key, "")
		wantError(t, r, http.StatusServiceUnavailable, "acquirer_unavailable")
	}
	if n := e.acquirerAuthorizations(pid); n != 0 {
		t.Fatalf("acquirer authorizations = %d, want 0", n)
	}
	if s := decode[paymentResp](t, e.do("GET", "/v1/payments/"+pid, "", "")).Status; s != "created" {
		t.Fatalf("status = %s, want created", s)
	}
}

// blockingAcquirer wraps the simulator. The first call signals entered and
// then waits for release. Later calls go straight through.
type blockingAcquirer struct {
	sim     *acquirer.Simulator
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func newBlockingAcquirer(db *pgxpool.Pool) *blockingAcquirer {
	return &blockingAcquirer{
		sim:     acquirer.NewSimulator(db, acquirer.Config{Seed: 9}),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *blockingAcquirer) Authorize(ctx context.Context, req acquirer.Request) (acquirer.Result, error) {
	if b.calls.Add(1) == 1 {
		close(b.entered)
		select {
		case <-b.release:
		case <-ctx.Done():
			return acquirer.Result{}, ctx.Err()
		}
	}
	return b.sim.Authorize(ctx, req)
}

func TestAuthorizeKeyInUseWhileRunning(t *testing.T) {
	var acq *blockingAcquirer
	e := newAPIEnv(t, withAcquirer(func(db *pgxpool.Pool) acquirer.Acquirer { acq = newBlockingAcquirer(db); return acq }))
	pid := e.createPayment("fp_ok", 10_000).ID
	key := newKey()

	firstDone := make(chan response, 1)
	go func() { firstDone <- e.do("POST", "/v1/payments/"+pid+"/authorize", key, "") }()
	<-acq.entered // the first request is now waiting on the acquirer, between transactions

	r := e.do("POST", "/v1/payments/"+pid+"/authorize", key, "")
	wantError(t, r, http.StatusConflict, "idempotency_key_in_use")
	if r.Header.Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", r.Header.Get("Retry-After"))
	}

	close(acq.release)
	first := <-firstDone
	wantStatus(t, first, 200)
	if again := e.do("POST", "/v1/payments/"+pid+"/authorize", key, ""); !bytes.Equal(again.Body, first.Body) {
		t.Fatalf("after completion = %s, want replay", again)
	}
}

// TestAuthorizeStaleLockTakeover: the first request holds the key longer than
// the lock timeout (as a hung or crashed process would). A retry takes over and
// finishes. When the first request finally wakes up, its fencing token no
// longer matches, and it returns the stored result instead of writing its own.
func TestAuthorizeStaleLockTakeover(t *testing.T) {
	var acq *blockingAcquirer
	e := newAPIEnv(t,
		withConfig(api.Config{AuthorizeLockTimeout: 100 * time.Millisecond, AcquirerTimeout: 10 * time.Second}),
		withAcquirer(func(db *pgxpool.Pool) acquirer.Acquirer { acq = newBlockingAcquirer(db); return acq }))
	pid := e.createPayment("fp_ok", 10_000).ID
	key := newKey()

	firstDone := make(chan response, 1)
	go func() { firstDone <- e.do("POST", "/v1/payments/"+pid+"/authorize", key, "") }()
	<-acq.entered
	time.Sleep(300 * time.Millisecond) // let the first request's lock go stale

	second := e.do("POST", "/v1/payments/"+pid+"/authorize", key, "")
	wantStatus(t, second, 200)

	close(acq.release)
	first := <-firstDone
	if first.Status != second.Status || !bytes.Equal(first.Body, second.Body) {
		t.Fatalf("fenced-out first request returned %s, want the stored %s", first, second)
	}
	if n := e.acquirerAuthorizations(pid); n != 1 {
		t.Fatalf("acquirer authorizations = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE payment_id = $1 AND type = 'payment.authorized'`, pid); n != 1 {
		t.Fatalf("payment.authorized events = %d, want 1", n)
	}
	e.mustAudit()
}

// TestAuthorizeCrashAfterAcquirerApproved reproduces a crash between the
// acquirer approving and us recording it. Phase 1's 'started' key is written
// as the crashed process left it, and the acquirer is called directly. After
// the lock goes stale, the client's retry resumes and records the existing
// authorization.
func TestAuthorizeCrashAfterAcquirerApproved(t *testing.T) {
	var sim *acquirer.Simulator
	e := newAPIEnv(t,
		withConfig(api.Config{AuthorizeLockTimeout: 100 * time.Millisecond, AcquirerTimeout: 10 * time.Second}),
		withAcquirer(func(db *pgxpool.Pool) acquirer.Acquirer {
			sim = acquirer.NewSimulator(db, acquirer.Config{Seed: 10})
			return sim
		}))
	p := e.createPayment("fp_ok", 10_000)
	path := "/v1/payments/" + p.ID + "/authorize"
	key := newKey()

	hash, err := idempotency.Fingerprint("POST", path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := idempotency.Request{MerchantID: e.merchantID, Key: key, Method: "POST", Path: path, Hash: hash}
	if err := withTx(t, e.db, func(ctx context.Context, tx pgx.Tx) error {
		_, err := idempotency.Start(ctx, tx, req, p.ID) // phase 1 committed...
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sim.Authorize(t.Context(), acquirer.Request{ // ...phase 2 reached the network...
		IdempotencyKey: p.ID, PaymentReference: p.ID, Amount: usd(10_000), CardFingerprint: "fp_ok", CardCountry: "US",
	}); err != nil {
		t.Fatal(err)
	}
	// ...and the process died before phase 3.

	wantError(t, e.do("POST", path, key, ""), http.StatusConflict, "idempotency_key_in_use") // lock still fresh
	time.Sleep(200 * time.Millisecond)
	r := e.do("POST", path, key, "")
	wantStatus(t, r, 200)
	if s := decode[paymentResp](t, r).Status; s != "authorized" {
		t.Fatalf("status = %s", s)
	}
	if n := e.acquirerAuthorizations(p.ID); n != 1 {
		t.Fatalf("acquirer authorizations = %d, want 1", n)
	}
	e.mustAudit()
}

// TestAuthorizeConcurrentDifferentKeys: several clients authorize one payment
// with different keys. All of them reach the acquirer with the same payment
// ID, so the network authorizes once. Exactly one request records it; the
// rest find the payment already authorized.
func TestAuthorizeConcurrentDifferentKeys(t *testing.T) {
	e := newAPIEnv(t)
	pid := e.createPayment("fp_ok", 10_000).ID
	const clients = 10
	results := make([]response, clients)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range clients {
		wg.Go(func() {
			<-start
			results[i] = e.post("/v1/payments/"+pid+"/authorize", "")
		})
	}
	close(start)
	wg.Wait()

	ok, conflicts := 0, 0
	for _, r := range results {
		switch r.Status {
		case 200:
			ok++
		case 409:
			wantError(t, r, 409, "invalid_state_transition")
			conflicts++
		default:
			t.Fatalf("unexpected response %s", r)
		}
	}
	if ok != 1 || conflicts != clients-1 {
		t.Fatalf("%d successes and %d conflicts, want 1 and %d", ok, conflicts, clients-1)
	}
	if n := e.acquirerAuthorizations(pid); n != 1 {
		t.Fatalf("acquirer authorizations = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE payment_id = $1 AND type = 'payment.authorized'`, pid); n != 1 {
		t.Fatalf("payment.authorized events = %d, want 1", n)
	}
}
