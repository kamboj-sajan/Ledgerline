package recon_test

import (
	"bytes"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/kamboj-sajan/ledgerline/internal/money"
	"github.com/kamboj-sajan/ledgerline/internal/recon"
)

var day = time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)

// expected turns the manifest into the exact report the reconciler must
// produce: reference → the multiset of classes its items must have.
func expected(captures []recon.Capture, m recon.Manifest) map[string][]recon.Class {
	want := map[string][]recon.Class{}
	for _, c := range captures {
		want[c.PaymentID] = []recon.Class{recon.Matched}
	}
	for _, f := range m.Faults {
		switch f.Fault {
		case recon.FaultMissingRow:
			want[f.Reference] = []recon.Class{recon.MissingInBank}
		case recon.FaultWrongAmount:
			want[f.Reference] = []recon.Class{recon.AmountMismatch}
		case recon.FaultWrongCurrency:
			want[f.Reference] = []recon.Class{recon.CurrencyMismatch}
		case recon.FaultDuplicateRow:
			want[f.Reference] = []recon.Class{recon.Duplicate, recon.Matched}
		case recon.FaultLate:
			want[f.Reference] = []recon.Class{recon.Late}
		case recon.FaultUnknownReference:
			want[f.Reference] = []recon.Class{recon.MissingInLedger}
		}
	}
	return want
}

func got(r recon.Report) map[string][]recon.Class {
	out := map[string][]recon.Class{}
	for _, it := range r.Items {
		out[it.Reference] = append(out[it.Reference], it.Class)
	}
	for k := range out {
		slices.Sort(out[k])
	}
	return out
}

func randomCaptures(rng *rand.Rand, n int) []recon.Capture {
	currencies := []money.Currency{money.USD, money.EUR, money.JPY, money.BHD}
	out := make([]recon.Capture, n)
	for i := range out {
		out[i] = recon.Capture{
			PaymentID:  fmt.Sprintf("pay_%04d", i),
			Amount:     money.New(int64(1+rng.IntN(1_000_000)), currencies[rng.IntN(len(currencies))]),
			CapturedAt: day.Add(time.Duration(rng.IntN(86_400)) * time.Second),
		}
	}
	return out
}

// TestReconcileMatchesManifestAcrossSeeds is the acceptance test: for every
// seed, the report finds every injected fault with the right class and
// reports nothing that was not injected. The file goes through CSV and back,
// so parsing is covered too.
func TestReconcileMatchesManifestAcrossSeeds(t *testing.T) {
	const seeds = 2000
	faults := map[recon.Fault]int{}
	for seed := uint64(1); seed <= seeds; seed++ {
		rng := rand.New(rand.NewPCG(seed, 99))
		captures := randomCaptures(rng, rng.IntN(80))
		cfg := recon.SimConfig{Seed: seed, FaultRate: rng.Float64(), UnknownRows: rng.IntN(6), FeeBasisPoint: 20}
		rows, man := recon.Simulate(day, captures, cfg)

		var buf bytes.Buffer
		if err := recon.WriteCSV(&buf, rows); err != nil {
			t.Fatal(err)
		}
		parsed, err := recon.ReadCSV(&buf)
		if err != nil {
			t.Fatalf("seed %d: reading back: %v", seed, err)
		}
		rep := recon.Reconcile(day, captures, parsed, 3)

		want, have := expected(captures, man), got(rep)
		for k := range want {
			slices.Sort(want[k])
		}
		if !maps.EqualFunc(want, have, slices.Equal) {
			for ref, w := range want {
				if !slices.Equal(w, have[ref]) {
					t.Fatalf("seed %d: %s classified %v, manifest says %v", seed, ref, have[ref], w)
				}
			}
			for ref, h := range have {
				if _, ok := want[ref]; !ok {
					t.Fatalf("seed %d: unexpected %s %v (false positive)", seed, ref, h)
				}
			}
		}
		for _, f := range man.Faults {
			faults[f.Fault]++
		}
	}
	t.Logf("%d seeds, injected faults: %v", seeds, faults)
	for _, f := range []recon.Fault{recon.FaultMissingRow, recon.FaultWrongAmount, recon.FaultWrongCurrency, recon.FaultDuplicateRow, recon.FaultUnknownReference, recon.FaultLate} {
		if faults[f] == 0 {
			t.Errorf("fault %s was never injected; the test did not cover it", f)
		}
	}
}

