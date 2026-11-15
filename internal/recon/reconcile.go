package recon

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// Class is how a row (or a capture with no row) is classified.
type Class string

// Classes.
const (
	Matched          Class = "matched"           // settled on time, amounts and currency agree; posted
	Late             Class = "late"              // agrees, settled after T+1 but within T+N; posted
	MissingInBank    Class = "missing_in_bank"   // a capture with no row
	MissingInLedger  Class = "missing_in_ledger" // a row for a reference with no capture that day
	AmountMismatch   Class = "amount_mismatch"   // row amount differs from the capture; not posted
	CurrencyMismatch Class = "currency_mismatch" // row currency differs; not posted
	Duplicate        Class = "duplicate"         // a second (or later) row for a reference; not posted
)

// Posts reports whether rows of this class are posted to the ledger.
func (c Class) Posts() bool { return c == Matched || c == Late }

// Item is one line of the report.
type Item struct {
	Reference string `json:"reference"`
	Class     Class  `json:"class"`
	// BankAmount/LedgerAmount are minor units, with their currencies; empty
	// when that side is missing.
	BankAmount     *int64 `json:"bank_amount,omitempty"`
	BankCurrency   string `json:"bank_currency,omitempty"`
	BankFee        *int64 `json:"bank_fee,omitempty"`
	LedgerAmount   *int64 `json:"ledger_amount,omitempty"`
	LedgerCurrency string `json:"ledger_currency,omitempty"`
	SettledAt      string `json:"settled_at,omitempty"`
	Detail         string `json:"detail,omitempty"`

	row *Row // for posting
}

// Report is the outcome of reconciling one day.
type Report struct {
	Day        string        `json:"day"`
	WindowDays int           `json:"window_days"`
	Counts     map[Class]int `json:"counts"`
	Items      []Item        `json:"items"`
	// Filled in by Post.
	Posted        int `json:"posted"`
	AlreadyPosted int `json:"already_posted"`
}

// Reconcile classifies the bank's rows against day's captures. It is pure
// (no clock, no database), so it can be checked against the simulator's
// manifest across thousands of seeds.
//
// A capture's expected settlement date is day+1 (T+1). A row settled later,
// but no later than day+windowDays, is late. A row outside [day, day+window]
// is not accepted as that capture's settlement: it is reported as
// missing_in_ledger with a detail, and the capture as missing_in_bank. For
// each reference, rows are taken in file order: the first is classified
// against the capture, any further ones are duplicate.
func Reconcile(day time.Time, captures []Capture, rows []Row, windowDays int) Report {
	byRef := make(map[string]Capture, len(captures))
	for _, c := range captures {
		byRef[c.PaymentID] = c
	}
	rep := Report{Day: day.Format(time.DateOnly), WindowDays: windowDays, Counts: map[Class]int{}}
	onTimeBy := day.AddDate(0, 0, 2) // before the start of day+2
	windowEnd := day.AddDate(0, 0, windowDays+1)
	settled := map[string]bool{} // references that got their first in-window row

	for i := range rows {
		r := &rows[i]
		amt, fee := r.Amount.Amount(), r.Fee.Amount()
		it := Item{Reference: r.Reference, BankAmount: &amt, BankCurrency: r.Amount.Currency().Code(), BankFee: &fee,
			SettledAt: r.SettledAt.Format(time.RFC3339), row: r}
		c, known := byRef[r.Reference]
		if known {
			la := c.Amount.Amount()
			it.LedgerAmount, it.LedgerCurrency = &la, c.Amount.Currency().Code()
		}
		switch {
		case !known:
			it.Class = MissingInLedger
		case r.SettledAt.Before(day) || !r.SettledAt.Before(windowEnd):
			it.Class, it.Detail = MissingInLedger, fmt.Sprintf("settled outside the T..T+%d window", windowDays)
		case settled[r.Reference]:
			it.Class = Duplicate
		case r.Amount.Currency() != c.Amount.Currency():
			it.Class = CurrencyMismatch
			settled[r.Reference] = true
		case r.Amount != c.Amount:
			it.Class = AmountMismatch
			it.Detail = fmt.Sprintf("bank %s, ledger %s", r.Amount, c.Amount)
			settled[r.Reference] = true
		case r.SettledAt.Before(onTimeBy):
			it.Class = Matched
			settled[r.Reference] = true
		default:
			it.Class = Late
			it.Detail = fmt.Sprintf("settled %s, expected by %s", r.SettledAt.Format(time.DateOnly), day.AddDate(0, 0, 1).Format(time.DateOnly))
			settled[r.Reference] = true
		}
		rep.Items = append(rep.Items, it)
	}
	for _, c := range captures {
		if !settled[c.PaymentID] {
			la := c.Amount.Amount()
			rep.Items = append(rep.Items, Item{Reference: c.PaymentID, Class: MissingInBank, LedgerAmount: &la, LedgerCurrency: c.Amount.Currency().Code()})
		}
	}
	slices.SortStableFunc(rep.Items, func(a, b Item) int { return strings.Compare(a.Reference, b.Reference) })
	for _, it := range rep.Items {
		rep.Counts[it.Class]++
	}
	return rep
}

// Write prints the report for humans.
func (r Report) Write(w io.Writer) {
	fmt.Fprintf(w, "Reconciliation for captures of %s (window T+%d)\n\n", r.Day, r.WindowDays)
	for _, c := range []Class{Matched, Late, MissingInBank, MissingInLedger, AmountMismatch, CurrencyMismatch, Duplicate} {
		fmt.Fprintf(w, "  %-18s %6d\n", c, r.Counts[c])
	}
	fmt.Fprintf(w, "\nposted %d settlement(s), %d already posted earlier\n", r.Posted, r.AlreadyPosted)
	var exceptions []Item
	for _, it := range r.Items {
		if it.Class != Matched {
			exceptions = append(exceptions, it)
		}
	}
	if len(exceptions) == 0 {
		fmt.Fprintln(w, "\nno exceptions")
		return
	}
	fmt.Fprintf(w, "\nexceptions (%d):\n", len(exceptions))
	for _, it := range exceptions {
		fmt.Fprintf(w, "  %-18s %s", it.Class, it.Reference)
		if it.BankAmount != nil {
			fmt.Fprintf(w, "  bank %d %s", *it.BankAmount, it.BankCurrency)
		}
		if it.LedgerAmount != nil {
			fmt.Fprintf(w, "  ledger %d %s", *it.LedgerAmount, it.LedgerCurrency)
		}
		if it.Detail != "" {
			fmt.Fprintf(w, "  (%s)", it.Detail)
		}
		fmt.Fprintln(w)
	}
}
