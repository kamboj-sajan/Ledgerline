//go:build integration

package integration

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/payments"
)

func insertRawPayment(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	seedMerchant(t, db, "m1")
	mustExec(t, db, `INSERT INTO payments (id, merchant_id, status, currency, amount, customer_id, card_fingerprint, card_country, ip_country)
		VALUES ('p1', 'm1', 'created', 'USD', 10000, 'cus_1', 'fp_1', 'US', 'US')`)
}

// TestPaymentsSchemaChecks drives the payments CHECK constraints with raw
// SQL: amounts must nest (refunded ≤ captured ≤ authorized ≤ amount), and
// each status implies facts about the amounts.
func TestPaymentsSchemaChecks(t *testing.T) {
	tests := []struct {
		name           string
		update         string
		wantConstraint string // "" = must succeed
	}{
		{"created → authorized", `UPDATE payments SET status = 'authorized', authorized_amount = 10000`, ""},
		{"authorized amount on a created payment", `UPDATE payments SET authorized_amount = 10000`, "payments_status_amounts"},
		{"authorized more than requested", `UPDATE payments SET status = 'authorized', authorized_amount = 10001`, "payments_amounts"},
		{"captured more than authorized", `UPDATE payments SET status = 'captured', authorized_amount = 100, captured_amount = 200`, "payments_amounts"},
		{"refunded more than captured", `UPDATE payments SET status = 'captured', authorized_amount = 100, captured_amount = 100, refunded_amount = 101`, "payments_amounts"},
		{"fee more than captured", `UPDATE payments SET status = 'captured', authorized_amount = 100, captured_amount = 100, fee_amount = 101`, "payments_amounts"},
		{"captured status with nothing captured", `UPDATE payments SET status = 'captured', authorized_amount = 100`, "payments_status_amounts"},
		{"captured status but fully refunded", `UPDATE payments SET status = 'captured', authorized_amount = 100, captured_amount = 100, refunded_amount = 100`, "payments_status_amounts"},
		{"refunded status but partly refunded", `UPDATE payments SET status = 'refunded', authorized_amount = 100, captured_amount = 100, refunded_amount = 50`, "payments_status_amounts"},
		{"failed without a failure code", `UPDATE payments SET status = 'failed'`, "payments_status_amounts"},
		{"failed after capture", `UPDATE payments SET status = 'failed', failure_code = 'x', authorized_amount = 100, captured_amount = 100`, "payments_status_amounts"},
		// 'pending' breaks both the column CHECK and payments_status_amounts;
		// Postgres reports the first by name, so either name is correct.
		{"unknown status", `UPDATE payments SET status = 'pending'`, "payments_status_amounts|payments_status_check"},
		{"amount above the cap", `UPDATE payments SET amount = 1000000000000`, "payments_amount_check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := pg.NewPool(t)
			insertRawPayment(t, db)
			_, err := db.Exec(t.Context(), tt.update)
			got := constraintOf(err)
			if !slices.Contains(strings.Split(tt.wantConstraint, "|"), got) || (got == "" && tt.wantConstraint != "") {
				t.Fatalf("constraint = %q, want %q (err: %v)", got, tt.wantConstraint, err)
			}
		})
	}
}

func TestRefundsAreAppendOnly(t *testing.T) {
	db := pg.NewPool(t)
	insertRawPayment(t, db)
	mustExec(t, db, `UPDATE payments SET status = 'captured', authorized_amount = 10000, captured_amount = 10000`)
	mustExec(t, db, `INSERT INTO refunds (id, payment_id, amount, currency) VALUES ('re_1', 'p1', 100, 'USD')`)
	for _, sql := range []string{`UPDATE refunds SET amount = 1`, `DELETE FROM refunds`, `TRUNCATE refunds`} {
		if _, err := db.Exec(t.Context(), sql); constraintOf(err) != "refunds_append_only" {
			t.Errorf("%s: err = %v, want refunds_append_only", sql, err)
		}
	}
}

