// Command banksim plays the bank: it reads one day's captures from the
// ledger and writes the settlement file the network would send, with
// seeded faults, plus a manifest of exactly which faults it injected.
//
//	banksim -date 2026-11-15 -seed 42 -out settlement.csv -manifest manifest.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/recon"
	"github.com/kamboj-sajan/ledgerline/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "banksim:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection URL")
	date := flag.String("date", time.Now().UTC().Format(time.DateOnly), "capture day to settle (UTC, YYYY-MM-DD)")
	var cfg recon.SimConfig
	flag.Uint64Var(&cfg.Seed, "seed", 1, "fault-injection seed")
	flag.Float64Var(&cfg.FaultRate, "fault-rate", 0.1, "probability that a capture gets a fault")
	flag.IntVar(&cfg.UnknownRows, "unknown", 2, "rows for references the ledger has never seen")
	flag.Int64Var(&cfg.FeeBasisPoint, "fee-bps", 20, "network fee on each settled amount, in basis points")
	out := flag.String("out", "settlement.csv", "settlement file to write")
	manifestPath := flag.String("manifest", "manifest.json", "ground-truth manifest to write")
	flag.Parse()
	if *dsn == "" {
		return fmt.Errorf("set -database-url or $DATABASE_URL")
	}
	day, err := time.Parse(time.DateOnly, *date)
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := store.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	captures, err := recon.DayCaptures(ctx, pool, day)
	if err != nil {
		return err
	}
	rows, man := recon.Simulate(day, captures, cfg)

	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	if err := recon.WriteCSV(f, rows); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(man, "", "  ")
	if err := os.WriteFile(*manifestPath, append(b, '\n'), 0o600); err != nil { //nolint:gosec // output path is the operator's own -flag
		return err
	}
	fmt.Printf("%d captures on %s → %d rows in %s; %d faults injected (manifest: %s)\n",
		len(captures), *date, len(rows), *out, len(man.Faults), *manifestPath)
	return nil
}
