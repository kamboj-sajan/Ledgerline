package payments_test

import (
	"os"
	"strconv"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/kamboj-sajan/ledgerline/internal/payments/paymenttest"
)

// TestPropPaymentsPure runs the payment-lifecycle property test against the
// domain methods and ledger.Memory. make prop-long runs it with
// LEDGERLINE_PROP_MIN_OPS=1000000, which fails the test if fewer ran.
func TestPropPaymentsPure(t *testing.T) {
	var stats paymenttest.Stats
	start := time.Now()
	rapid.Check(t, func(rt *rapid.T) {
		paymenttest.Run(rt, paymenttest.NewPureSystem(), &stats)
	})
	t.Logf("operations: %d (%d accepted, %d rejected) in %s",
		stats.Ops, stats.Accepted, stats.Rejected, time.Since(start).Round(time.Millisecond))
	if v := os.Getenv("LEDGERLINE_PROP_MIN_OPS"); v != "" {
		minOps, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("LEDGERLINE_PROP_MIN_OPS=%q: %v", v, err)
		}
		if stats.Ops < minOps {
			t.Fatalf("ran %d operations, want at least %d", stats.Ops, minOps)
		}
	}
}
