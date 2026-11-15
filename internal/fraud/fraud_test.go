package fraud_test

import (
	"math"
	"slices"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/kamboj-sajan/ledgerline/internal/fraud"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func tx(minutesAgo int, amount int64) fraud.Transaction {
	return fraud.Transaction{CardFingerprint: "card", Amount: amount, Currency: "USD", Time: t0.Add(-time.Duration(minutesAgo) * time.Minute)}
}

func cfg() fraud.Config {
	var c fraud.Config
	c.HistoryWindow = fraud.Duration{Duration: 30 * 24 * time.Hour}
	c.Velocity.Window = fraud.Duration{Duration: time.Hour}
	c.Velocity.MaxCount = 3
	c.Velocity.MaxAmount = map[string]int64{"USD": 10_000}
	c.AmountAbsolute.Max = map[string]int64{"USD": 50_000}
	c.AmountAbsolute.Action = fraud.Block
	c.AmountRelative.MaxMultiple = 5
	c.AmountRelative.MinHistory = 3
	c.CountryMismatch.Enabled = true
	c.Distance.MaxKm = 1000
	return c
}

func TestSignals(t *testing.T) {
	c := cfg()
	now := tx(0, 2_000)
	history := []fraud.Transaction{
		tx(10, 1_000), tx(59, 1_000), // inside the 1h velocity window
		tx(61, 1_000), // outside velocity, inside history
		{CardFingerprint: "card", Amount: 9_999, Currency: "EUR", Time: t0.Add(-5 * time.Minute)},  // other currency
		{CardFingerprint: "other", Amount: 9_999, Currency: "USD", Time: t0.Add(-5 * time.Minute)}, // other card
		tx(-5, 9_999),       // after this transaction: ignored
		tx(31*24*60, 9_999), // older than the history window: ignored
	}
	s := c.Signals(now, history)
	if s.VelocityCount != 4 { // 3 earlier in window (2 USD + 1 EUR) + this one
		t.Errorf("VelocityCount = %d, want 4", s.VelocityCount)
	}
	if s.VelocityAmount != 4_000 { // USD only: 1000 + 1000 + 2000
		t.Errorf("VelocityAmount = %d, want 4000", s.VelocityAmount)
	}
	if s.HistoryCount != 3 || s.AmountRatio != 2 { // median of [1000 1000 1000]
		t.Errorf("HistoryCount %d AmountRatio %v, want 3 and 2", s.HistoryCount, s.AmountRatio)
	}

	// Too little history: no ratio.
	if s := c.Signals(now, []fraud.Transaction{tx(10, 1_000), tx(20, 1_000)}); s.AmountRatio != 0 {
		t.Errorf("ratio with 2 earlier transactions = %v, want 0 (min_history is 3)", s.AmountRatio)
	}
	// Even-sized history uses the mean of the middle two.
	if s := c.Signals(now, []fraud.Transaction{tx(1, 100), tx(2, 300), tx(3, 500), tx(4, 700)}); s.AmountRatio != 5 {
		t.Errorf("ratio = %v, want 2000/400 = 5", s.AmountRatio)
	}
}

func TestRulesFire(t *testing.T) {
	c := cfg()
	burst := []fraud.Transaction{tx(1, 100), tx(2, 100), tx(3, 100)}
	steady := []fraud.Transaction{tx(2000, 1_000), tx(3000, 1_000), tx(4000, 1_000)}
	far := fraud.Transaction{CardFingerprint: "card", Amount: 500, Currency: "USD", Time: t0, HasLocation: true,
		HolderLat: 51.5074, HolderLon: -0.1278, MerchLat: 40.7128, MerchLon: -74.0060} // London → New York

	tests := []struct {
		name     string
		tx       fraud.Transaction
		history  []fraud.Transaction
		wantHits []string
		want     fraud.Decision
	}{
		{"quiet card", tx(0, 1_000), steady, nil, fraud.Allow},
		{"fourth transaction in an hour", tx(0, 100), burst, []string{fraud.RuleVelocityCount}, fraud.Review},
		{"hourly spend over 100.00", tx(0, 9_801), []fraud.Transaction{tx(1, 100), tx(2, 100)}, []string{fraud.RuleVelocityAmount}, fraud.Review}, // only 2 earlier: too few for amount_relative
		{"absolute amount blocks", tx(0, 50_001), nil, []string{fraud.RuleVelocityAmount, fraud.RuleAmountAbsolute}, fraud.Block},
		{"6x the usual amount", tx(0, 6_000), steady, []string{fraud.RuleAmountRelative}, fraud.Review},
		{"country mismatch", fraud.Transaction{CardFingerprint: "card", Amount: 100, Currency: "USD", Time: t0, CardCountry: "US", IPCountry: "RU"},
			nil, []string{fraud.RuleCountryMismatch}, fraud.Review},
		{"merchant 5,570 km from home", far, nil, []string{fraud.RuleDistance}, fraud.Review},
		{"currency without thresholds", fraud.Transaction{CardFingerprint: "card", Amount: 99_999_999, Currency: "JPY", Time: t0}, nil, nil, fraud.Allow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := c.Evaluate(tt.tx, tt.history)
			if r.Decision != tt.want || !slices.Equal(r.Rules(), tt.wantHits) {
				t.Fatalf("Evaluate = %s %v, want %s %v (%+v)", r.Decision, r.Rules(), tt.want, tt.wantHits, r.Hits)
			}
			for _, h := range r.Hits {
				if h.Detail == "" {
					t.Errorf("hit %s has no detail", h.Rule)
				}
			}
		})
	}
}

