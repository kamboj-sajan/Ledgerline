//go:build chaos

// Package chaos runs Ledgerline's processes (serve, dispatch, receiver) as
// child processes built with the chaos tag, crashes them with SIGKILL at
// failpoints and at random times, restarts them, and checks that money and
// webhooks stay correct. Run with make chaos.
package chaos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/merchants"
	"github.com/kamboj-sajan/ledgerline/internal/payments"
	"github.com/kamboj-sajan/ledgerline/internal/store"
	"github.com/kamboj-sajan/ledgerline/internal/store/storetest"
	"github.com/kamboj-sajan/ledgerline/internal/webhooks"
)

var (
	pg     *storetest.Postgres
	binDir string
)

func TestMain(m *testing.M) { os.Exit(runTests(m)) }

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "ledgerline-chaos-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	binDir = dir
	// Build the real binaries with the failpoints compiled in.
	build := exec.CommandContext(context.Background(), "go", "build", "-tags", "chaos", "-o", binDir+string(os.PathSeparator), "../../cmd/ledgerline", "../../cmd/receiver")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building binaries:", err)
		return 1
	}
	pg, err = storetest.Start(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = pg.Close() }()
	return m.Run()
}

// proc is a supervised child process that is restarted whenever it dies.
type proc struct {
	name string
	args []string
	env  []string
	log  *os.File

	mu       sync.Mutex
	cmd      *exec.Cmd
	stopping bool
	restarts atomic.Int32
	exited   chan struct{}
}

func (p *proc) start(t *testing.T) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Not tied to a context: the supervisor ends processes with signals.
	cmd := exec.CommandContext(context.Background(), filepath.Join(binDir, p.args[0]), p.args[1:]...)
	cmd.Env = append(os.Environ(), p.env...)
	cmd.Stdout, cmd.Stderr = p.log, p.log
	if err := cmd.Start(); err != nil {
		t.Errorf("starting %s: %v", p.name, err)
		return
	}
	p.cmd = cmd
	exited := make(chan struct{})
	p.exited = exited
	go func() {
		_ = cmd.Wait()
		close(exited)
		p.mu.Lock()
		stopping := p.stopping
		p.mu.Unlock()
		if !stopping {
			p.restarts.Add(1)
			time.Sleep(50 * time.Millisecond)
			p.start(t)
		}
	}()
}

func (p *proc) kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGKILL)
	}
}

