//go:build integration

package integration

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/payments"
	"github.com/kamboj-sajan/ledgerline/internal/webhooks"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fastDispatch() webhooks.DispatcherConfig {
	cfg := webhooks.DefaultDispatcherConfig()
	cfg.BaseDelay, cfg.MaxDelay, cfg.PollInterval, cfg.HTTPTimeout = 10*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond, 2*time.Second
	return cfg
}

// drain runs dispatchers until nothing is pending or the deadline passes.
func (e *apiEnv) drain(d *webhooks.Dispatcher, within time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := d.RunOnce(e.t.Context()); err != nil {
			e.t.Fatal(err)
		}
		if e.count(`SELECT count(*) FROM webhook_deliveries WHERE status = 'pending'`) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("deliveries still pending after %s", within)
}

func (e *apiEnv) eventIDs() []string {
	e.t.Helper()
	rows, err := e.db.Query(e.t.Context(), `SELECT id FROM events ORDER BY id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			e.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// receiverServer runs the sample Receiver over HTTP. Its secret is set after
// the endpoint is created, because the API generates it.
type receiverServer struct {
	srv   *httptest.Server
	rc    *webhooks.Receiver
	store string
}

func newReceiverServer(t *testing.T) *receiverServer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "processed.log")
	st, err := webhooks.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := &receiverServer{rc: &webhooks.Receiver{Tolerance: webhooks.DefaultTolerance, Store: st, Log: quietLog()}, store: path}
	r.srv = httptest.NewServer(r.rc)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiverServer) processed(t *testing.T) []string {
	t.Helper()
	ids, err := webhooks.ProcessedIDs(r.store)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestDispatchDeliversSignedEventsToReceiver(t *testing.T) {
	e := newAPIEnv(t)
	rs := newReceiverServer(t)
	ep := e.createEndpoint(rs.srv.URL)
	rs.rc.Secrets = []string{ep.Secret}

	e.paymentIn(payments.StatusRefunded) // 4 events
	e.paymentIn(payments.StatusFailed)   // 2 events
	e.drain(webhooks.NewDispatcher(e.db, fastDispatch(), quietLog()), 10*time.Second)

	if n := e.count(`SELECT count(*) FROM webhook_deliveries WHERE status <> 'delivered'`); n != 0 {
		t.Fatalf("%d deliveries not delivered", n)
	}
	got := rs.processed(t)
	sort.Strings(got)
	if want := e.eventIDs(); !slices.Equal(got, want) {
		t.Fatalf("receiver processed %v, want every event once: %v", got, want)
	}
}

// TestReceiverRejectsAndDedupes sends hand-made requests to the receiver.
func TestReceiverRejectsAndDedupes(t *testing.T) {
	rs := newReceiverServer(t)
	rs.rc.Secrets = []string{"whsec_right"}
	body := []byte(`{"id":"evt_A","object":"event","type":"payment.created","data":{}}`)
	post := func(b []byte, header string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), "POST", rs.srv.URL, bytes.NewReader(b))
		req.Header.Set(webhooks.SignatureHeader, header)
		resp, err := rs.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	now := time.Now().Unix()
	valid := webhooks.Header(now, body, "whsec_right")

	cases := []struct {
		name   string
		body   []byte
		header string
		want   int
	}{
		{"tampered body", bytes.Replace(body, []byte("created"), []byte("refunded"), 1), valid, 400},
		{"wrong secret", body, webhooks.Header(now, body, "whsec_wrong"), 400},
		{"stale timestamp", body, webhooks.Header(now-int64(6*time.Minute/time.Second), body, "whsec_right"), 400},
		{"no signature", body, "", 400},
		{"valid", body, valid, 200},
		{"the same valid request replayed inside the window", body, valid, 200},
	}
	for _, c := range cases {
		if got := post(c.body, c.header); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	// The replay passed verification (it is a genuine signature) but was
	// deduped: the event was processed exactly once.
	if got := rs.processed(t); !slices.Equal(got, []string{"evt_A"}) {
		t.Fatalf("processed = %v, want [evt_A]", got)
	}
}

func TestDispatchRetriesAfter500ThenSucceeds(t *testing.T) {
	e := newAPIEnv(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	e.createEndpoint(srv.URL)
	e.createPayment("fp_ok", 100) // one event

	d := webhooks.NewDispatcher(e.db, fastDispatch(), quietLog())
	if _, err := d.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM webhook_deliveries WHERE status = 'pending' AND attempts = 1 AND last_status = 500`); n != 1 {
		t.Fatal("after a 500 the delivery should be pending with 1 attempt and last_status 500")
	}
	e.drain(d, 5*time.Second)
	if n := e.count(`SELECT count(*) FROM webhook_deliveries WHERE status = 'delivered' AND attempts = 2`); n != 1 {
		t.Fatalf("delivery not delivered on attempt 2 (server saw %d calls)", calls.Load())
	}
}

