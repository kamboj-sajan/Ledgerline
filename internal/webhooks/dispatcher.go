package webhooks

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kamboj-sajan/ledgerline/internal/failpoint"
)

// DispatcherConfig tunes delivery.
type DispatcherConfig struct {
	BatchSize    int           // deliveries claimed per round
	Workers      int           // concurrent HTTP requests per dispatcher
	Lease        time.Duration // a claimed delivery is not due again until the lease ends
	HTTPTimeout  time.Duration // per request
	BaseDelay    time.Duration // first retry delay cap
	MaxDelay     time.Duration // retry delay cap
	MaxAttempts  int           // after this many failed attempts a delivery is dead
	PollInterval time.Duration // sleep when nothing is due
}

// DefaultDispatcherConfig: base 1s, cap 1h, 10 attempts.
func DefaultDispatcherConfig() DispatcherConfig {
	return DispatcherConfig{
		BatchSize: 50, Workers: 8, Lease: 30 * time.Second, HTTPTimeout: 10 * time.Second,
		BaseDelay: time.Second, MaxDelay: time.Hour, MaxAttempts: 10, PollInterval: 500 * time.Millisecond,
	}
}

// Dispatcher sends due deliveries. Any number of dispatchers can run at once
// against one database: each claims rows with FOR UPDATE SKIP LOCKED, so no
// two claim the same delivery at the same time.
type Dispatcher struct {
	db     *pgxpool.Pool
	client *http.Client
	cfg    DispatcherConfig
	log    *slog.Logger

	mu  sync.Mutex
	rng *rand.Rand
}

// NewDispatcher returns a Dispatcher.
func NewDispatcher(db *pgxpool.Pool, cfg DispatcherConfig, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		db: db, cfg: cfg, log: log,
		client: &http.Client{Timeout: cfg.HTTPTimeout},
		rng:    rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())), //nolint:gosec // jitter, not security
	}
}

// Backoff returns the delay before the retry that follows failed attempt
// number attempt (1-based). It uses "full jitter": a uniformly random
// duration in [0, min(maxDelay, base·2^(attempt-1))]. Spreading retries over
// the whole interval, instead of all retrying at the cap, keeps a receiver
// that recovers from an outage from being hit by every dispatcher at once.
func Backoff(attempt int, base, maxDelay time.Duration, randInt64N func(int64) int64) time.Duration {
	ceiling := maxDelay
	if shift := attempt - 1; shift < 62 && base <= maxDelay>>shift {
		ceiling = base << shift
	}
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(randInt64N(int64(ceiling) + 1))
}

func (d *Dispatcher) backoff(attempt int) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Backoff(attempt, d.cfg.BaseDelay, d.cfg.MaxDelay, d.rng.Int64N)
}

type claim struct {
	deliveryID int64
	attempt    int
	url        string
	secrets    []string
	event      Event
}

