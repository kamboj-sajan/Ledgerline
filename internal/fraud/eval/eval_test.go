package eval_test

import (
	"math"
	"testing"
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/fraud"
	"github.com/kamboj-sajan/ledgerline/internal/fraud/eval"
)

func TestConfusionMetrics(t *testing.T) {
	c := eval.Confusion{TP: 8, FP: 2, FN: 8, TN: 982}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	// precision 8/10, recall 8/16, F1 = 2·0.8·0.5/1.3
	if !near(c.Precision(), 0.8) || !near(c.Recall(), 0.5) || !near(c.F1(), 0.8/1.3) || !near(c.FlagRate(), 0.01) {
		t.Fatalf("metrics = %v %v %v %v", c.Precision(), c.Recall(), c.F1(), c.FlagRate())
	}
	if z := (eval.Confusion{TN: 5}); z.Precision() != 0 || z.Recall() != 0 || z.F1() != 0 {
		t.Fatal("empty classes must give 0, not NaN")
	}
}

func ex(amount int64, isFraud bool) eval.Example {
	return eval.Example{Tx: fraud.Transaction{Amount: amount}, Fraud: isFraud}
}

// TestSweepChoosesOnTuneOnly: the threshold that is best on the test split
// must not be picked unless it is also best on the tuning split.
func TestSweepChoosesOnTuneOnly(t *testing.T) {
	tune := []eval.Example{ex(10, false), ex(20, false), ex(30, true), ex(40, true)}
	test := []eval.Example{ex(10, false), ex(15, true), ex(30, true), ex(40, true)}
	fires := func(th float64, e eval.Example) bool { return float64(e.Tx.Amount) > th }
	points, best := eval.Sweep(tune, test, []float64{5, 12, 25}, fires)
	if points[best].Threshold != 25 { // perfect on tune
		t.Fatalf("chose %v, want 25 (best on tune)", points[best].Threshold)
	}
	if points[1].Test.F1() <= points[best].Test.F1() {
		t.Fatal("test setup: threshold 12 should be better on test, to prove it was not chosen for that")
	}
}

func TestQuantiles(t *testing.T) {
	got := eval.Quantiles([]float64{5, 1, 4, 2, 3, 3}, []float64{0.5, 0.9, 0.99})
	if len(got) != 2 || got[0] != 3 || got[1] != 5 { // duplicates removed
		t.Fatalf("Quantiles = %v", got)
	}
}

func TestHistoryWindow(t *testing.T) {
	h := eval.NewHistory(time.Hour)
	at := func(m int) fraud.Transaction {
		return fraud.Transaction{CardFingerprint: "c", Time: time.Unix(0, 0).Add(time.Duration(m) * time.Minute)}
	}
	h.Add(at(0))
	h.Add(at(30))
	if n := len(h.Before(at(59))); n != 2 {
		t.Fatalf("at 59 min: %d in history, want 2", n)
	}
	if n := len(h.Before(at(61))); n != 1 { // the one at 0 is now older than an hour
		t.Fatalf("at 61 min: %d in history, want 1", n)
	}
	if n := len(h.Before(fraud.Transaction{CardFingerprint: "other", Time: at(61).Time})); n != 0 {
		t.Fatalf("other card has %d", n)
	}
}

func TestLoadULB(t *testing.T) {
	xs, err := eval.LoadULB("testdata/ulb.csv")
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 4 {
		t.Fatalf("%d rows", len(xs))
	}
	// Sorted by time; amounts parsed exactly into cents.
	want := []struct {
		amount int64
		fraud  bool
	}{{14_962, false}, {269, false}, {37_866, false}, {0, true}}
	for i, w := range want {
		if xs[i].Tx.Amount != w.amount || xs[i].Fraud != w.fraud || xs[i].Tx.Currency != "EUR" {
			t.Errorf("row %d = %+v, want %+v", i, xs[i], w)
		}
	}
}

func TestLoadSparkovUsesTrainHistoryForTest(t *testing.T) {
	var base fraud.Config
	base.HistoryWindow = fraud.Duration{Duration: 30 * 24 * time.Hour}
	base.AmountRelative.MinHistory = 2
	train, test, err := eval.LoadSparkov("testdata/sparkov_train.csv", "testdata/sparkov_test.csv", base, []time.Duration{time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(train) != 3 || len(test) != 1 {
		t.Fatalf("%d train, %d test", len(train), len(test))
	}
	got := test[0]
	// Card 111 had two train transactions in the previous 30 minutes.
	if got.Sigs[0].VelocityCount != 3 || got.Sigs[0].VelocityAmount != 3_497 || !got.Fraud {
		t.Fatalf("test signals = %+v", got.Sigs[0])
	}
	if got.Sigs[0].AmountRatio < 2.6 || got.Sigs[0].AmountRatio > 2.7 { // 2000 / median(497, 1000)
		t.Fatalf("ratio = %v", got.Sigs[0].AmountRatio)
	}
	if !train[1].Sigs[0].HasDistance || train[1].Sigs[0].DistanceKm < 55 || train[1].Sigs[0].DistanceKm > 56 { // 0.5° of latitude
		t.Fatalf("distance = %v", train[1].Sigs[0].DistanceKm)
	}
}
