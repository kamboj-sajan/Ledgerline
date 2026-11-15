package recon

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/money"
)

// Capture is a capture the ledger expects to be settled.
type Capture struct {
	PaymentID  string
	Amount     money.Money
	CapturedAt time.Time
}

// Fault is a defect the bank simulator injects.
type Fault string

// Faults.
const (
	FaultMissingRow       Fault = "missing_row"       // the capture has no row
	FaultWrongAmount      Fault = "wrong_amount"      // the row's amount is off
	FaultWrongCurrency    Fault = "wrong_currency"    // the row is in another currency
	FaultDuplicateRow     Fault = "duplicate_row"     // the row appears twice
	FaultUnknownReference Fault = "unknown_reference" // a row for no capture at all
	FaultLate             Fault = "late"              // settled a day late (T+2)
)

// captureFaults are the faults applied to existing captures.
var captureFaults = []Fault{FaultMissingRow, FaultWrongAmount, FaultWrongCurrency, FaultDuplicateRow, FaultLate}

// Injected is one fault in the manifest.
type Injected struct {
	Reference string `json:"reference"`
	Fault     Fault  `json:"fault"`
}

// Manifest is the bank simulator's ground truth: every fault it injected.
type Manifest struct {
	Day    string     `json:"day"`
	Seed   uint64     `json:"seed"`
	Faults []Injected `json:"faults"`
}

// SimConfig controls the simulator.
type SimConfig struct {
	Seed          uint64
	FaultRate     float64 // probability that a capture gets one fault
	UnknownRows   int     // rows for references the ledger has never seen
	FeeBasisPoint int64   // the network's fee on each settled amount
}

// Simulate writes the settlement file the bank would send for day's
// captures, with seeded faults, and the manifest of what it injected. Each
// capture gets at most one fault. Normally a capture settles on day+1 (T+1).
func Simulate(day time.Time, captures []Capture, cfg SimConfig) ([]Row, Manifest) {
	rng := rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x5bd1e995)) //nolint:gosec // simulation
	man := Manifest{Day: day.Format(time.DateOnly), Seed: cfg.Seed}
	onTime := day.AddDate(0, 0, 1).Add(9 * time.Hour)
	fee := func(m money.Money) money.Money {
		f, _ := m.MulBasisPoints(cfg.FeeBasisPoint) // captured amounts are far below the overflow point
		return f
	}

	var rows []Row
	for _, c := range captures {
		row := Row{Reference: c.PaymentID, Amount: c.Amount, Fee: fee(c.Amount), SettledAt: onTime}
		if rng.Float64() >= cfg.FaultRate {
			rows = append(rows, row)
			continue
		}
		f := captureFaults[rng.IntN(len(captureFaults))]
		man.Faults = append(man.Faults, Injected{Reference: c.PaymentID, Fault: f})
		switch f {
		case FaultMissingRow:
			continue
		case FaultWrongAmount:
			delta := int64(1 + rng.IntN(500))
			if rng.IntN(2) == 0 && c.Amount.Amount() > delta {
				delta = -delta
			}
			row.Amount = money.New(c.Amount.Amount()+delta, c.Amount.Currency())
			row.Fee = fee(row.Amount)
		case FaultWrongCurrency:
			other := money.EUR
			if c.Amount.Currency() == money.EUR {
				other = money.USD
			}
			row.Amount = money.New(c.Amount.Amount(), other)
			row.Fee = money.New(row.Fee.Amount(), other)
		case FaultDuplicateRow:
			rows = append(rows, row)
		case FaultLate:
			row.SettledAt = onTime.AddDate(0, 0, 1)
		}
		rows = append(rows, row)
	}
	for i := range cfg.UnknownRows {
		ref := "pay_UNKNOWN" + strconv.FormatUint(cfg.Seed, 10) + "x" + strconv.Itoa(i)
		amt := money.New(int64(100+rng.IntN(100_000)), money.USD)
		rows = append(rows, Row{Reference: ref, Amount: amt, Fee: fee(amt), SettledAt: onTime})
		man.Faults = append(man.Faults, Injected{Reference: ref, Fault: FaultUnknownReference})
	}
	// Banks do not sort their files the way we would.
	rng.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
	slices.SortStableFunc(man.Faults, func(a, b Injected) int {
		switch {
		case a.Reference < b.Reference:
			return -1
		case a.Reference > b.Reference:
			return 1
		}
		return 0
	})
	return rows, man
}