// claimDue claims up to BatchSize due deliveries in one statement: the
// locked rows are skipped by other dispatchers, and pushing next_attempt_at
// one lease into the future means they stay unclaimed after this statement
// commits. If this dispatcher dies mid-send, the lease runs out and another
// dispatcher sends the delivery again (at-least-once).
func (d *Dispatcher) claimDue(ctx context.Context) ([]claim, error) {
	rows, err := d.db.Query(ctx, `
		WITH due AS (
			SELECT id FROM webhook_deliveries
			WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY next_attempt_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED)
		UPDATE webhook_deliveries d
		SET attempts = d.attempts + 1, next_attempt_at = now() + make_interval(secs => $2)
		FROM due, webhook_endpoints e, events ev
		WHERE d.id = due.id AND e.id = d.endpoint_id AND ev.id = d.event_id
		RETURNING d.id, d.attempts, e.url, e.secret,
		          CASE WHEN e.previous_secret_expires_at > now() THEN e.previous_secret END,
		          ev.id, ev.merchant_id, ev.type, COALESCE(ev.payment_id, ''), ev.data, ev.created_at`,
		d.cfg.BatchSize, d.cfg.Lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("webhooks: claiming deliveries: %w", err)
	}
	defer rows.Close()
	var out []claim
	for rows.Next() {
		var (
			c        claim
			secret   string
			previous *string
		)
		if err := rows.Scan(&c.deliveryID, &c.attempt, &c.url, &secret, &previous,
			&c.event.ID, &c.event.MerchantID, &c.event.Type, &c.event.PaymentID, &c.event.Data, &c.event.CreatedAt); err != nil {
			return nil, fmt.Errorf("webhooks: claiming deliveries: %w", err)
		}
		c.secrets = []string{secret}
		if previous != nil {
			c.secrets = append(c.secrets, *previous)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RunOnce claims one batch and delivers it. It returns how many deliveries
// it attempted.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	batch, err := d.claimDue(ctx)
	if err != nil {
		return 0, err
	}
	sem := make(chan struct{}, max(1, d.cfg.Workers))
	var wg sync.WaitGroup
	for _, c := range batch {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			d.deliver(ctx, c)
		})
	}
	wg.Wait()
	return len(batch), nil
}

// Run delivers until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		n, err := d.RunOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			d.log.Error("dispatch round failed", "error", err)
		}
		if n == 0 || err != nil {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(d.cfg.PollInterval):
			}
		}
	}
}

func (d *Dispatcher) deliver(ctx context.Context, c claim) {
	body, err := Payload(c.event)
	if err != nil {
		d.finish(ctx, c, 0, err)
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, d.cfg.HTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.url, bytesReader(body))
	if err != nil {
		d.finish(ctx, c, 0, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Ledgerline-Webhooks/1")
	req.Header.Set("Ledgerline-Event-Id", c.event.ID)
	req.Header.Set("Ledgerline-Delivery-Attempt", strconv.Itoa(c.attempt))
	req.Header.Set(SignatureHeader, Header(time.Now().Unix(), body, c.secrets...))

	status := 0
	resp, err := d.client.Do(req)
	if err == nil {
		status = resp.StatusCode
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if status < 200 || status > 299 {
			err = fmt.Errorf("endpoint answered %d", status)
		}
	}
	failpoint.Inject("dispatch-after-send")
	d.finish(ctx, c, status, err)
}

// finish records the outcome of an attempt. Each UPDATE only touches a row
// that is still pending, so a delivery marked delivered by another
// dispatcher (after a lease expired) is never moved back.
func (d *Dispatcher) finish(ctx context.Context, c claim, status int, sendErr error) {
	// Record the outcome even if ctx was cancelled during the request.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var err error
	switch {
	case sendErr == nil:
		_, err = d.db.Exec(ctx, `
			UPDATE webhook_deliveries SET status = 'delivered', delivered_at = now(), last_status = $2, last_error = NULL
			WHERE id = $1 AND status = 'pending'`, c.deliveryID, status)
	case c.attempt >= d.cfg.MaxAttempts:
		_, err = d.db.Exec(ctx, `
			UPDATE webhook_deliveries SET status = 'dead', last_status = $2, last_error = $3
			WHERE id = $1 AND status = 'pending'`, c.deliveryID, nullStatus(status), sendErr.Error())
		d.log.Warn("delivery dead", "event", c.event.ID, "attempts", c.attempt, "error", sendErr)
	default:
		delay := d.backoff(c.attempt)
		_, err = d.db.Exec(ctx, `
			UPDATE webhook_deliveries SET next_attempt_at = now() + make_interval(secs => $4), last_status = $2, last_error = $3
			WHERE id = $1 AND status = 'pending'`, c.deliveryID, nullStatus(status), sendErr.Error(), delay.Seconds())
	}
	if err != nil {
		// The lease will expire and the delivery will be retried.
		d.log.Error("recording delivery outcome", "delivery", c.deliveryID, "error", err)
	}
}

func nullStatus(s int) *int {
	if s == 0 {
		return nil
	}
	return &s
}
