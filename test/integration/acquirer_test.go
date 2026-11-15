//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kamboj-sajan/ledgerline/internal/acquirer"
)

func authRequest(key, fingerprint string, amount int64) acquirer.Request {
	return acquirer.Request{IdempotencyKey: key, PaymentReference: key, Amount: usd(amount), CardFingerprint: fingerprint, CardCountry: "US"}
}

func acquirerCalls(t *testing.T, db *pgxpool.Pool, key string) map[string]int {
	t.Helper()
	rows, err := db.Query(t.Context(), `SELECT outcome, count(*) FROM acquirer.calls WHERE idempotency_key = $1 GROUP BY outcome`, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var o string
		var n int
		if err := rows.Scan(&o, &n); err != nil {
			t.Fatal(err)
		}
		out[o] = n
	}
	return out
}

func authorizationsFor(t *testing.T, db *pgxpool.Pool, ref string) int {
	t.Helper()
	n, err := acquirer.Authorizations(t.Context(), db, ref)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAcquirerIsIdempotentOnKey(t *testing.T) {
	db := pg.NewPool(t)
	sim := acquirer.NewSimulator(db, acquirer.Config{Seed: 1})
	ctx := t.Context()

	first, err := sim.Authorize(ctx, authRequest("pay_1", "fp_ok", 1_000))
	if err != nil || !first.Approved || first.Reference == "" {
		t.Fatalf("first call = %+v, %v", first, err)
	}
	again, err := sim.Authorize(ctx, authRequest("pay_1", "fp_ok", 1_000))
	if err != nil || again != first {
		t.Fatalf("replay = %+v, %v; want %+v", again, err, first)
	}
	if n := authorizationsFor(t, db, "pay_1"); n != 1 {
		t.Fatalf("authorizations = %d, want 1", n)
	}
	if c := acquirerCalls(t, db, "pay_1"); c["approved"] != 1 || c["replayed"] != 1 {
		t.Fatalf("calls = %v", c)
	}
	if _, err := sim.Authorize(ctx, authRequest("pay_1", "fp_ok", 2_000)); !errors.Is(err, acquirer.ErrKeyReused) {
		t.Fatalf("same key, different amount: error = %v, want ErrKeyReused", err)
	}
}

func TestAcquirerConcurrentCallsWithOneKeyAuthorizeOnce(t *testing.T) {
	db := pg.NewPool(t)
	sim := acquirer.NewSimulator(db, acquirer.Config{Seed: 2})
	const callers = 20
	results := make([]acquirer.Result, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			<-start
			results[i], errs[i] = sim.Authorize(t.Context(), authRequest("pay_1", "fp_ok", 1_000))
		})
	}
	close(start)
	wg.Wait()
	for i := range callers {
		if errs[i] != nil || results[i] != results[0] {
			t.Fatalf("caller %d got %+v, %v; caller 0 got %+v", i, results[i], errs[i], results[0])
		}
	}
	if n := authorizationsFor(t, db, "pay_1"); n != 1 {
		t.Fatalf("authorizations = %d, want 1", n)
	}
}

func TestAcquirerDeclines(t *testing.T) {
	db := pg.NewPool(t)
	sim := acquirer.NewSimulator(db, acquirer.Config{Seed: 3})
	for _, tc := range []struct{ card, code string }{
		{acquirer.CardDecline, "card_declined"},
		{acquirer.CardInsufficientFunds, "insufficient_funds"},
	} {
		for range 2 { // the replay returns the same decline
			r, err := sim.Authorize(t.Context(), authRequest("pay_"+tc.card, tc.card, 1_000))
			if err != nil || r.Approved || r.DeclineCode != tc.code || r.DeclineMessage == "" {
				t.Fatalf("%s: %+v, %v", tc.card, r, err)
			}
		}
	}
	// DeclineRate 1 declines every ordinary card.
	all := acquirer.NewSimulator(db, acquirer.Config{DeclineRate: 1, Seed: 4})
	if r, err := all.Authorize(t.Context(), authRequest("pay_x", "fp_ok", 1_000)); err != nil || r.Approved || r.DeclineCode != "do_not_honor" {
		t.Fatalf("DeclineRate=1: %+v, %v", r, err)
	}
}

// TestAcquirerTimeoutIsAmbiguous is the case idempotency exists for: the
// network authorized, but the caller never heard back. Retrying with the same
// key reveals the authorization instead of creating a second one.
func TestAcquirerTimeoutIsAmbiguous(t *testing.T) {
	db := pg.NewPool(t)
	sim := acquirer.NewSimulator(db, acquirer.Config{Seed: 5})
	req := authRequest("pay_1", acquirer.CardTimeout, 1_000)
	if _, err := sim.Authorize(t.Context(), req); !errors.Is(err, acquirer.ErrTimeout) {
		t.Fatalf("first call error = %v, want ErrTimeout", err)
	}
	if n := authorizationsFor(t, db, "pay_1"); n != 1 {
		t.Fatalf("after the timeout, authorizations = %d, want 1 (it happened)", n)
	}
	r, err := sim.Authorize(t.Context(), req)
	if err != nil || !r.Approved {
		t.Fatalf("retry = %+v, %v; want the recorded approval", r, err)
	}
	if n := authorizationsFor(t, db, "pay_1"); n != 1 {
		t.Fatalf("after the retry, authorizations = %d, want 1", n)
	}
}

func TestAcquirerUnavailableRecordsNothing(t *testing.T) {
	db := pg.NewPool(t)
	sim := acquirer.NewSimulator(db, acquirer.Config{Seed: 6})
	if _, err := sim.Authorize(t.Context(), authRequest("pay_1", acquirer.CardUnavailable, 1_000)); !errors.Is(err, acquirer.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	flaky := acquirer.NewSimulator(db, acquirer.Config{ErrorRate: 1, Seed: 7})
	if _, err := flaky.Authorize(t.Context(), authRequest("pay_2", "fp_ok", 1_000)); !errors.Is(err, acquirer.ErrUnavailable) {
		t.Fatalf("ErrorRate=1: error = %v, want ErrUnavailable", err)
	}
	if n := rowCountIn(t, db, "acquirer.authorizations"); n != 0 {
		t.Fatalf("authorizations = %d, want 0", n)
	}
}

func TestAcquirerLatencyRespectsContext(t *testing.T) {
	db := pg.NewPool(t)
	sim := acquirer.NewSimulator(db, acquirer.Config{Latency: 2 * time.Second, Seed: 8})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := sim.Authorize(ctx, authRequest("pay_1", "fp_ok", 1_000))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("call took %s; it should stop when the context ends", elapsed)
	}
	if n := rowCountIn(t, db, "acquirer.authorizations"); n != 0 {
		t.Fatalf("authorizations = %d after a cancelled call, want 0", n)
	}
}

func rowCountIn(t *testing.T, db *pgxpool.Pool, qualified string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(t.Context(), "SELECT count(*) FROM "+qualified).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