func TestDispatchReceiverTimeout(t *testing.T) {
	e := newAPIEnv(t)
	var slow atomic.Bool
	slow.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow.Load() {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	e.createEndpoint(srv.URL)
	e.createPayment("fp_ok", 100)

	cfg := fastDispatch()
	cfg.HTTPTimeout = 100 * time.Millisecond
	d := webhooks.NewDispatcher(e.db, cfg, quietLog())
	if _, err := d.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM webhook_deliveries WHERE status = 'pending' AND last_error LIKE '%deadline exceeded%'`); n != 1 {
		t.Fatal("a timed-out attempt should leave the delivery pending with a timeout error")
	}
	slow.Store(false)
	e.drain(d, 5*time.Second)
	if n := e.count(`SELECT count(*) FROM webhook_deliveries WHERE status = 'delivered' AND last_status = 204`); n != 1 {
		t.Fatal("delivery not delivered after the receiver recovered")
	}
}

func TestDispatchGivesUpThenRedelivers(t *testing.T) {
	e := newAPIEnv(t)
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	e.createEndpoint(srv.URL)
	e.createPayment("fp_ok", 100)

	cfg := fastDispatch()
	cfg.MaxAttempts = 3
	d := webhooks.NewDispatcher(e.db, cfg, quietLog())
	e.drain(d, 5*time.Second) // "drained" once the delivery is dead
	if n := e.count(`SELECT count(*) FROM webhook_deliveries WHERE status = 'dead' AND attempts = 3 AND last_status = 502`); n != 1 {
		t.Fatal("delivery should be dead after 3 attempts")
	}

	ev := e.eventIDs()[0]
	healthy.Store(true)
	wantStatus(t, e.post("/v1/events/"+ev+"/redeliver", ""), 200)
	e.drain(d, 5*time.Second)
	if n := e.count(`SELECT count(*) FROM webhook_deliveries WHERE status = 'delivered' AND attempts = 1`); n != 1 {
		t.Fatal("redelivered event not delivered with a fresh attempt count")
	}
}

func TestDispatchSignsWithBothSecretsDuringRotation(t *testing.T) {
	e := newAPIEnv(t)
	onOld, onNew := newReceiverServer(t), newReceiverServer(t)
	// Two endpoints so each receiver gets its own copy of every event.
	epOld := e.createEndpoint(onOld.srv.URL)
	epNew := e.createEndpoint(onNew.srv.URL)
	onOld.rc.Secrets = []string{epOld.Secret} // has not picked up the rotation yet
	rotated := decode[endpointResp](t, e.post("/v1/webhook_endpoints/"+epNew.ID+"/rotate_secret", ""))
	onNew.rc.Secrets = []string{rotated.Secret} // already switched to the new secret
	// epOld rotates too, but its receiver still only knows the previous secret.
	wantStatus(t, e.post("/v1/webhook_endpoints/"+epOld.ID+"/rotate_secret", ""), 200)

	e.createPayment("fp_ok", 100)
	e.drain(webhooks.NewDispatcher(e.db, fastDispatch(), quietLog()), 5*time.Second)
	if n := e.count(`SELECT count(*) FROM webhook_deliveries WHERE status = 'delivered'`); n != 2 {
		t.Fatalf("delivered %d of 2: a receiver on either secret must accept during rotation", n)
	}
}

// TestDispatchManyDispatchersNoDoubleSend: four dispatchers drain 120 events
// at once. SKIP LOCKED and the lease mean every delivery is claimed by
// exactly one of them, so with no crashes each event is sent exactly once.
func TestDispatchManyDispatchersNoDoubleSend(t *testing.T) {
	e := newAPIEnv(t)
	var mu sync.Mutex
	sends := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sends[r.Header.Get("Ledgerline-Event-Id")]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	e.createEndpoint(srv.URL)
	for range 40 {
		e.paymentIn(payments.StatusAuthorized) // 2 events each
	}
	for range 20 {
		e.paymentIn(payments.StatusFailed) // 2 events each
	}
	total := e.count(`SELECT count(*) FROM webhook_deliveries`)

	cfg := fastDispatch()
	cfg.BatchSize = 7 // small batches so the dispatchers interleave
	var wg sync.WaitGroup
	for range 4 {
		d := webhooks.NewDispatcher(e.db, cfg, quietLog())
		wg.Go(func() {
			for e.count(`SELECT count(*) FROM webhook_deliveries WHERE status = 'pending'`) > 0 {
				if _, err := d.RunOnce(t.Context()); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()

	if len(sends) != total {
		t.Fatalf("%d distinct events sent, want %d", len(sends), total)
	}
	for ev, n := range sends {
		if n != 1 {
			t.Fatalf("event %s sent %d times", ev, n)
		}
	}
}

// TestDispatchExpiredLeaseResendsAndReceiverDedupes: a dispatcher that takes
// longer than its lease (as a hung or crashed one would) loses the delivery
// to another dispatcher, so the event is sent twice. At-least-once. The
// receiver processes it once.
func TestDispatchExpiredLeaseResendsAndReceiverDedupes(t *testing.T) {
	e := newAPIEnv(t)
	rs := newReceiverServer(t)
	var arrivals atomic.Int32
	slowFirst := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if arrivals.Add(1) == 1 {
			time.Sleep(400 * time.Millisecond) // longer than the lease
		}
		rs.rc.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(slowFirst)
	t.Cleanup(srv.Close)
	ep := e.createEndpoint(srv.URL)
	rs.rc.Secrets = []string{ep.Secret}
	e.createPayment("fp_ok", 100)

	cfg := fastDispatch()
	cfg.Lease = 100 * time.Millisecond
	first := webhooks.NewDispatcher(e.db, cfg, quietLog())
	second := webhooks.NewDispatcher(e.db, cfg, quietLog())
	done := make(chan struct{})
	go func() { _, _ = first.RunOnce(t.Context()); close(done) }()
	time.Sleep(200 * time.Millisecond) // the first dispatcher's lease has expired
	if n, err := second.RunOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("second dispatcher claimed %d (err %v), want the expired lease", n, err)
	}
	<-done

	if arrivals.Load() != 2 {
		t.Fatalf("receiver saw %d sends, want 2", arrivals.Load())
	}
	if got := rs.processed(t); len(got) != 1 {
		t.Fatalf("receiver processed %v, want the event exactly once", got)
	}
	if n := e.count(`SELECT count(*) FROM webhook_deliveries WHERE status = 'delivered'`); n != 1 {
		t.Fatal("delivery not marked delivered")
	}
}
