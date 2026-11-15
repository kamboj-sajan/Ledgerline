package webhooks_test

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/webhooks"
)

func TestBackoffCeilings(t *testing.T) {
	// With the RNG pinned to its maximum, Backoff returns the ceiling itself.
	maxRand := func(n int64) int64 { return n - 1 }
	base, capDelay := time.Second, time.Hour
	want := []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048}
	for i, w := range want {
		if got := webhooks.Backoff(i+1, base, capDelay, maxRand); got != w*time.Second {
			t.Errorf("attempt %d: ceiling %s, want %s", i+1, got, w*time.Second)
		}
	}
	// 2^12 s > 1 h: capped from attempt 13 on, and no overflow at absurd attempt numbers.
	for _, attempt := range []int{13, 20, 63, 64, 1000} {
		if got := webhooks.Backoff(attempt, base, capDelay, maxRand); got != capDelay {
			t.Errorf("attempt %d: %s, want the cap %s", attempt, got, capDelay)
		}
	}
	if got := webhooks.Backoff(3, base, capDelay, func(int64) int64 { return 0 }); got != 0 {
		t.Errorf("full jitter must allow a zero delay, got %s", got)
	}
}

func TestBackoffIsSpreadOverTheInterval(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var below, above int
	for range 10_000 {
		d := webhooks.Backoff(4, time.Second, time.Hour, rng.Int64N) // ceiling 8s
		if d < 0 || d > 8*time.Second {
			t.Fatalf("delay %s outside [0, 8s]", d)
		}
		if d < 4*time.Second {
			below++
		} else {
			above++
		}
	}
	// Uniform: roughly half below the midpoint. A generous band avoids flakes.
	if below < 4_500 || above < 4_500 {
		t.Fatalf("%d below and %d above the midpoint; not uniform", below, above)
	}
}

func TestFileStoreDedupesAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "processed.log")
	s, err := webhooks.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		id      string
		wantDup bool
	}{{"evt_1", false}, {"evt_2", false}, {"evt_1", true}} {
		dup, err := s.Record(step.id, "payment.created")
		if err != nil || dup != step.wantDup {
			t.Fatalf("Record(%s) = %v, %v; want duplicate=%v", step.id, dup, err, step.wantDup)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash in the middle of writing a third record.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("evt_3 paym")
	_ = f.Close()

	s, err = webhooks.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if dup, _ := s.Record("evt_2", "x"); !dup {
		t.Fatal("evt_2 processed again after a restart")
	}
	if dup, _ := s.Record("evt_4", "payment.captured"); dup {
		t.Fatal("evt_4 reported as duplicate")
	}
	ids, err := webhooks.ProcessedIDs(path)
	if err != nil {
		t.Fatal(err)
	}
	// The torn "evt_3 paym" line still names a whole id, so it counts as
	// processed. evt_4 starts on a fresh line instead of being glued onto it.
	if want := []string{"evt_1", "evt_2", "evt_3", "evt_4"}; !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}
