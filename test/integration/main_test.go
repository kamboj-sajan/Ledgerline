//go:build integration

// Package integration holds tests that need a real PostgreSQL. They are
// excluded from `go test ./...` by the integration build tag; run them with
// `make test-integration` (Docker required).
package integration

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/kamboj-sajan/ledgerline/internal/store/storetest"
)

// pg is shared by every test in this package. Tests isolate themselves by
// asking it for their own database.
var pg *storetest.Postgres

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

// runTests exists because os.Exit skips deferred calls: keeping the defer in
// a separate function guarantees the container is terminated before exit.
func runTests(m *testing.M) int {
	var err error
	pg, err = storetest.Start(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		if err := pg.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "terminating postgres:", err)
		}
	}()
	return m.Run()
}
