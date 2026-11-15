// Package eval replays labelled transaction datasets through the fraud
// engine and measures it: confusion matrices, precision, recall and F1 per
// rule and combined, and threshold sweeps. Thresholds are chosen on a tuning
// split (the earlier data) and reported on a held-out split (the later data).
//
// Signals come from fraud.Config.Signals and threshold decisions from
// fraud.Config.Fires, the same functions the API runs at authorization.
package eval

import (
	"cmp"
	"slices"
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/fraud"
)

// Example is one labelled transaction with its signals. Sigs[i] uses
// velocity window Windows[i]; every other signal is the same in each.
type Example struct {
	Tx    fraud.Transaction
	Sigs  []fraud.Signals
	Fraud bool
}

// Confusion is a confusion matrix: positive means "flagged" (review or block).
type Confusion struct {
	TP, FP, FN, TN int
}

func (c Confusion) total() int { return c.TP + c.FP + c.FN + c.TN }

// Precision is TP / (TP + FP): of what we flagged, how much was fraud.
func (c Confusion) Precision() float64 { return ratio(c.TP, c.TP+c.FP) }

// Recall is TP / (TP + FN): of the fraud, how much we flagged.
func (c Confusion) Recall() float64 { return ratio(c.TP, c.TP+c.FN) }

// F1 is the harmonic mean of precision and recall.
func (c Confusion) F1() float64 {
	p, r := c.Precision(), c.Recall()
	if p+r == 0 {
		return 0
	}
	return 2 * p * r / (p + r)
}

// FlagRate is the share of all transactions flagged.
func (c Confusion) FlagRate() float64 { return ratio(c.TP+c.FP, c.total()) }

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// Score counts how flag classifies examples.
func Score(examples []Example, flag func(Example) bool) Confusion {
	var c Confusion
	for _, e := range examples {
		switch f := flag(e); {
		case f && e.Fraud:
			c.TP++
		case f && !e.Fraud:
			c.FP++
		case !f && e.Fraud:
			c.FN++
		default:
			c.TN++
		}
	}
	return c
}

// Point is one threshold of a sweep.
type Point struct {
	Threshold  float64
	Tune, Test Confusion
}

// Sweep scores every candidate threshold on both splits. best is the index
// with the highest F1 on the tuning split (ties go to higher precision). The
// test split plays no part in choosing it.
func Sweep(tune, test []Example, candidates []float64, firesAt func(threshold float64, e Example) bool) (points []Point, best int) {
	for _, th := range candidates {
		flag := func(e Example) bool { return firesAt(th, e) }
		points = append(points, Point{Threshold: th, Tune: Score(tune, flag), Test: Score(test, flag)})
	}
	for i, p := range points {
		b := points[best]
		if p.Tune.F1() > b.Tune.F1() || (p.Tune.F1() == b.Tune.F1() && p.Tune.Precision() > b.Tune.Precision()) {
			best = i
		}
	}
	return points, best
}

// Quantiles returns the q-quantiles (0 < q < 1) of values, deduplicated and
// sorted. They make candidate thresholds that follow the data's distribution.
func Quantiles(values []float64, qs []float64) []float64 {
	if len(values) == 0 {
		return nil
	}
	s := slices.Clone(values)
	slices.Sort(s)
	var out []float64
	for _, q := range qs {
		out = append(out, s[min(len(s)-1, int(q*float64(len(s))))])
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// History keeps each card's transactions within a time window, in arrival
// order, for computing signals as a dataset is replayed in time order. It is
// what the API's RecentByCard query returns, built incrementally.
type History struct {
	window time.Duration
	byCard map[string][]fraud.Transaction
}

// NewHistory returns an empty History.
func NewHistory(window time.Duration) *History {
	return &History{window: window, byCard: map[string][]fraud.Transaction{}}
}

// Before returns the card's transactions in (tx.Time - window, tx.Time),
// dropping older ones for good. Transactions must be fed in time order.
func (h *History) Before(tx fraud.Transaction) []fraud.Transaction {
	list := h.byCard[tx.CardFingerprint]
	from := tx.Time.Add(-h.window)
	i, _ := slices.BinarySearchFunc(list, from, func(t fraud.Transaction, at time.Time) int { return t.Time.Compare(at) })
	list = list[i:]
	h.byCard[tx.CardFingerprint] = list
	return list
}

// Add records tx (after its signals were computed).
func (h *History) Add(tx fraud.Transaction) {
	h.byCard[tx.CardFingerprint] = append(h.byCard[tx.CardFingerprint], tx)
}

// SortByTime orders examples by transaction time, keeping file order for ties.
func SortByTime(xs []Example) {
	slices.SortStableFunc(xs, func(a, b Example) int { return cmp.Compare(a.Tx.Time.UnixNano(), b.Tx.Time.UnixNano()) })
}