func TestZeroConfigAllowsEverything(t *testing.T) {
	var c fraud.Config
	extreme := fraud.Transaction{CardFingerprint: "card", Amount: math.MaxInt32, Currency: "USD", Time: t0, CardCountry: "US", IPCountry: "RU"}
	if r := c.Evaluate(extreme, []fraud.Transaction{tx(1, 1), tx(2, 1), tx(3, 1)}); r.Decision != fraud.Allow || len(r.Hits) != 0 {
		t.Fatalf("zero config = %+v", r)
	}
}

func TestHaversine(t *testing.T) {
	c := cfg()
	for _, tc := range []struct {
		name                   string
		lat1, lon1, lat2, lon2 float64
		wantKm                 float64
	}{
		// Reference distances on a 6371 km sphere, to the nearest 10 km.
		{"London–Paris", 51.5074, -0.1278, 48.8566, 2.3522, 340},
		{"New York–Los Angeles", 40.7128, -74.0060, 34.0522, -118.2437, 3940},
		{"same point", 10, 10, 10, 10, 0},
	} {
		s := c.Signals(fraud.Transaction{HasLocation: true, HolderLat: tc.lat1, HolderLon: tc.lon1, MerchLat: tc.lat2, MerchLon: tc.lon2, Time: t0}, nil)
		if math.Abs(s.DistanceKm-tc.wantKm) > 10 {
			t.Errorf("%s: %.0f km, want ≈ %.0f", tc.name, s.DistanceKm, tc.wantKm)
		}
	}
}

func TestConfigParsing(t *testing.T) {
	d := fraud.Default()
	if d.Velocity.MaxCount == 0 || d.AmountAbsolute.Max["USD"] == 0 || !d.CountryMismatch.Enabled {
		t.Fatalf("default config looks empty: %+v", d)
	}
	for _, bad := range []string{
		`{"velocity": {"window": "1h"}, "history_window": "10m"}`, // velocity longer than history
		`{"velocity": {"action": "ignore"}}`,                      // unknown action
		`{"velocity": {"window": 3600}}`,                          // duration must be a string
		`{"surprise": true}`,                                      // unknown field
	} {
		if _, err := fraud.Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%s) accepted an invalid config", bad)
		}
	}
}

func historyGen() *rapid.Generator[[]fraud.Transaction] {
	return rapid.SliceOfN(rapid.Custom(func(t *rapid.T) fraud.Transaction {
		return tx(rapid.IntRange(1, 3000).Draw(t, "minutes ago"), rapid.Int64Range(1, 100_000).Draw(t, "amount"))
	}), 0, 30)
}

// TestPropOrderDoesNotMatter: history may arrive in any order (the API reads
// it from SQL, the harness from a sliding window).
func TestPropOrderDoesNotMatter(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		c := cfg()
		h := historyGen().Draw(rt, "history")
		now := tx(0, rapid.Int64Range(1, 100_000).Draw(rt, "amount"))
		want := c.Evaluate(now, h)
		shuffled := rapid.Permutation(h).Draw(rt, "shuffled")
		if got := c.Evaluate(now, shuffled); got.Decision != want.Decision || !slices.Equal(got.Rules(), want.Rules()) {
			rt.Fatalf("order changed the result: %+v vs %+v", got, want)
		}
	})
}

// TestPropLooserThresholdsNeverAddHits: raising every threshold can only
// remove hits, which is what makes a threshold sweep meaningful.
func TestPropLooserThresholdsNeverAddHits(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		strict := cfg()
		loose := cfg()
		loose.Velocity.MaxCount += rapid.IntRange(0, 5).Draw(rt, "count slack")
		loose.Velocity.MaxAmount = map[string]int64{"USD": strict.Velocity.MaxAmount["USD"] + rapid.Int64Range(0, 50_000).Draw(rt, "amount slack")}
		loose.AmountRelative.MaxMultiple += rapid.Float64Range(0, 5).Draw(rt, "multiple slack")
		h := historyGen().Draw(rt, "history")
		now := tx(0, rapid.Int64Range(1, 100_000).Draw(rt, "amount"))
		strictHits, looseHits := strict.Evaluate(now, h).Rules(), loose.Evaluate(now, h).Rules()
		for _, r := range looseHits {
			if !slices.Contains(strictHits, r) {
				rt.Fatalf("rule %s fires with looser thresholds but not stricter ones", r)
			}
		}
	})
}
