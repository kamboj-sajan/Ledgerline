//go:build chaos

// Package failpoint marks places where the chaos tests crash the process on
// purpose. This file is compiled only with the chaos build tag.
//
// LEDGERLINE_FAILPOINTS configures it: a comma-separated list of name=p,
// where p is the probability in [0, 1] that reaching the point kills the
// process with SIGKILL. SIGKILL cannot be caught, so no deferred cleanup,
// rollback or graceful shutdown runs: the process just stops, as it would
// on a power cut.
package failpoint

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"syscall"
)

var probabilities = parse(os.Getenv("LEDGERLINE_FAILPOINTS"))

func parse(spec string) map[string]float64 {
	out := map[string]float64{}
	for _, part := range strings.Split(spec, ",") {
		name, p, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(p, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failpoint: bad probability in %q\n", part)
			os.Exit(2)
		}
		out[name] = f
	}
	return out
}

// Inject kills the process with the configured probability for name.
func Inject(name string) {
	p, ok := probabilities[name]
	if !ok || rand.Float64() >= p { //nolint:gosec // fault injection, not security
		return
	}
	fmt.Fprintf(os.Stderr, "failpoint %s: killing process\n", name)
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {} // SIGKILL is not instant; never continue past the point
}
