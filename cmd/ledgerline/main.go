// Command ledgerline is the Ledgerline server and operations CLI.
//
// Usage:
//
//	ledgerline migrate [-database-url URL] [up|down|status]
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

	"github.com/kamboj-sajan/ledgerline/internal/store"
)

const usage = `usage: ledgerline <command> [flags]

commands:
  migrate [up|down|status]   apply, roll back one, or list schema migrations
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
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runMigrate(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dsn := fs.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dsn == "" {
		return errors.New("migrate: set -database-url or $DATABASE_URL")
	}
	action := "up"
	if fs.NArg() > 0 {
		action = fs.Arg(0)
	}

	m, err := store.NewMigrator(*dsn)
	if err != nil {
		return err
	}
	defer m.Close()

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