func TestPropReconcileIndependentOfRowOrder(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		seed := rapid.Uint64().Draw(rt, "seed")
		rng := rand.New(rand.NewPCG(seed, 7))
		captures := randomCaptures(rng, rapid.IntRange(0, 30).Draw(rt, "captures"))
		rows, _ := recon.Simulate(day, captures, recon.SimConfig{Seed: seed, FaultRate: 0.5, UnknownRows: 2, FeeBasisPoint: 20})
		a := recon.Reconcile(day, captures, rows, 3)
		shuffled := rapid.Permutation(rows).Draw(rt, "shuffled")
		b := recon.Reconcile(day, captures, shuffled, 3)
		if !maps.Equal(a.Counts, b.Counts) || !maps.EqualFunc(got(a), got(b), slices.Equal) {
			rt.Fatalf("row order changed the result: %v vs %v", a.Counts, b.Counts)
		}
	})
}

func TestReconcileWindow(t *testing.T) {
	c := recon.Capture{PaymentID: "pay_1", Amount: money.New(1000, money.USD)}
	row := func(at time.Time) []recon.Row {
		return []recon.Row{{Reference: "pay_1", Amount: c.Amount, Fee: money.New(2, money.USD), SettledAt: at}}
	}
	for _, tc := range []struct {
		at   time.Time
		want []recon.Class
	}{
		{day.Add(15 * time.Hour), []recon.Class{recon.Matched}},                            // same day
		{day.AddDate(0, 0, 1).Add(23 * time.Hour), []recon.Class{recon.Matched}},           // T+1, last hour
		{day.AddDate(0, 0, 2), []recon.Class{recon.Late}},                                  // T+2
		{day.AddDate(0, 0, 3).Add(23 * time.Hour), []recon.Class{recon.Late}},              // T+3, the window's last day
		{day.AddDate(0, 0, 4), []recon.Class{recon.MissingInBank, recon.MissingInLedger}},  // outside the window
		{day.Add(-time.Second), []recon.Class{recon.MissingInBank, recon.MissingInLedger}}, // before the capture day
	} {
		rep := recon.Reconcile(day, []recon.Capture{c}, row(tc.at), 3)
		if g := got(rep)["pay_1"]; !slices.Equal(g, tc.want) {
			t.Errorf("settled %s: %v, want %v", tc.at, g, tc.want)
		}
	}
}

func TestReadCSVRejectsBadRows(t *testing.T) {
	hdr := "reference,amount,currency,fee,settled_at\n"
	for name, line := range map[string]string{
		"float-ish amount":    "pay_1,10.005,USD,0.02,2026-11-15T09:00:00Z",
		"unknown currency":    "pay_1,10.00,XXX,0.02,2026-11-15T09:00:00Z",
		"lower-case currency": "pay_1,10.00,usd,0.02,2026-11-15T09:00:00Z",
		"bad time":            "pay_1,10.00,USD,0.02,15/11/2026",
		"empty reference":     ",10.00,USD,0.02,2026-11-15T09:00:00Z",
		"missing column":      "pay_1,10.00,USD,2026-11-15T09:00:00Z",
	} {
		if _, err := recon.ReadCSV(strings.NewReader(hdr + line + "\n")); err == nil {
			t.Errorf("%s: accepted %q", name, line)
		}
	}
	if _, err := recon.ReadCSV(strings.NewReader("ref,amount,currency,fee,settled_at\n")); err == nil {
		t.Error("accepted a wrong header")
	}
	rows, err := recon.ReadCSV(strings.NewReader(hdr + "pay_1,1234,JPY,2,2026-11-15T09:00:00Z\n"))
	if err != nil || rows[0].Amount != money.New(1234, money.JPY) {
		t.Fatalf("JPY row = %+v, %v", rows, err)
	}
}
