package ledger_test

import (
	"os"
	"strconv"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/kamboj-sajan/ledgerline/internal/ledger/ledgertest"
)

// TestPropLedgerMemory runs the stateful ledger property test against the
// in-memory ledger: random captures, refunds, settlements and reversals across
// several merchants and currencies, mixed with operations that must be
// rejected, all checked against a reference model after every step.
//
// make prop-long runs it with enough checks for at least 1,000,000 operations.
// LEDGERLINE_PROP_MIN_OPS makes the test fail if fewer operations ran, so the
// figure is enforced rather than estimated.
func TestPropLedgerMemory(t *testing.T) {
	var stats ledgertest.Stats
	start := time.Now()
	rapid.Check(t, func(rt *rapid.T) {
		ledgertest.Run(rt, ledgertest.NewMemorySystem(), &stats)
	})
	elapsed := time.Since(start)
	t.Logf("operations: %d (%d posted, %d rejected) in %s",
		stats.Ops, stats.Posted, stats.Rejected, elapsed.Round(time.Millisecond))

	if v := os.Getenv("LEDGERLINE_PROP_MIN_OPS"); v != "" {
		minOps, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("LEDGERLINE_PROP_MIN_OPS=%q: %v", v, err)
		}
		if stats.Ops < minOps {
			t.Fatalf("ran %d operations, want at least %d; raise -rapid.checks or -rapid.steps", stats.Ops, minOps)
		}
	}
}