func (p *proc) stop() {
	p.mu.Lock()
	p.stopping = true
	cmd, exited := p.cmd, p.exited
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// client sends API requests and retries each one with the same key until
// the answer is definitive: anything except a connection error, a 5xx or
// 409 idempotency_key_in_use.
type client struct {
	base, apiKey string
	http         *http.Client
	retries      atomic.Int64
}

func (c *client) send(ctx context.Context, method, path, key, body string) (int, []byte) {
	for {
		req, _ := http.NewRequestWithContext(ctx, method, c.base+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := c.http.Do(req)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			inUse := resp.StatusCode == http.StatusConflict && bytes.Contains(b, []byte("idempotency_key_in_use"))
			if resp.StatusCode < 500 && !inUse {
				return resp.StatusCode, b
			}
		}
		if ctx.Err() != nil {
			return 0, nil
		}
		c.retries.Add(1)
		time.Sleep(time.Duration(30+rand.IntN(120)) * time.Millisecond)
	}
}

type flowResult struct {
	paymentID string
	declined  bool
	voided    bool
	captured  int64
	refunded  int64
}

// flow runs one payment through create → authorize → capture → refund (or
// void), with a fixed idempotency key per step.
func (c *client) flow(ctx context.Context, t *testing.T, n int) flowResult {
	key := func(step string) string { return fmt.Sprintf("flow-%d-%s", n, step) }
	var r flowResult
	amount := int64(1_000 + n*37)
	card := "fp_ok"
	status, body := c.send(ctx, "POST", "/v1/payments", key("create"),
		fmt.Sprintf(`{"amount": %d, "currency": "USD", "customer_id": "cus_%d", "card": {"fingerprint": "%s", "country": "US"}, "ip_country": "US"}`, amount, n, card))
	if status != http.StatusCreated {
		t.Errorf("flow %d create: %d %s", n, status, body)
		return r
	}
	var p struct{ ID string }
	_ = json.Unmarshal(body, &p)
	r.paymentID = p.ID
	path := "/v1/payments/" + p.ID

	switch status, body = c.send(ctx, "POST", path+"/authorize", key("authorize"), ""); status {
	case http.StatusOK:
	case http.StatusPaymentRequired:
		r.declined = true
		return r
	default:
		t.Errorf("flow %d authorize: %d %s", n, status, body)
		return r
	}
	if n%5 == 0 {
		if status, body = c.send(ctx, "POST", path+"/void", key("void"), ""); status != http.StatusOK {
			t.Errorf("flow %d void: %d %s", n, status, body)
		}
		r.voided = true
		return r
	}
	capture := amount - int64(n%3)*100 // some partial captures
	if status, body = c.send(ctx, "POST", path+"/capture", key("capture"), fmt.Sprintf(`{"amount": %d}`, capture)); status != http.StatusOK {
		t.Errorf("flow %d capture: %d %s", n, status, body)
		return r
	}
	r.captured = capture
	for i, refund := range []int64{capture / 4, capture / 4} {
		if status, body = c.send(ctx, "POST", path+"/refunds", key(fmt.Sprintf("refund-%d", i)), fmt.Sprintf(`{"amount": %d}`, refund)); status != http.StatusCreated {
			t.Errorf("flow %d refund %d: %d %s", n, i, status, body)
			return r
		}
		r.refunded += refund
	}
	return r
}

func TestChaos(t *testing.T) {
	const (
		flows   = 200
		workers = 8
	)
	ctx := t.Context()
	dsn := pg.NewDatabase(t)
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dir := t.TempDir()

	m, apiKey, err := merchants.Create(ctx, db, "Chaos Merchant")
	if err != nil {
		t.Fatal(err)
	}
	apiPort, rcvPort := freePort(t), freePort(t)
	var secret string
	if err := withTx(ctx, db, func(tx pgx.Tx) error {
		ep, err := webhooks.CreateEndpoint(ctx, tx, m.ID, fmt.Sprintf("http://127.0.0.1:%d/hooks", rcvPort))
		secret = ep.Secret
		return err
	}); err != nil {
		t.Fatal(err)
	}

	openLog := func(name string) *os.File {
		f, err := os.Create(filepath.Join(dir, name+".log"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	dbEnv := "DATABASE_URL=" + dsn
	procs := []*proc{
		{name: "serve", log: openLog("serve"), env: []string{dbEnv,
			"LEDGERLINE_FAILPOINTS=authorize-after-acquirer-call=0.05,api-after-commit=0.03"},
			args: []string{"ledgerline", "serve", "-addr", fmt.Sprintf("127.0.0.1:%d", apiPort), "-log-level", "warn",
				"-authorize-lock-timeout", "1s", "-acquirer-latency", "5ms", "-acquirer-jitter", "20ms",
				"-acquirer-decline-rate", "0.1", "-acquirer-timeout-rate", "0.05", "-acquirer-error-rate", "0.05",
				"-sweep-interval", "1h"}},
		{name: "dispatch", log: openLog("dispatch"), env: []string{dbEnv, "LEDGERLINE_FAILPOINTS=dispatch-after-send=0.05"},
			args: []string{"ledgerline", "dispatch", "-lease", "1s", "-retry-base", "50ms", "-retry-max", "500ms",
				"-max-attempts", "1000", "-poll", "20ms", "-timeout", "2s"}},
		{name: "receiver", log: openLog("receiver"),
			args: []string{"receiver", "-addr", fmt.Sprintf("127.0.0.1:%d", rcvPort), "-secrets", secret,
				"-store", filepath.Join(dir, "processed.log")}},
	}
	for _, p := range procs {
		p.start(t)
	}
	defer func() {
		for _, p := range procs {
			p.stop()
		}
	}()

	// The killer: SIGKILL a random process every 200-800 ms.
	var kills atomic.Int32
	stopKilling := make(chan struct{})
	killerDone := make(chan struct{})
	go func() {
		defer close(killerDone)
		for {
			select {
			case <-stopKilling:
				return
			case <-time.After(time.Duration(200+rand.IntN(600)) * time.Millisecond):
			}
			procs[rand.IntN(len(procs))].kill()
			kills.Add(1)
		}
	}()

	c := &client{base: fmt.Sprintf("http://127.0.0.1:%d", apiPort), apiKey: apiKey, http: &http.Client{Timeout: 5 * time.Second}}
	start := time.Now()
	results := make([]flowResult, flows)
	next := atomic.Int32{}
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for {
				n := int(next.Add(1)) - 1
				if n >= flows {
					return
				}
				results[n] = c.flow(ctx, t, n)
			}
		})
	}
	wg.Wait()
	close(stopKilling)
	<-killerDone
	clientsDone := time.Since(start)

	// Let the dispatcher (still hit by its failpoint) finish delivering.
	deadline := time.Now().Add(90 * time.Second)
	for count(t, db, `SELECT count(*) FROM webhook_deliveries WHERE status <> 'delivered'`) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d deliveries still not delivered", count(t, db, `SELECT count(*) FROM webhook_deliveries WHERE status <> 'delivered'`))
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, p := range procs {
		p.stop()
	}

	// ---- Assertions ----

	// 1. The books balance and agree with the payments table.
	lr, err := ledger.Audit(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	pr, err := payments.Audit(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if !lr.OK() || !pr.OK() {
		t.Errorf("audit failed:\nledger %+v\npayments %+v", lr, pr)
	}

	// Each flow made exactly one payment, in the state its client saw.
	if n := count(t, db, `SELECT count(*) FROM payments`); n != flows {
		t.Errorf("%d payments for %d flows: a retried create made a duplicate", n, flows)
	}
	for i, r := range results {
		var status string
		var captured, refunded int64
		if err := db.QueryRow(ctx, `SELECT status, captured_amount, refunded_amount FROM payments WHERE id = $1`, r.paymentID).
			Scan(&status, &captured, &refunded); err != nil {
			t.Errorf("flow %d payment %q: %v", i, r.paymentID, err)
			continue
		}
		if captured != r.captured || refunded != r.refunded || ((r.declined || r.voided) != (status == "failed")) {
			t.Errorf("flow %d: payment %s is %s captured %d refunded %d, client saw %+v", i, r.paymentID, status, captured, refunded, r)
		}
	}

	// 2. Exactly one outbox event per committed state change. From the
	// payments table: created (1) + authorized (if it ever was) + failed (if
	// it is) + captured (if it was) + one per refund.
	if bad := count(t, db, `
		SELECT count(*) FROM payments p
		WHERE (SELECT count(*) FROM events e WHERE e.payment_id = p.id) <>
		      1 + (p.authorized_amount > 0)::int + (p.status = 'failed')::int + (p.captured_amount > 0)::int
		        + (SELECT count(*) FROM refunds r WHERE r.payment_id = p.id)`); bad != 0 {
		t.Errorf("%d payments whose event count differs from their committed state changes", bad)
	}

	// 3. Every event was delivered at least once.
	events := ids(t, db, `SELECT id FROM events ORDER BY id`)
	if n := count(t, db, `SELECT count(*) FROM webhook_deliveries WHERE status = 'delivered'`); n != len(events) {
		t.Errorf("%d delivered deliveries for %d events", n, len(events))
	}

	// 4. The receiver processed each event id exactly once.
	processed, err := webhooks.ProcessedIDs(filepath.Join(dir, "processed.log"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(processed)
	if !slices.Equal(processed, events) {
		t.Errorf("receiver processed %d ids (%d unique) for %d events", len(processed), len(slices.Compact(slices.Clone(processed))), len(events))
	}

	// 5. No payment was authorized twice at the acquirer, and the acquirer
	// and our payments agree on which were approved.
	if n := count(t, db, `SELECT count(*) FROM (SELECT payment_reference FROM acquirer.authorizations GROUP BY payment_reference HAVING count(*) > 1) d`); n != 0 {
		t.Errorf("%d payments authorized more than once at the acquirer", n)
	}
	if n := count(t, db, `
		SELECT count(*) FROM payments p LEFT JOIN acquirer.authorizations a ON a.payment_reference = p.id
		WHERE COALESCE(a.approved, false) <> (p.authorized_amount > 0)`); n != 0 {
		t.Errorf("%d payments where the acquirer's decision and our record disagree", n)
	}

	// What actually happened, so a pass is not a pass by inactivity.
	serveLog, _ := os.ReadFile(filepath.Join(dir, "serve.log"))
	dispatchLog, _ := os.ReadFile(filepath.Join(dir, "dispatch.log"))
	byPoint := map[string]int{
		"authorize-after-acquirer-call": bytes.Count(serveLog, []byte("failpoint authorize-after-acquirer-call")),
		"api-after-commit":              bytes.Count(serveLog, []byte("failpoint api-after-commit")),
		"dispatch-after-send":           bytes.Count(dispatchLog, []byte("failpoint dispatch-after-send")),
	}
	failpointKills := 0
	for point, n := range byPoint {
		failpointKills += n
		if n == 0 {
			t.Errorf("failpoint %s never fired; that crash point went untested", point)
		}
	}
	rlog, _ := os.ReadFile(filepath.Join(dir, "receiver.log"))
	dupes := bytes.Count(rlog, []byte("duplicate event acknowledged"))
	restarts := map[string]int32{}
	for _, p := range procs {
		restarts[p.name] = p.restarts.Load()
	}
	t.Logf("flows %d in %s; random SIGKILLs %d; failpoint SIGKILLs %d %v; restarts %v; client retries %d; events %d; duplicate deliveries deduped by the receiver %d",
		flows, clientsDone.Round(time.Millisecond), kills.Load(), failpointKills, byPoint, restarts, c.retries.Load(), len(events), dupes)
	if kills.Load() == 0 || failpointKills == 0 {
		t.Errorf("no crashes happened (random %d, failpoint %d); the test proved nothing", kills.Load(), failpointKills)
	}
}

func withTx(ctx context.Context, db *pgxpool.Pool, f func(pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := f(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func count(t *testing.T, db *pgxpool.Pool, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(t.Context(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func ids(t *testing.T, db *pgxpool.Pool, q string) []string {
	t.Helper()
	rows, err := db.Query(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}
