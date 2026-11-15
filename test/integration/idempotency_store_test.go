//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kamboj-sajan/ledgerline/internal/idempotency"
)

func idemRequest(t *testing.T, key, body string) idempotency.Request {
	t.Helper()
	h, err := idempotency.Fingerprint("POST", "/v1/payments", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return idempotency.Request{MerchantID: "m1", Key: key, Method: "POST", Path: "/v1/payments", Hash: h}
}

// withTx runs f in a transaction and commits if f returns nil.
func withTx(t *testing.T, db *pgxpool.Pool, f func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	ctx := t.Context()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := f(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func lookup(t *testing.T, db *pgxpool.Pool, r idempotency.Request, lockTimeout time.Duration) (idempotency.Record, error) {
	t.Helper()
	var rec idempotency.Record
	err := withTx(t, db, func(ctx context.Context, tx pgx.Tx) error {
		if err := idempotency.Lock(ctx, tx, r); err != nil {
			return err
		}
		var err error
		rec, err = idempotency.Lookup(ctx, tx, r, lockTimeout)
		return err
	})
	return rec, err
}

func TestIdempotencyStoreFinishedRecord(t *testing.T) {
	db := pg.NewPool(t)
	seedMerchant(t, db, "m1")
	r := idemRequest(t, "k1", `{"amount":100}`)

	if rec, err := lookup(t, db, r, time.Minute); err != nil || rec.State != idempotency.StateNew {
		t.Fatalf("fresh key: %+v, %v", rec, err)
	}
	if err := withTx(t, db, func(ctx context.Context, tx pgx.Tx) error {
		return idempotency.Save(ctx, tx, r, idempotency.Response{Status: 201, Body: []byte(`{"id":"pay_1"}`)})
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := lookup(t, db, r, time.Minute)
	if err != nil || rec.State != idempotency.StateFinished || rec.Response.Status != 201 || string(rec.Response.Body) != `{"id":"pay_1"}` {
		t.Fatalf("after save: %+v, %v", rec, err)
	}
	// The same key with a different body is rejected; formatting differences are not.
	if _, err := lookup(t, db, idemRequest(t, "k1", `{"amount":101}`), time.Minute); !errors.Is(err, idempotency.ErrReused) {
		t.Fatalf("different body: error = %v, want ErrReused", err)
	}
	if rec, err := lookup(t, db, idemRequest(t, "k1", `{ "amount" : 100 }`), time.Minute); err != nil || rec.State != idempotency.StateFinished {
		t.Fatalf("same body, other formatting: %+v, %v", rec, err)
	}
}

func TestIdempotencyStoreExpiry(t *testing.T) {
	db := pg.NewPool(t)
	seedMerchant(t, db, "m1")
	r := idemRequest(t, "k1", `{}`)
	if err := withTx(t, db, func(ctx context.Context, tx pgx.Tx) error {
		return idempotency.Save(ctx, tx, r, idempotency.Response{Status: 200, Body: []byte(`{}`)})
	}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE idempotency_keys SET expires_at = now() - interval '1 second'`)
	// An expired key is forgotten, even for a different request.
	if rec, err := lookup(t, db, idemRequest(t, "k1", `{"other":true}`), time.Minute); err != nil || rec.State != idempotency.StateNew {
		t.Fatalf("expired key: %+v, %v", rec, err)
	}

	mustExec(t, db, `INSERT INTO idempotency_keys (merchant_id, key, request_method, request_path, request_hash, recovery_point, response_status, response_body, expires_at)
		VALUES ('m1', 'old', 'POST', '/x', sha256('x'), 'finished', 200, '{}', now() - interval '1 hour'),
		       ('m1', 'live', 'POST', '/x', sha256('x'), 'finished', 200, '{}', now() + interval '1 hour')`)
	n, err := idempotency.Purge(t.Context(), db)
	if err != nil || n != 1 {
		t.Fatalf("Purge = %d, %v; want 1", n, err)
	}
}

func TestIdempotencyLockIsExclusiveAndReleasedWithTransaction(t *testing.T) {
	db := pg.NewPool(t)
	seedMerchant(t, db, "m1")
	r := idemRequest(t, "k1", `{}`)
	ctx := t.Context()

	holder, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := idempotency.Lock(ctx, holder, r); err != nil {
		t.Fatal(err)
	}
	if _, err := lookup(t, db, r, time.Minute); !errors.Is(err, idempotency.ErrInUse) {
		t.Fatalf("while held: error = %v, want ErrInUse", err)
	}
	if _, err := lookup(t, db, idemRequest(t, "k2", `{}`), time.Minute); err != nil {
		t.Fatalf("another key must not be blocked: %v", err)
	}
	// Rolling back (as a crashed or failed request would) releases the lock
	// and leaves no record, so the key is not used up.
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if rec, err := lookup(t, db, r, time.Minute); err != nil || rec.State != idempotency.StateNew {
		t.Fatalf("after rollback: %+v, %v", rec, err)
	}
}

// TestIdempotencyMultiPhase covers the authorize path: Start, a lock that
// keeps other requests out, takeover once it is stale, and the fencing token
// that stops the original owner from finishing after a takeover.
func TestIdempotencyMultiPhase(t *testing.T) {
	db := pg.NewPool(t)
	seedMerchant(t, db, "m1")
	r := idemRequest(t, "k1", `{}`)

	var tokenA string
	if err := withTx(t, db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		tokenA, err = idempotency.Start(ctx, tx, r, "pay_1")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := lookup(t, db, r, time.Minute); !errors.Is(err, idempotency.ErrInUse) {
		t.Fatalf("fresh lock: error = %v, want ErrInUse", err)
	}
	// With a zero lock timeout, the lock counts as stale: resume.
	rec, err := lookup(t, db, r, 0)
	if err != nil || rec.State != idempotency.StateStarted || rec.PaymentID != "pay_1" {
		t.Fatalf("stale lock: %+v, %v", rec, err)
	}

	// Request B takes over.
	var tokenB string
	if err := withTx(t, db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		tokenB, err = idempotency.Start(ctx, tx, r, "pay_1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	claim := func(token string) (idempotency.Record, error) {
		var rec idempotency.Record
		err := withTx(t, db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rec, err = idempotency.Claim(ctx, tx, r, token)
			return err
		})
		return rec, err
	}
	if _, err := claim(tokenA); !errors.Is(err, idempotency.ErrInUse) {
		t.Fatalf("old owner claiming after takeover: error = %v, want ErrInUse", err)
	}
	if rec, err := claim(tokenB); err != nil || rec.State != idempotency.StateStarted {
		t.Fatalf("new owner claiming: %+v, %v", rec, err)
	}
	// Unlock with the wrong token is a no-op; with the right one it frees the key.
	if err := idempotency.Unlock(t.Context(), db, r, tokenA); err != nil {
		t.Fatal(err)
	}
	if _, err := lookup(t, db, r, time.Minute); !errors.Is(err, idempotency.ErrInUse) {
		t.Fatalf("after wrong-token unlock: error = %v, want ErrInUse", err)
	}
	if err := idempotency.Unlock(t.Context(), db, r, tokenB); err != nil {
		t.Fatal(err)
	}
	if rec, err := lookup(t, db, r, time.Minute); err != nil || rec.State != idempotency.StateStarted {
		t.Fatalf("after unlock: %+v, %v", rec, err)
	}

	// B finishes. Now anyone claiming, even with an old token, sees the stored result.
	if err := withTx(t, db, func(ctx context.Context, tx pgx.Tx) error {
		return idempotency.Save(ctx, tx, r, idempotency.Response{Status: 200, Body: []byte(`{"status":"authorized"}`)})
	}); err != nil {
		t.Fatal(err)
	}
	if rec, err := claim(tokenA); err != nil || rec.State != idempotency.StateFinished || string(rec.Response.Body) != `{"status":"authorized"}` {
		t.Fatalf("claim after finish: %+v, %v", rec, err)
	}
}
