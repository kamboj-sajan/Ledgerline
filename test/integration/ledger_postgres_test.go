//go:build integration

package integration

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/money"
)

func usd(a int64) money.Money { return money.New(a, money.USD) }

func seedMerchant(t *testing.T, db *pgxpool.Pool, id string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO merchants (id, name, api_key_hash) VALUES ($1, $1, sha256(convert_to($1, 'UTF8')))`, id)
}

// post writes one journal transaction in its own database transaction.
func post(t *testing.T, db *pgxpool.Pool, lt ledger.Tx) (string, error) {
	t.Helper()
	ctx := t.Context()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, err := ledger.Post(ctx, tx, lt)
	if err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

func mustPost(t *testing.T, db *pgxpool.Pool, lt ledger.Tx, err error) string {
	t.Helper()
	if err != nil {
		t.Fatalf("building transaction: %v", err)
	}
	id, err := post(t, db, lt)
	if err != nil {
		t.Fatalf("posting %s: %v", lt.Kind, err)
	}
	return id
}

func wantBalance(t *testing.T, db *pgxpool.Pool, k ledger.AccountKey, want int64) {
	t.Helper()
	got, err := ledger.BalanceOf(t.Context(), db, k)
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount() != want {
		t.Errorf("balance of %s = %d, want %d", k, got.Amount(), want)
	}
}

func TestLedgerPostAndReadBack(t *testing.T) {
	db := pg.NewPool(t)
	seedMerchant(t, db, "m1")
	ctx := t.Context()

	capture, err := ledger.Capture("pay_1", "m1", usd(10_000), usd(320))
	captureID := mustPost(t, db, capture, err)

	stored, err := ledger.LoadTransaction(ctx, db, captureID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Kind != capture.Kind || stored.PaymentID != "pay_1" || !slices.Equal(stored.Postings, capture.Postings) {
		t.Fatalf("read back %+v\nposted     %+v", stored.Tx, capture)
	}

	wantBalance(t, db, ledger.Receivable(money.USD), 10_000)
	wantBalance(t, db, ledger.Payable("m1", money.USD), -9_680)
	wantBalance(t, db, ledger.Fees(money.USD), -320)
	wantBalance(t, db, ledger.Bank(money.USD), 0) // never used: zero, not an error

	refund, err := ledger.Refund("pay_1", "m1", usd(2_500))
	mustPost(t, db, refund, err)
	wantBalance(t, db, ledger.Payable("m1", money.USD), -7_180)
	wantBalance(t, db, ledger.Receivable(money.USD), 7_500)

	// Balance by account ID, converted to the account's normal sign.
	acct, err := ledger.EnsureAccount(ctx, db, ledger.Payable("m1", money.USD))
	if err != nil {
		t.Fatal(err)
	}
	gotAcct, signed, err := ledger.Balance(ctx, db, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	normal, _ := ledger.NormalBalance(gotAcct.Key.Type(), signed)
	if gotAcct.Key != ledger.Payable("m1", money.USD) || normal != usd(7_180) {
		t.Fatalf("Balance(%s) = %v %v (normal %v), want m1 payable 71.80 USD", acct.ID, gotAcct.Key, signed, normal)
	}
	if _, _, err := ledger.Balance(ctx, db, "acct_missing"); !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Fatalf("Balance of unknown account: error = %v", err)
	}

	txs, err := ledger.PaymentTransactions(ctx, db, "pay_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 2 || txs[0].Kind != ledger.KindCapture || txs[1].Kind != ledger.KindRefund {
		t.Fatalf("PaymentTransactions = %d txs %v", len(txs), txs)
	}
}

func TestLedgerPostMapsSchemaRulesToErrors(t *testing.T) {
	db := pg.NewPool(t)
	seedMerchant(t, db, "m1")

	capture, err := ledger.Capture("pay_1", "m1", usd(10_000), usd(320))
	captureID := mustPost(t, db, capture, err)

	settle, err := ledger.Settlement("pay_1", "bank-ref-1", usd(10_000), usd(0))
	mustPost(t, db, settle, err)
	if _, err := post(t, db, settle); !errors.Is(err, ledger.ErrDuplicateExternalRef) {
		t.Fatalf("second settlement: error = %v, want ErrDuplicateExternalRef", err)
	}

	rev, err := ledger.Reverse(captureID, capture)
	mustPost(t, db, rev, err)
	if _, err := post(t, db, rev); !errors.Is(err, ledger.ErrAlreadyReversed) {
		t.Fatalf("second reversal: error = %v, want ErrAlreadyReversed", err)
	}
	ghost, _ := ledger.Reverse("jtx_missing", capture)
	if _, err := post(t, db, ghost); !errors.Is(err, ledger.ErrUnknownTransaction) {
		t.Fatalf("reversal of a missing transaction: error = %v, want ErrUnknownTransaction", err)
	}

	if n := rowCount(t, db, "journal_transactions"); n != 3 {
		t.Fatalf("journal_transactions = %d, want 3 (capture, settlement, reversal)", n)
	}
	// The reversal undid the capture; the settlement then left the receivable
	// at -100.00 and cash at +100.00.
	wantBalance(t, db, ledger.Payable("m1", money.USD), 0)
	wantBalance(t, db, ledger.Fees(money.USD), 0)
	wantBalance(t, db, ledger.Receivable(money.USD), -10_000)
	wantBalance(t, db, ledger.Bank(money.USD), 10_000)
}

func TestLedgerPostValidatesBeforeWriting(t *testing.T) {
	db := pg.NewPool(t)
	bad := ledger.Tx{Kind: ledger.KindCapture, Postings: []ledger.Posting{
		{Account: ledger.Receivable(money.USD), Amount: usd(100)},
	}}
	if _, err := post(t, db, bad); !errors.Is(err, ledger.ErrTooFewPostings) {
		t.Fatalf("error = %v, want ErrTooFewPostings", err)
	}
	if n := rowCount(t, db, "journal_transactions"); n != 0 {
		t.Fatalf("%d journal transactions written for an invalid Tx", n)
	}
}

func TestLedgerPostRollsBackWithCallerTransaction(t *testing.T) {
	db := pg.NewPool(t)
	seedMerchant(t, db, "m1")
	ctx := t.Context()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	capture, _ := ledger.Capture("pay_1", "m1", usd(100), usd(0))
	if _, err := ledger.Post(ctx, tx, capture); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"journal_transactions", "entries", "accounts"} {
		if n := rowCount(t, db, table); n != 0 {
			t.Errorf("%s has %d rows after rollback", table, n)
		}
	}
}

// TestLedgerEnsureAccountConcurrent races many transactions to create the
// same account. Exactly one row may result, and everyone must get its ID. The
// platform key has a NULL merchant_id, which only conflicts because of
// UNIQUE NULLS NOT DISTINCT.
func TestLedgerEnsureAccountConcurrent(t *testing.T) {
	db := pg.NewPool(t)
	seedMerchant(t, db, "m1")
	const workers = 50

	for _, key := range []ledger.AccountKey{ledger.Receivable(money.USD), ledger.Payable("m1", money.USD)} {
		t.Run(key.String(), func(t *testing.T) {
			ctx := t.Context()
			ids := make([]string, workers)
			errs := make([]error, workers)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range workers {
				wg.Go(func() {
					<-start // release every goroutine at once for maximum contention
					tx, err := db.Begin(ctx)
					if err != nil {
						errs[i] = err
						return
					}
					defer func() { _ = tx.Rollback(ctx) }()
					acct, err := ledger.EnsureAccount(ctx, tx, key)
					if err != nil {
						errs[i] = err
						return
					}
					ids[i], errs[i] = acct.ID, tx.Commit(ctx)
				})
			}
			close(start)
			wg.Wait()

			for i, err := range errs {
				if err != nil {
					t.Fatalf("worker %d: %v", i, err)
				}
				if ids[i] != ids[0] {
					t.Fatalf("worker %d got account %s, worker 0 got %s", i, ids[i], ids[0])
				}
			}
			var n int
			if err := db.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE code = $1 AND currency = 'USD'`, string(key.Code)).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("%d %s accounts exist, want 1", n, key.Code)
			}
		})
	}
}

