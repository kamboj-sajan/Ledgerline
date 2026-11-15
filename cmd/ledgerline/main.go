// Command ledgerline is the Ledgerline server and operations CLI.
//
// Usage:
//
//	ledgerline migrate [-database-url URL] [up|down|status]
//	ledgerline audit   [-database-url URL]
//
// The database URL defaults to $DATABASE_URL.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5"

	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/store"
)

const usage = `usage: ledgerline <command> [flags]

commands:
  migrate [up|down|status]   apply, roll back one, or list schema migrations
  audit                      check ledger invariants; exits 1 if any fail
`

func main() {
	// Cancel ctx on Ctrl-C or SIGTERM so in-flight work can stop cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		slog.Error("ledgerline failed", "error", err)
		os.Exit(1)
	}
}

// run is main without the process-level side effects, so it can be tested.
func run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command given")
	}
	switch args[0] {
	case "migrate":
		return runMigrate(ctx, args[1:], stdout)
	case "audit":
		return runAudit(ctx, args[1:], stdout)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// runMigrate uses a named result (err) so the deferred Close can report its
// own failure without hiding an earlier error.
func runMigrate(ctx context.Context, args []string, stdout io.Writer) (err error) {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dsn := databaseURLFlag(fs)
	if err := parseWithDSN(fs, args, dsn); err != nil {
		return err
	}
	action := "up"
	if fs.NArg() > 0 {
		action = fs.Arg(0)
	}

	m, err := store.NewMigrator(*dsn)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, m.Close()) }()

	switch action {
	case "up":
		results, err := m.Up(ctx)
		for _, r := range results {
			fmt.Fprintf(stdout, "applied  %s (%s)\n", r.Source.Path, r.Duration)
		}
		if err == nil && len(results) == 0 {
			fmt.Fprintln(stdout, "no pending migrations")
		}
		return err
	case "down":
		r, err := m.Down(ctx)
		if r != nil {
			fmt.Fprintf(stdout, "reverted %s (%s)\n", r.Source.Path, r.Duration)
		}
		return err
	case "status":
		statuses, err := m.Status(ctx)
		for _, s := range statuses {
			applied := "pending"
			if !s.AppliedAt.IsZero() {
				applied = s.AppliedAt.Format("2006-01-02 15:04:05")
			}
			fmt.Fprintf(stdout, "%-40s %s\n", s.Source.Path, applied)
		}
		return err
	default:
		return fmt.Errorf("migrate: unknown action %q (want up, down or status)", action)
	}
}

// runAudit checks the ledger invariants against stored rows. It reads in one
// REPEATABLE READ, read-only transaction, so every check sees the same
// snapshot even while the API is writing.
func runAudit(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	dsn := databaseURLFlag(fs)
	if err := parseWithDSN(fs, args, dsn); err != nil {
		return err
	}
	pool, err := store.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // read-only: nothing to commit

	report, err := ledger.Audit(ctx, tx)
	if err != nil {
		return err
	}
	report.Write(stdout)
	if !report.OK() {
		return errors.New("audit: ledger invariants violated")
	}
	return nil
}

func databaseURLFlag(fs *flag.FlagSet) *string {
	return fs.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection URL (default $DATABASE_URL)")
}

func parseWithDSN(fs *flag.FlagSet, args []string, dsn *string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dsn == "" {
		return fmt.Errorf("%s: set -database-url or $DATABASE_URL", fs.Name())
	}
	return nil
}
