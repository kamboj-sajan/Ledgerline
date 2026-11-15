package acquirer

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kamboj-sajan/ledgerline/internal/id"
)

// Config sets the simulator's behaviour. Rates are probabilities in [0, 1].
type Config struct {
	Latency     time.Duration // base delay of every call
	Jitter      time.Duration // extra uniformly random delay in [0, Jitter)
	DeclineRate float64       // share of new authorizations that are declined
	TimeoutRate float64       // share of new authorizations whose call then times out
	ErrorRate   float64       // share of calls rejected with ErrUnavailable before processing
	Seed        uint64        // 0 picks a random seed
}

// Simulator is a fake card network backed by the acquirer schema. It commits
// its own transactions, independently of the caller's, like a remote system.
type Simulator struct {
	db  *pgxpool.Pool
	cfg Config

	mu  sync.Mutex
	rng *rand.Rand
}

// NewSimulator returns a simulator that stores its state in db.
func NewSimulator(db *pgxpool.Pool, cfg Config) *Simulator {
	seed := cfg.Seed
	if seed == 0 {
		seed = rand.Uint64() //nolint:gosec // simulation randomness, not security
	}
	return &Simulator{db: db, cfg: cfg, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))} //nolint:gosec // simulation randomness, not security
}

type rolls struct {
	delay                 time.Duration
	err, decline, timeout float64
}

func (s *Simulator) roll() rolls {
	s.mu.Lock() // *rand.Rand is not safe for concurrent use
	defer s.mu.Unlock()
	r := rolls{delay: s.cfg.Latency, err: s.rng.Float64(), decline: s.rng.Float64(), timeout: s.rng.Float64()}
	if s.cfg.Jitter > 0 {
		r.delay += time.Duration(s.rng.Int64N(int64(s.cfg.Jitter)))
	}
	return r
}

// Authorize implements Acquirer.
//
// The order mirrors a real network: wait (latency), maybe refuse outright
// (ErrUnavailable, nothing recorded), otherwise record the decision
// idempotently and commit, then maybe fail to deliver the answer
// (ErrTimeout, decision already recorded). A replay of a recorded key returns
// the recorded decision.
func (s *Simulator) Authorize(ctx context.Context, req Request) (Result, error) {
	r := s.roll()
	if err := sleep(ctx, r.delay); err != nil {
		return Result{}, err
	}
	if req.CardFingerprint == CardUnavailable || r.err < s.cfg.ErrorRate {
		s.logCall(ctx, req.IdempotencyKey, "unavailable")
		return Result{}, ErrUnavailable
	}

	decision := Result{Approved: true, Reference: id.New("auth")}
	switch {
	case req.CardFingerprint == CardDecline:
		decision = Result{DeclineCode: "card_declined", DeclineMessage: "the card was declined"}
	case req.CardFingerprint == CardInsufficientFunds:
		decision = Result{DeclineCode: "insufficient_funds", DeclineMessage: "the card has insufficient funds"}
	case r.decline < s.cfg.DeclineRate:
		decision = Result{DeclineCode: "do_not_honor", DeclineMessage: "the issuer declined the authorization"}
	}

	stored, created, err := s.record(ctx, req, decision)
	if err != nil {
		return Result{}, err
	}
	if created && (req.CardFingerprint == CardTimeout || r.timeout < s.cfg.TimeoutRate) {
		s.logCall(ctx, req.IdempotencyKey, "timeout")
		return Result{}, ErrTimeout
	}
	return stored, nil
}

// record stores decision under the request's key, or returns what an earlier
// call stored. created reports whether this call made the decision.
func (s *Simulator) record(ctx context.Context, req Request, decision Result) (stored Result, created bool, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Result{}, false, fmt.Errorf("acquirer: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		INSERT INTO acquirer.authorizations
			(idempotency_key, payment_reference, amount, currency, card_fingerprint, approved, reference, decline_code)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		req.IdempotencyKey, req.PaymentReference, req.Amount.Amount(), req.Amount.Currency().Code(),
		req.CardFingerprint, decision.Approved, nullIfEmpty(decision.Reference), nullIfEmpty(decision.DeclineCode))
	if err != nil {
		return Result{}, false, fmt.Errorf("acquirer: recording authorization: %w", err)
	}
	created = tag.RowsAffected() == 1

	var (
		ref, code, currency, paymentRef, fingerprint *string
		amount                                       int64
	)
	err = tx.QueryRow(ctx, `
		SELECT approved, reference, decline_code, amount, currency, payment_reference, card_fingerprint
		FROM acquirer.authorizations WHERE idempotency_key = $1`, req.IdempotencyKey,
	).Scan(&stored.Approved, &ref, &code, &amount, &currency, &paymentRef, &fingerprint)
	if err != nil {
		return Result{}, false, fmt.Errorf("acquirer: reading authorization: %w", err)
	}
	if amount != req.Amount.Amount() || *currency != req.Amount.Currency().Code() ||
		*paymentRef != req.PaymentReference || *fingerprint != req.CardFingerprint {
		return Result{}, false, ErrKeyReused
	}
	stored.Reference, stored.DeclineCode = deref(ref), deref(code)
	if !stored.Approved {
		stored.DeclineMessage = declineMessage(stored.DeclineCode)
	}

	outcome := "replayed"
	if created {
		outcome = "declined"
		if stored.Approved {
			outcome = "approved"
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO acquirer.calls (idempotency_key, outcome) VALUES ($1, $2)`, req.IdempotencyKey, outcome); err != nil {
		return Result{}, false, fmt.Errorf("acquirer: logging call: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, false, fmt.Errorf("acquirer: %w", err)
	}
	return stored, created, nil
}

// logCall records a call that stored no decision. It is best effort: it only
// feeds test assertions, so a failure here must not change the outcome.
func (s *Simulator) logCall(ctx context.Context, key, outcome string) {
	_, _ = s.db.Exec(ctx, `INSERT INTO acquirer.calls (idempotency_key, outcome) VALUES ($1, $2)`, key, outcome)
}

func declineMessage(code string) string {
	switch code {
	case "card_declined":
		return "the card was declined"
	case "insufficient_funds":
		return "the card has insufficient funds"
	default:
		return "the issuer declined the authorization"
	}
}

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Authorizations counts the authorizations stored for a payment reference.
// Tests use it to prove that no payment was ever authorized twice.
func Authorizations(ctx context.Context, db *pgxpool.Pool, paymentReference string) (int, error) {
	var n int
	err := db.QueryRow(ctx, `SELECT count(*) FROM acquirer.authorizations WHERE payment_reference = $1`, paymentReference).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