func TestAuditPassesOnHealthyLedger(t *testing.T) {
	db := pg.NewPool(t)
	seedMerchant(t, db, "m1")
	capture, err := ledger.Capture("pay_1", "m1", usd(10_000), usd(320))
	mustPost(t, db, capture, err)
	refund, err := ledger.Refund("pay_1", "m1", usd(1_000))
	mustPost(t, db, refund, err)
	jpy, err := ledger.Capture("pay_2", "m1", money.New(5_000, money.JPY), money.New(150, money.JPY))
	mustPost(t, db, jpy, err)

	r, err := ledger.Audit(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK() || r.Transactions != 3 || r.Entries != 8 {
		t.Fatalf("audit = %+v, want OK with 3 transactions and 8 entries", r)
	}
	want := []ledger.CurrencyTotal{{Currency: "JPY", Total: "0"}, {Currency: "USD", Total: "0"}}
	if !slices.Equal(r.CurrencyTotals, want) {
		t.Fatalf("currency totals = %v, want %v", r.CurrencyTotals, want)
	}
}

// TestAuditDetectsCorruptionThatBypassedTriggers writes damage the schema
// would normally reject, using session_replication_role = replica (which turns
// off triggers, and with them foreign keys) as a superuser could. The audit
// must find every problem. This proves the audit checks the data itself and
// does not pass merely because the triggers usually prevent bad writes.
func TestAuditDetectsCorruptionThatBypassedTriggers(t *testing.T) {
	db := pg.NewPool(t)
	seedLedgerAccounts(t, db)
	ctx := t.Context()

	conn, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, s := range []string{
		`SET session_replication_role = replica`,
		`BEGIN`,
		// one entry, unbalanced
		`INSERT INTO journal_transactions (id, kind) VALUES ('bad1', 'capture')`,
		`INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES ('bad1', 'recv_usd', 100, 'USD')`,
		// no entries
		`INSERT INTO journal_transactions (id, kind) VALUES ('empty', 'capture')`,
		// two entries, but one is in EUR on a USD account (so neither currency balances)
		`INSERT INTO journal_transactions (id, kind) VALUES ('bad2', 'capture')`,
		`INSERT INTO entries (transaction_id, account_id, amount, currency) VALUES ('bad2', 'recv_usd', 5, 'USD'), ('bad2', 'pay_usd', -5, 'EUR')`,
		`COMMIT`,
		`SET session_replication_role = origin`,
	} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	r, err := ledger.Audit(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK() {
		t.Fatalf("audit passed on a corrupted ledger: %+v", r)
	}
	if !slices.Equal(r.Unbalanced, []string{"bad1", "bad2"}) || r.UnbalancedCount != 2 {
		t.Errorf("unbalanced = %d %v, want [bad1 bad2]", r.UnbalancedCount, r.Unbalanced)
	}
	if !slices.Equal(r.TooFewEntries, []string{"bad1", "empty"}) || r.TooFewEntriesCount != 2 {
		t.Errorf("too few entries = %d %v, want [bad1 empty]", r.TooFewEntriesCount, r.TooFewEntries)
	}
	if r.CurrencyMismatchCount != 1 {
		t.Errorf("currency mismatches = %d, want 1", r.CurrencyMismatchCount)
	}
	want := []ledger.CurrencyTotal{{Currency: "EUR", Total: "-5"}, {Currency: "USD", Total: "105"}}
	if !slices.Equal(r.CurrencyTotals, want) {
		t.Errorf("currency totals = %v, want %v", r.CurrencyTotals, want)
	}
}
