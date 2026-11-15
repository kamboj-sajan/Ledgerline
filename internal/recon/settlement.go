// Package recon reconciles the bank's settlement file against the ledger.
//
// The card network pays out a day's captures in a settlement file, one row
// per captured payment. Reconcile matches rows to the ledger's captures by
// reference (the payment ID) and classifies every row, and every capture
// with no row. Matched rows (including late ones within the window) are
// posted to the ledger as settlement transactions keyed by the bank
// reference. external_ref is UNIQUE, so running reconciliation again never
// posts twice.
//
// Simulate is the bank: it writes a day's file from the ledger's captures,
// injects seeded faults, and records them in a manifest the tests check the
// report against.
package recon

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/money"
)

// Row is one line of a settlement file.
type Row struct {
	Reference string
	Amount    money.Money // gross amount settled for the capture
	Fee       money.Money // the network's fee, deducted from what reaches the bank
	SettledAt time.Time
}

var header = []string{"reference", "amount", "currency", "fee", "settled_at"}

// WriteCSV writes rows as a settlement file. Amounts are decimals in major
// units ("25.00"), the way banks write them; they are parsed back exactly.
func WriteCSV(w io.Writer, rows []Row) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, r := range rows {
		if err := cw.Write([]string{
			r.Reference, r.Amount.FormatMajor(), r.Amount.Currency().Code(), r.Fee.FormatMajor(),
			r.SettledAt.UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// ReadCSV parses a settlement file strictly: exact decimals, known
// currencies, RFC 3339 times. A malformed row is an error naming its line,
// not a silent skip.
func ReadCSV(r io.Reader) ([]Row, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = len(header)
	got, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("recon: reading header: %w", err)
	}
	for i := range header {
		if got[i] != header[i] {
			return nil, fmt.Errorf("recon: header %v, want %v", got, header)
		}
	}
	var rows []Row
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, fmt.Errorf("recon: line %d: %w", line, err)
		}
		c, err := money.ParseCurrency(rec[2])
		if err != nil {
			return nil, fmt.Errorf("recon: line %d: %w", line, err)
		}
		amount, err := money.ParseMajor(rec[1], c)
		if err != nil {
			return nil, fmt.Errorf("recon: line %d: amount: %w", line, err)
		}
		fee, err := money.ParseMajor(rec[3], c)
		if err != nil {
			return nil, fmt.Errorf("recon: line %d: fee: %w", line, err)
		}
		at, err := time.Parse(time.RFC3339, rec[4])
		if err != nil {
			return nil, fmt.Errorf("recon: line %d: settled_at: %w", line, err)
		}
		if rec[0] == "" {
			return nil, fmt.Errorf("recon: line %d: empty reference", line)
		}
		rows = append(rows, Row{Reference: rec[0], Amount: amount, Fee: fee, SettledAt: at.UTC()})
	}
}
