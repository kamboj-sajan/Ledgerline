// Command receiver is a sample merchant webhook endpoint. It verifies each
// delivery's signature and timestamp, processes every event id once (dedupe
// across retries and restarts), and records processed ids in a file that
// tests read.
//
// Usage:
//
//	receiver -addr :9090 -secrets whsec_new,whsec_old -store processed.log
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/webhooks"
)

func main() {
	addr := flag.String("addr", ":9090", "listen address")
	secrets := flag.String("secrets", os.Getenv("RECEIVER_SECRETS"), "comma-separated signing secrets (two during a rotation)")
	storePath := flag.String("store", "processed.log", "file recording processed event ids")
	tolerance := flag.Duration("tolerance", webhooks.DefaultTolerance, "maximum age of a signature timestamp")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(*addr, *secrets, *storePath, *tolerance, log); err != nil {
		log.Error("receiver failed", "error", err)
		os.Exit(1)
	}
}

func run(addr, secrets, storePath string, tolerance time.Duration, log *slog.Logger) error {
	if secrets == "" {
		return errors.New("set -secrets or $RECEIVER_SECRETS")
	}
	store, err := webhooks.OpenFileStore(storePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{
		Addr:              addr,
		Handler:           &webhooks.Receiver{Secrets: strings.Split(secrets, ","), Tolerance: tolerance, Store: store, Log: log},
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Info("listening", "addr", addr)
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
