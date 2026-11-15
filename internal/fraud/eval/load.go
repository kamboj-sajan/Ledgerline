package eval

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/fraud"
	"github.com/kamboj-sajan/ledgerline/internal/money"
)

// columns maps header names to indexes and fails on a missing one.
func columns(header []string, want ...string) (map[string]int, error) {
	idx := map[string]int{}
	for i, h := range header {
		idx[h] = i
	}
	for _, w := range want {
		if _, ok := idx[w]; !ok {
			return nil, fmt.Errorf("eval: column %q not found in header %v", w, header)
		}
	}
	return idx, nil
}

func openCSV(path string) (*csv.Reader, func() error, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied dataset path
	if err != nil {
		return nil, nil, err
	}
	r := csv.NewReader(f)
	r.ReuseRecord = true
	return r, f.Close, nil
}

// LoadULB reads the ULB dataset (creditcard.csv: Time, V1..V28, Amount,
// Class). Time is seconds since the first transaction; Amount is a decimal
// in euros, parsed exactly into cents. There is no card identifier and no
// location, so only signals that need neither are meaningful.
func LoadULB(path string) ([]Example, error) {
	r, closeFn, err := openCSV(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = closeFn() }()
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("eval: reading %s header: %w", path, err)
	}
	col, err := columns(header, "Time", "Amount", "Class")
	if err != nil {
		return nil, err
	}
	epoch := time.Date(2013, 9, 1, 0, 0, 0, 0, time.UTC) // the dataset covers two days in September 2013
	var out []Example
	for line := 2; ; line++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("eval: %s line %d: %w", path, line, err)
		}
		secs, err := strconv.ParseFloat(rec[col["Time"]], 64)
		if err != nil {
			return nil, fmt.Errorf("eval: %s line %d: Time: %w", path, line, err)
		}
		amt, err := money.ParseMajor(rec[col["Amount"]], money.EUR)
		if err != nil {
			return nil, fmt.Errorf("eval: %s line %d: Amount: %w", path, line, err)
		}
		tx := fraud.Transaction{CardFingerprint: "ulb", Amount: amt.Amount(), Currency: "EUR",
			Time: epoch.Add(time.Duration(secs * float64(time.Second)))}
		out = append(out, Example{Tx: tx, Fraud: rec[col["Class"]] == "1"})
	}
	SortByTime(out)
	return out, nil
}

// LoadSparkov reads the Sparkov train and test files and computes signals
// for every transaction, replaying both files in time order through one
// card history. Test transactions can therefore see the card's earlier train
// transactions as history, as a live system would. Labels are never used to
// compute signals. One signal set is computed per velocity window.
func LoadSparkov(trainPath, testPath string, base fraud.Config, windows []time.Duration) (train, test []Example, err error) {
	trainRaw, err := readSparkov(trainPath)
	if err != nil {
		return nil, nil, err
	}
	testRaw, err := readSparkov(testPath)
	if err != nil {
		return nil, nil, err
	}
	all := slices.Concat(trainRaw, testRaw)
	SortByTime(all)

	cfgs := make([]fraud.Config, len(windows))
	for i, w := range windows {
		cfgs[i] = base
		cfgs[i].Velocity.Window = fraud.Duration{Duration: w}
	}
	hist := NewHistory(base.HistoryWindow.Duration)
	isTrain := make(map[string]bool, len(trainRaw))
	for _, e := range trainRaw {
		isTrain[e.Tx.ID] = true
	}
	for i := range all {
		e := &all[i]
		h := hist.Before(e.Tx)
		e.Sigs = make([]fraud.Signals, len(cfgs))
		for j, c := range cfgs {
			e.Sigs[j] = c.Signals(e.Tx, h)
		}
		hist.Add(e.Tx)
	}
	for _, e := range all {
		if isTrain[e.Tx.ID] {
			train = append(train, e)
		} else {
			test = append(test, e)
		}
	}
	return train, test, nil
}

func readSparkov(path string) ([]Example, error) {
	r, closeFn, err := openCSV(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = closeFn() }()
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("eval: reading %s header: %w", path, err)
	}
	col, err := columns(header, "cc_num", "amt", "unix_time", "lat", "long", "merch_lat", "merch_long", "is_fraud", "trans_num")
	if err != nil {
		return nil, err
	}
	var out []Example
	for line := 2; ; line++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("eval: %s line %d: %w", path, line, err)
		}
		f := func(name string) (float64, error) { return strconv.ParseFloat(rec[col[name]], 64) }
		ts, err := strconv.ParseInt(rec[col["unix_time"]], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("eval: %s line %d: unix_time: %w", path, line, err)
		}
		amt, err := money.ParseMajor(rec[col["amt"]], money.USD)
		if err != nil {
			return nil, fmt.Errorf("eval: %s line %d: amt: %w", path, line, err)
		}
		lat, err1 := f("lat")
		lon, err2 := f("long")
		mlat, err3 := f("merch_lat")
		mlon, err4 := f("merch_long")
		if err := errors.Join(err1, err2, err3, err4); err != nil {
			return nil, fmt.Errorf("eval: %s line %d: coordinates: %w", path, line, err)
		}
		tx := fraud.Transaction{
			ID: rec[col["trans_num"]], CardFingerprint: rec[col["cc_num"]], Amount: amt.Amount(), Currency: "USD",
			Time: time.Unix(ts, 0).UTC(), HasLocation: true, HolderLat: lat, HolderLon: lon, MerchLat: mlat, MerchLon: mlon,
		}
		out = append(out, Example{Tx: tx, Fraud: rec[col["is_fraud"]] == "1"})
	}
	return out, nil
}
