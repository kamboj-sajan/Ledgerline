// Command reconcile matches a bank settlement file against one day's
// captures in the ledger, classifies every row, posts the settlements it
// can match, and writes a JSON report plus a readable one. Running it again
// posts nothing new.
//
//	reconcile -date 2026-11-15 -file settlement.csv -report report.json
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
		fmt.Fprintln(os.Stderr, "reconcile:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection URL")
	date := flag.String("date", time.Now().UTC().Format(time.DateOnly), "capture day the file settles (UTC, YYYY-MM-DD)")
	file := flag.String("file", "settlement.csv", "settlement file from the bank")
	window := flag.Int("window-days", 3, "rows settled after T+1 but within T+N are late (and still posted)")
	reportPath := flag.String("report", "report.json", "JSON report to write")
	dryRun := flag.Bool("dry-run", false, "classify only; post nothing")
	flag.Parse()
	if *dsn == "" {
		return fmt.Errorf("set -database-url or $DATABASE_URL")
	}
	day, err := time.Parse(time.DateOnly, *date)
	if err != nil {
		return err
	}
	f, err := os.Open(*file)
	if err != nil {
		return err
	}
	rows, err := recon.ReadCSV(f)
	_ = f.Close()
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
	rep := recon.Reconcile(day, captures, rows, *window)
	if !*dryRun {
		if err := recon.Post(ctx, pool, &rep); err != nil {
			return err
		}
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(*reportPath, append(b, '\n'), 0o600); err != nil { //nolint:gosec // output path is the operator's own -flag
		return err
	}
	rep.Write(os.Stdout)
	return nil
}
