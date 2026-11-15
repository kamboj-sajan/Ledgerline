// Command ledgerline is the Ledgerline server and operations CLI.
//
// Usage:
//
//	ledgerline serve   [flags]                 run the HTTP API
//	ledgerline dispatch [flags]                deliver webhooks from the outbox
//	ledgerline merchant create -name NAME      create a merchant and print its API key
//	ledgerline migrate [up|down|status]        manage schema migrations
//	ledgerline audit                           check ledger and payment invariants
//
// The database URL defaults to $DATABASE_URL.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kamboj-sajan/ledgerline/internal/acquirer"
	"github.com/kamboj-sajan/ledgerline/internal/api"
	"github.com/kamboj-sajan/ledgerline/internal/idempotency"
	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/merchants"
	"github.com/kamboj-sajan/ledgerline/internal/payments"
	"github.com/kamboj-sajan/ledgerline/internal/store"
	"github.com/kamboj-sajan/ledgerline/internal/webhooks"
)

const usage = `usage: ledgerline <command> [flags]

commands:
  serve                      run the HTTP API (see 'ledgerline serve -h')
  dispatch                   deliver webhook events (any number may run at once)
  merchant create -name N    create a merchant; prints its ID and API key (shown once)
  migrate [up|down|status]   apply, roll back one, or list schema migrations
  audit                      check ledger and payment invariants; exits 1 if any fail
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
	case "serve":
		return runServe(ctx, args[1:])
	case "dispatch":
		return runDispatch(ctx, args[1:])
	case "merchant":
		if len(args) < 2 || args[1] != "create" {
			return errors.New("usage: ledgerline merchant create -name NAME")
		}
		return runMerchantCreate(ctx, args[2:], stdout)
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

	ledgerReport, err := ledger.Audit(ctx, tx)
	if err != nil {
		return err
	}
	paymentsReport, err := payments.Audit(ctx, tx)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "== ledger")
	ledgerReport.Write(stdout)
	fmt.Fprintln(stdout, "== payments vs ledger")
	paymentsReport.Write(stdout)
	if !ledgerReport.OK() || !paymentsReport.OK() {
		fmt.Fprintln(stdout, "AUDIT FAILED")
		return errors.New("audit: invariants violated")
	}
	fmt.Fprintln(stdout, "AUDIT PASSED")
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

func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dsn := databaseURLFlag(fs)
	addr := fs.String("addr", envOr("LEDGERLINE_ADDR", ":8080"), "listen address")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	def := api.DefaultConfig()
	lockTimeout := fs.Duration("authorize-lock-timeout", def.AuthorizeLockTimeout, "how long an authorize request may hold its key before a retry may take over")
	callTimeout := fs.Duration("acquirer-call-timeout", def.AcquirerTimeout, "timeout for each acquirer call")
	expiry := fs.Duration("authorization-expiry", 7*24*time.Hour, "uncaptured authorizations older than this are expired")
	sweepEvery := fs.Duration("sweep-interval", time.Minute, "how often to expire authorizations and purge old idempotency keys")
	var sim acquirer.Config
	fs.DurationVar(&sim.Latency, "acquirer-latency", 50*time.Millisecond, "simulated acquirer base latency")
	fs.DurationVar(&sim.Jitter, "acquirer-jitter", 50*time.Millisecond, "simulated acquirer extra random latency")
	fs.Float64Var(&sim.DeclineRate, "acquirer-decline-rate", 0.05, "share of authorizations the simulated acquirer declines")
	fs.Float64Var(&sim.TimeoutRate, "acquirer-timeout-rate", 0, "share of authorizations recorded and then timed out")
	fs.Float64Var(&sim.ErrorRate, "acquirer-error-rate", 0, "share of calls refused before processing")
	fs.Uint64Var(&sim.Seed, "acquirer-seed", 0, "simulator random seed (0 = random)")
	if err := parseWithDSN(fs, args, dsn); err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("serve: -log-level: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	pool, err := store.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	srv := api.New(pool, acquirer.NewSimulator(pool, sim),
		api.Config{AuthorizeLockTimeout: *lockTimeout, AcquirerTimeout: *callTimeout}, log)

	go sweep(ctx, log, srv, pool, *expiry, *sweepEvery)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	log.Info("listening", "addr", *addr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// sweep periodically expires stale authorizations and purges expired
// idempotency keys until ctx is cancelled.
func sweep(ctx context.Context, log *slog.Logger, srv *api.Server, pool store.DB, expiry, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if n, err := srv.ExpireAuthorizations(ctx, expiry, 100); err != nil {
			log.Error("expiring authorizations", "error", err)
		} else if n > 0 {
			log.Info("expired authorizations", "count", n)
		}
		if n, err := idempotency.Purge(ctx, pool); err != nil {
			log.Error("purging idempotency keys", "error", err)
		} else if n > 0 {
			log.Info("purged idempotency keys", "count", n)
		}
	}
}

func runMerchantCreate(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("merchant create", flag.ContinueOnError)
	dsn := databaseURLFlag(fs)
	name := fs.String("name", "", "merchant name")
	if err := parseWithDSN(fs, args, dsn); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("merchant create: -name is required")
	}
	pool, err := store.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	m, key, err := merchants.Create(ctx, pool, *name)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"id": m.ID, "name": m.Name, "api_key": key,
		"note": "the API key is shown only once; Ledgerline stores only its SHA-256 hash"})
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func runDispatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dispatch", flag.ContinueOnError)
	dsn := databaseURLFlag(fs)
	cfg := webhooks.DefaultDispatcherConfig()
	fs.IntVar(&cfg.BatchSize, "batch", cfg.BatchSize, "deliveries claimed per round")
	fs.IntVar(&cfg.Workers, "workers", cfg.Workers, "concurrent requests")
	fs.DurationVar(&cfg.Lease, "lease", cfg.Lease, "how long a claimed delivery stays claimed (re-sent after a crash once it expires)")
	fs.DurationVar(&cfg.HTTPTimeout, "timeout", cfg.HTTPTimeout, "per-request timeout")
	fs.DurationVar(&cfg.BaseDelay, "retry-base", cfg.BaseDelay, "first retry delay cap (full jitter)")
	fs.DurationVar(&cfg.MaxDelay, "retry-max", cfg.MaxDelay, "retry delay cap")
	fs.IntVar(&cfg.MaxAttempts, "max-attempts", cfg.MaxAttempts, "attempts before a delivery is dead")
	fs.DurationVar(&cfg.PollInterval, "poll", cfg.PollInterval, "sleep when nothing is due")
	if err := parseWithDSN(fs, args, dsn); err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	pool, err := store.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("dispatching")
	return webhooks.NewDispatcher(pool, cfg, log).Run(ctx)
}