// capturePayment runs a full authorize + capture (+ optional refund) through
// the domain and store functions in one transaction, the way the API does.
func capturePayment(t *testing.T, db *pgxpool.Pool, id string, amount, refund int64) payments.Payment {
	t.Helper()
	ctx := t.Context()
	p, err := payments.New(id, "m1", usd(amount))
	if err != nil {
		t.Fatal(err)
	}
	p.CustomerID, p.Card, p.IPCountry = "cus_1", payments.Card{Fingerprint: "fp_1", Country: "US"}, "US"
	p.Metadata = map[string]string{"order": "42"}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := payments.Insert(ctx, tx, &p); err != nil {
		t.Fatal(err)
	}
	locked, err := payments.GetForUpdate(ctx, tx, "m1", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := locked.MarkAuthorized("auth_" + id); err != nil {
		t.Fatal(err)
	}
	lt, err := locked.Capture(usd(amount), payments.Pricing{BasisPoints: 290, Fixed: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Post(ctx, tx, lt); err != nil {
		t.Fatal(err)
	}
	if refund > 0 {
		rt, err := locked.Refund(usd(refund))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.Post(ctx, tx, rt); err != nil {
			t.Fatal(err)
		}
		if err := payments.InsertRefund(ctx, tx, &payments.Refund{ID: "re_" + id, PaymentID: id, Amount: usd(refund)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := payments.Update(ctx, tx, &locked); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return locked
}

func TestPaymentStoreRoundTrip(t *testing.T) {
	db := pg.NewPool(t)
	seedMerchant(t, db, "m1")
	seedMerchant(t, db, "m2")
	want := capturePayment(t, db, "pay_1", 10_000, 2_500)

	got, err := payments.Get(t.Context(), db, "m1", "pay_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != payments.StatusCaptured || got.Captured != usd(10_000) || got.Refunded != usd(2_500) ||
		got.Fee != usd(320) || got.Authorized != usd(10_000) || got.AcquirerReference != "auth_pay_1" ||
		got.Metadata["order"] != "42" || got.Card != want.Card || got.UpdatedAt.IsZero() {
		t.Fatalf("read back %+v", got)
	}
	if _, err := payments.Get(t.Context(), db, "m2", "pay_1"); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("another merchant's payment: error = %v, want ErrNotFound", err)
	}
	if _, err := payments.Get(t.Context(), db, "m1", "pay_missing"); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("missing payment: error = %v, want ErrNotFound", err)
	}
}

func TestPaymentsAuditCrossChecksLedger(t *testing.T) {
	db := pg.NewPool(t)
	seedMerchant(t, db, "m1")
	capturePayment(t, db, "pay_1", 10_000, 2_500)
	capturePayment(t, db, "pay_2", 5_000, 0)

	r, err := payments.Audit(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK() || r.Payments != 2 {
		t.Fatalf("audit of a consistent database = %+v", r)
	}

	// Each corruption is something the payments CHECKs allow but which
	// contradicts the ledger.
	mustExec(t, db, `UPDATE payments SET captured_amount = 9000, authorized_amount = 10000 WHERE id = 'pay_1'`)
	mustExec(t, db, `UPDATE payments SET refunded_amount = 100 WHERE id = 'pay_2'`)
	orphan, _ := ledger.Capture("pay_ghost", "m1", usd(100), usd(0))
	mustPost(t, db, orphan, nil)

	r, err = payments.Audit(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK() {
		t.Fatal("audit passed on a database where payments contradict the ledger")
	}
	if r.LedgerMismatchCount != 2 { // pay_1 captured amount, pay_2 refunded amount
		t.Errorf("ledger mismatches = %d %v, want 2", r.LedgerMismatchCount, r.LedgerMismatches)
	}
	if r.RefundMismatchCount != 1 || r.RefundMismatches[0] != "pay_2" {
		t.Errorf("refund mismatches = %d %v, want [pay_2]", r.RefundMismatchCount, r.RefundMismatches)
	}
	if r.OrphanTransactionsCount != 1 {
		t.Errorf("orphan transactions = %d, want 1", r.OrphanTransactionsCount)
	}
}
