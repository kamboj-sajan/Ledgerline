// Package fraud is the rules engine run at authorization. It returns allow,
// review or block, plus the rules that fired.
//
// Every rule is a pure function in two halves. A signal is computed from the
// transaction and the card's history (Signals), and a threshold is compared
// against it (Decide). Both halves are pure, with no clock and no database,
// so the API (with history loaded from Postgres) and the offline evaluation
// harness (cmd/fraudeval, with history built from a dataset) run exactly the
// same code.
//
// Amounts are int64 minor units. Distances and ratios are statistics, not
// money, and use float64.
package fraud

import (
	"fmt"
	"math"
	"slices"
	"time"
)

// Decision is the engine's verdict.
type Decision string

// Decisions, in increasing severity.
const (
	Allow  Decision = "allow"
	Review Decision = "review" // let it through, flag it for a human
	Block  Decision = "block"  // fail the payment with fraud_suspected
)

func (d Decision) severity() int {
	switch d {
	case Review:
		return 1
	case Block:
		return 2
	default:
		return 0
	}
}

// Rule names.
const (
	RuleVelocityCount   = "velocity_count"
	RuleVelocityAmount  = "velocity_amount"
	RuleAmountAbsolute  = "amount_absolute"
	RuleAmountRelative  = "amount_relative"
	RuleCountryMismatch = "geo_country_mismatch"
	RuleDistance        = "geo_distance"
)

// Transaction is what the rules see of a payment.
type Transaction struct {
	ID              string
	CardFingerprint string
	Amount          int64 // minor units
	Currency        string
	Time            time.Time

	// Country signal, from the API: card issuing country and the client IP's country.
	CardCountry, IPCountry string

	// Location signal, from datasets that have coordinates (Sparkov):
	// cardholder home and merchant location.
	HasLocation                              bool
	HolderLat, HolderLon, MerchLat, MerchLon float64
}

// Signals are the measured facts the thresholds are compared against.
type Signals struct {
	// Velocity: earlier transactions of this card within the velocity
	// window, plus this one.
	VelocityCount  int
	VelocityAmount int64 // same currency only
	// AmountRatio is amount / median(earlier amounts in the history window),
	// or 0 when the card has fewer than MinHistory earlier transactions.
	AmountRatio  float64
	HistoryCount int
	// Geography.
	CountryMismatch bool
	DistanceKm      float64
	HasDistance     bool
}

// Signals computes the signals for tx from history, which may contain any
// earlier transactions of the same card in any order. Entries at or after
// tx.Time, or older than the history window, are ignored, so callers may pass
// a superset.
func (c Config) Signals(tx Transaction, history []Transaction) Signals {
	s := Signals{VelocityCount: 1, VelocityAmount: tx.Amount}
	velocityFrom := tx.Time.Add(-c.Velocity.Window.Duration)
	historyFrom := tx.Time.Add(-c.HistoryWindow.Duration)
	var amounts []int64
	for _, h := range history {
		if !h.Time.Before(tx.Time) || h.Time.Before(historyFrom) || h.CardFingerprint != tx.CardFingerprint {
			continue
		}
		if h.Currency == tx.Currency {
			amounts = append(amounts, h.Amount)
		}
		if !h.Time.Before(velocityFrom) {
			s.VelocityCount++
			if h.Currency == tx.Currency {
				s.VelocityAmount += h.Amount
			}
		}
	}
	s.HistoryCount = len(amounts)
	if len(amounts) >= max(1, c.AmountRelative.MinHistory) {
		if med := median(amounts); med > 0 {
			s.AmountRatio = float64(tx.Amount) / med
		}
	}
	s.CountryMismatch = tx.CardCountry != "" && tx.IPCountry != "" && tx.CardCountry != tx.IPCountry
	if tx.HasLocation {
		s.DistanceKm, s.HasDistance = haversineKm(tx.HolderLat, tx.HolderLon, tx.MerchLat, tx.MerchLon), true
	}
	return s
}

// Hit is one rule that fired.
type Hit struct {
	Rule   string   `json:"rule"`
	Action Decision `json:"action"`
	Detail string   `json:"detail"`
}

// Result is the engine's output.
type Result struct {
	Decision Decision `json:"decision"`
	Hits     []Hit    `json:"rules"`
}

// Rules returns the names of the rules that fired.
func (r Result) Rules() []string {
	out := make([]string, len(r.Hits))
	for i, h := range r.Hits {
		out[i] = h.Rule
	}
	return out
}

// Fires reports whether one rule fires on the given signals. Decide is the
// sum of these; the evaluation harness calls it per rule.
func (c Config) Fires(rule string, tx Transaction, s Signals) (bool, string) {
	switch rule {
	case RuleVelocityCount:
		v := c.Velocity
		return v.MaxCount > 0 && s.VelocityCount > v.MaxCount,
			fmt.Sprintf("%d transactions within %s (limit %d)", s.VelocityCount, v.Window.Duration, v.MaxCount)
	case RuleVelocityAmount:
		limit, ok := c.Velocity.MaxAmount[tx.Currency]
		return ok && limit > 0 && s.VelocityAmount > limit,
			fmt.Sprintf("%d %s within %s (limit %d)", s.VelocityAmount, tx.Currency, c.Velocity.Window.Duration, limit)
	case RuleAmountAbsolute:
		limit, ok := c.AmountAbsolute.Max[tx.Currency]
		return ok && limit > 0 && tx.Amount > limit, fmt.Sprintf("amount %d %s over limit %d", tx.Amount, tx.Currency, limit)
	case RuleAmountRelative:
		k := c.AmountRelative.MaxMultiple
		return k > 0 && s.AmountRatio > k,
			fmt.Sprintf("amount is %.1f× the card's median over %d earlier transactions (limit %.1f×)", s.AmountRatio, s.HistoryCount, k)
	case RuleCountryMismatch:
		return c.CountryMismatch.Enabled && s.CountryMismatch, fmt.Sprintf("card country %s, IP country %s", tx.CardCountry, tx.IPCountry)
	case RuleDistance:
		return c.Distance.MaxKm > 0 && s.HasDistance && s.DistanceKm > c.Distance.MaxKm,
			fmt.Sprintf("merchant %.0f km from cardholder (limit %.0f km)", s.DistanceKm, c.Distance.MaxKm)
	}
	return false, ""
}

// AllRules lists every rule in a fixed order.
var AllRules = []string{RuleVelocityCount, RuleVelocityAmount, RuleAmountAbsolute, RuleAmountRelative, RuleCountryMismatch, RuleDistance}

// Decide applies every enabled rule to the signals. The decision is the most
// severe action among the rules that fired.
func (c Config) Decide(tx Transaction, s Signals) Result {
	res := Result{Decision: Allow}
	for _, rule := range AllRules {
		if fired, detail := c.Fires(rule, tx, s); fired {
			action := c.action(rule)
			res.Hits = append(res.Hits, Hit{Rule: rule, Action: action, Detail: detail})
			if action.severity() > res.Decision.severity() {
				res.Decision = action
			}
		}
	}
	return res
}

// Evaluate is Signals followed by Decide.
func (c Config) Evaluate(tx Transaction, history []Transaction) Result {
	return c.Decide(tx, c.Signals(tx, history))
}

func median(xs []int64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return float64(s[n/2])
	}
	return (float64(s[n/2-1]) + float64(s[n/2])) / 2
}

// haversineKm is the great-circle distance between two points on a sphere
// with the Earth's mean radius.
func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusKm = 6371.0
	rad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusKm * math.Asin(math.Min(1, math.Sqrt(a)))
}
