//go:build integration

package integration

import (
	"bytes"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/money"
	"github.com/kamboj-sajan/ledgerline/internal/payments"
	"github.com/kamboj-sajan/ledgerline/internal/recon"
)

// manifestClasses is the report the manifest implies (see the unit test of
// the same name in internal/recon).
func manifestClasses(captures []recon.Capture, m recon.Manifest) map[string][]recon.Class {
	want := map[string][]recon.Class{}
	for _, c := range captures {
		want[c.PaymentID] = []recon.Class{recon.Matched}
	}
	for _, f := range m.Faults {
		want[f.Reference] = map[recon.Fault][]recon.Class{
			recon.FaultMissingRow:       {recon.MissingInBank},
			recon.FaultWrongAmount:      {recon.AmountMismatch},
			recon.FaultWrongCurrency:    {recon.CurrencyMismatch},
			recon.FaultDuplicateRow:     {recon.Duplicate, recon.Matched},
			recon.FaultLate:             {recon.Late},
			recon.FaultUnknownReference: {recon.MissingInLedger},
		}[f.Fault]
	}
	return want
}

func reportClasses(r recon.Report) map[string][]recon.Class {
	out := map[string][]recon.Class{}
	for _, it := range r.Items {
		out[it.Reference] = append(out[it.Reference], it.Class)
	}
	for k := range out {
		slices.Sort(out[k])
	}
	return out
}

// TestReconcileAgainstLedger: real captures through the API, the bank file
// from banksim, reconciliation and posting, then a second run. For several
// seeds, each with its own database.
func TestReconcileAgainstLedger(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3, 4, 5} {
		t.Run("seed "+itoa(int64(seed)), func(t *testing.T) {
			e := newAPIEnv(t)
			for i := range 30 {
				pid := e.paymentIn(payments.StatusAuthorized)
				wantStatus(t, e.post("/v1/payments/"+pid+"/capture", `{"amount": `+itoa(int64(5_000+i*37))+`}`), 200)
				if i%4 == 0 { // refunds are not settlement lines; they must not disturb matching
					wantStatus(t, e.post("/v1/payments/"+pid+"/refunds", `{"amount": 100}`), 201)
				}
			}
			ctx := t.Context()
			var first time.Time
			if err := e.db.QueryRow(ctx, `SELECT min(created_at) FROM journal_transactions WHERE kind = 'capture'`).Scan(&first); err != nil {
				t.Fatal(err)
			}
			day := first.UTC().Truncate(24 * time.Hour)
			captures, err := recon.DayCaptures(ctx, e.db, day)
			if err != nil {
				t.Fatal(err)
			}
			if len(captures) == 0 {
				t.Fatal("no captures found for the day")
			}

			rows, man := recon.Simulate(day, captures, recon.SimConfig{Seed: seed, FaultRate: 0.4, UnknownRows: 2, FeeBasisPoint: 20})
			var buf bytes.Buffer
			if err := recon.WriteCSV(&buf, rows); err != nil {
				t.Fatal(err)
			}
			parsed, err := recon.ReadCSV(&buf)
			if err != nil {
				t.Fatal(err)
			}

			rep := recon.Reconcile(day, captures, parsed, 3)
			if want, got := manifestClasses(captures, man), reportClasses(rep); !maps.EqualFunc(want, got, func(a, b []recon.Class) bool {
				a = slices.Clone(a)
				slices.Sort(a)
				return slices.Equal(a, b)
			}) {
				t.Fatalf("report %v\ndoes not match manifest %v", got, want)
			}
			if err := recon.Post(ctx, e.db, &rep); err != nil {
				t.Fatal(err)
			}

			// What should have been posted: one settlement per matched or late item.
			var wantPosted int
			var wantCash, wantFees int64
			for _, it := range rep.Items {
				if it.Class.Posts() {
					wantPosted++
					wantCash += *it.BankAmount - *it.BankFee
					wantFees += *it.BankFee
				}
			}
			if rep.Posted != wantPosted || rep.AlreadyPosted != 0 {
				t.Fatalf("posted %d (already %d), want %d", rep.Posted, rep.AlreadyPosted, wantPosted)
			}
			settlements := e.count(`SELECT count(*) FROM journal_transactions WHERE kind = 'settlement'`)
			if settlements != wantPosted {
				t.Fatalf("%d settlement transactions in the ledger, want %d", settlements, wantPosted)
			}
			cash, err := ledger.BalanceOf(ctx, e.db, ledger.Bank(money.USD))
			if err != nil {
				t.Fatal(err)
			}
			fees, err := ledger.BalanceOf(ctx, e.db, ledger.NetworkFees(money.USD))
			if err != nil {
				t.Fatal(err)
			}
			if cash.Amount() != wantCash || fees.Amount() != wantFees {
				t.Fatalf("bank_cash %d network fees %d, want %d and %d", cash.Amount(), fees.Amount(), wantCash, wantFees)
			}
			e.mustAudit()

			// Second run of the same file: it classifies the same and posts nothing.
			again := recon.Reconcile(day, captures, parsed, 3)
			if err := recon.Post(ctx, e.db, &again); err != nil {
				t.Fatal(err)
			}
			if again.Posted != 0 || again.AlreadyPosted != wantPosted {
				t.Fatalf("second run posted %d (already %d), want 0 (%d)", again.Posted, again.AlreadyPosted, wantPosted)
			}
			if n := e.count(`SELECT count(*) FROM journal_transactions WHERE kind = 'settlement'`); n != settlements {
				t.Fatalf("settlements %d → %d after a second run", settlements, n)
			}
			e.mustAudit()
			t.Logf("captures %d, rows %d, faults %d, posted %d, counts %v", len(captures), len(rows), len(man.Faults), rep.Posted, rep.Counts)
		})
	}
}
