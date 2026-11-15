package idempotency

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kamboj-sajan/ledgerline/internal/store"
)

// TTL is how long a key is remembered.
const TTL = 24 * time.Hour

var (
	// ErrInUse means another request with this key is executing right now.
	// The client should retry after a short wait (HTTP 409 + Retry-After).
	ErrInUse = errors.New("idempotency: key is in use by a request that is still running")
	// ErrReused means the key was already used for a different request.
	ErrReused = errors.New("idempotency: key was already used for a different request")
)

// Request identifies one keyed request.
type Request struct {
	MerchantID string
	Key        string
	Method     string
	Path       string
	Hash       [32]byte // from Fingerprint
}

// Response is a stored HTTP response, replayed byte for byte.
type Response struct {
	Status int
	Body   []byte
}

// State says what Lookup found.
type State int

// Lookup states.
const (
	// StateNew: no live record. Execute the request.
	StateNew State = iota
	// StateFinished: a response is stored. Replay it.
	StateFinished
	// StateStarted: a multi-phase request started and is not running any more
	// (its lock was released or went stale). Resume it from its recovery point.
	StateStarted
)

// Record is what Lookup found for a key.
type Record struct {
	State     State
	Response  Response // for StateFinished
	PaymentID string   // for StateStarted
}

// lockKey maps (merchant, key) to the 64-bit number Postgres advisory locks
// use. Two keys can collide; the only effect is a spurious "in use" that the
// client retries through.
func lockKey(r Request) int64 {
	h := sha256.Sum256([]byte(r.MerchantID + "\x00" + r.Key))
	return int64(binary.BigEndian.Uint64(h[:8])) //nolint:gosec // reinterpreting bits, overflow is intended
}

// Lock takes a transaction-scoped advisory lock on the key without waiting,
// or returns ErrInUse if another transaction holds it. The lock is released
// when tx ends, so a request that crashes or rolls back leaves nothing behind.
// Call it before Lookup.
func Lock(ctx context.Context, tx pgx.Tx, r Request) error {
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, lockKey(r)).Scan(&ok); err != nil {
		return fmt.Errorf("idempotency: locking key: %w", err)
	}
	if !ok {
		return ErrInUse
	}
	return nil
}

// Peek is the lock-free fast path for replays. It returns the stored
// response if the key has a finished, unexpired record for this request,
// ErrReused if the record is for a different request, and ok=false
// otherwise (no record, still running, or expired). Callers then take the
// lock and Lookup.
//
// It is safe without a lock because a finished record never changes. Save
// writes 'finished' once, and the only later change is deletion after
// expiry. Without this, concurrent replays of a finished key would all
// queue on the key's advisory lock, and those that lost the try-lock would
// wrongly get 409 idempotency_key_in_use. In use is meant only for a request
// that is still running.
func Peek(ctx context.Context, db store.DB, r Request) (resp Response, ok bool, err error) {
	var (
		hash   []byte
		status int32
		body   []byte
	)
	err = db.QueryRow(ctx, `
		SELECT request_hash, response_status, response_body FROM idempotency_keys
		WHERE merchant_id = $1 AND key = $2 AND recovery_point = 'finished' AND expires_at > now()`,
		r.MerchantID, r.Key).Scan(&hash, &status, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return Response{}, false, nil
	}
	if err != nil {
		return Response{}, false, fmt.Errorf("idempotency: peeking key: %w", err)
	}
	if [32]byte(hash) != r.Hash {
		return Response{}, false, ErrReused
	}
	return Response{Status: int(status), Body: body}, true, nil
}

// Lookup reads the key's record and locks its row until tx ends.
//
// An expired record is deleted and reported as StateNew. A record for a
// different request is ErrReused. A started record whose lock is younger than
// lockTimeout belongs to a request that is still running: ErrInUse. An older
// or released one is StateStarted, to be resumed.
func Lookup(ctx context.Context, tx pgx.Tx, r Request, lockTimeout time.Duration) (Record, error) {
	var (
		hash                 []byte
		point                string
		status               *int32
		body                 []byte
		paymentID            *string
		lockedAt             *time.Time
		expired, lockIsFresh bool
	)
	err := tx.QueryRow(ctx, `
		SELECT request_hash, recovery_point, response_status, response_body, payment_id, locked_at,
		       expires_at <= now(),
		       locked_at IS NOT NULL AND locked_at > now() - make_interval(secs => $3)
		FROM idempotency_keys WHERE merchant_id = $1 AND key = $2
		FOR UPDATE`,
		r.MerchantID, r.Key, lockTimeout.Seconds(),
	).Scan(&hash, &point, &status, &body, &paymentID, &lockedAt, &expired, &lockIsFresh)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{State: StateNew}, nil
	}
	if err != nil {
		return Record{}, fmt.Errorf("idempotency: looking up key: %w", err)
	}
	if expired {
		if _, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE merchant_id = $1 AND key = $2`, r.MerchantID, r.Key); err != nil {
			return Record{}, fmt.Errorf("idempotency: deleting expired key: %w", err)
		}
		return Record{State: StateNew}, nil
	}
	if [32]byte(hash) != r.Hash {
		return Record{}, ErrReused
	}
	switch point {
	case "finished":
		return Record{State: StateFinished, Response: Response{Status: int(*status), Body: body}}, nil
	case "started":
		if lockIsFresh {
			return Record{}, ErrInUse
		}
		var pid string
		if paymentID != nil {
			pid = *paymentID
		}
		return Record{State: StateStarted, PaymentID: pid}, nil
	default:
		return Record{}, fmt.Errorf("idempotency: unknown recovery point %q", point)
	}
}

// Save stores the final response, in the caller's transaction. For a
// single-transaction endpoint this inserts the record; for a multi-phase one
// it finishes the started record and releases its lock.
func Save(ctx context.Context, tx pgx.Tx, r Request, resp Response) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (merchant_id, key, request_method, request_path, request_hash,
			recovery_point, response_status, response_body, expires_at)
		VALUES ($1, $2, $3, $4, $5, 'finished', $6, $7, now() + make_interval(secs => $8))
		ON CONFLICT (merchant_id, key) DO UPDATE SET
			recovery_point = 'finished', response_status = EXCLUDED.response_status,
			response_body = EXCLUDED.response_body, locked_at = NULL, lock_token = NULL`,
		r.MerchantID, r.Key, r.Method, r.Path, r.Hash[:], resp.Status, resp.Body, TTL.Seconds())
	if err != nil {
		return fmt.Errorf("idempotency: saving response: %w", err)
	}
	return nil
}

// Start records that a multi-phase request has begun (recovery point
// 'started') and takes the key's lock, returning the fencing token the
// request must present to finish. Call it in the transaction that holds the
// advisory lock, after Lookup returned StateNew or StateStarted.
func Start(ctx context.Context, tx pgx.Tx, r Request, paymentID string) (token string, err error) {
	token = rand.Text()
	_, err = tx.Exec(ctx, `
		INSERT INTO idempotency_keys (merchant_id, key, request_method, request_path, request_hash,
			recovery_point, locked_at, lock_token, payment_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, 'started', now(), $6, $7, now() + make_interval(secs => $8))
		ON CONFLICT (merchant_id, key) DO UPDATE SET locked_at = now(), lock_token = EXCLUDED.lock_token`,
		r.MerchantID, r.Key, r.Method, r.Path, r.Hash[:], token, paymentID, TTL.Seconds())
	if err != nil {
		return "", fmt.Errorf("idempotency: starting key: %w", err)
	}
	return token, nil
}

// Claim re-locks a started key at the start of a later phase. It waits for
// the advisory lock (a same-key request that holds it does so only briefly),
// then checks the record. If another request took over and finished, Claim
// returns that record as StateFinished. If another request holds the lock now
// (the token changed), it returns ErrInUse. Otherwise it returns StateStarted:
// the caller still owns the key.
func Claim(ctx context.Context, tx pgx.Tx, r Request, token string) (Record, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey(r)); err != nil {
		return Record{}, fmt.Errorf("idempotency: locking key: %w", err)
	}
	var (
		point        string
		status       *int32
		body         []byte
		currentToken *string
		paymentID    *string
	)
	err := tx.QueryRow(ctx, `
		SELECT recovery_point, response_status, response_body, lock_token, payment_id
		FROM idempotency_keys WHERE merchant_id = $1 AND key = $2 FOR UPDATE`,
		r.MerchantID, r.Key).Scan(&point, &status, &body, &currentToken, &paymentID)
	if err != nil {
		return Record{}, fmt.Errorf("idempotency: claiming key: %w", err)
	}
	if point == "finished" {
		return Record{State: StateFinished, Response: Response{Status: int(*status), Body: body}}, nil
	}
	if currentToken == nil || *currentToken != token {
		return Record{}, ErrInUse
	}
	var pid string
	if paymentID != nil {
		pid = *paymentID
	}
	return Record{State: StateStarted, PaymentID: pid}, nil
}

// Unlock releases a started key after a transient failure, so a retry can
// resume at once instead of waiting for the lock to go stale. It is a no-op if
// the token no longer matches.
func Unlock(ctx context.Context, db store.DB, r Request, token string) error {
	_, err := db.Exec(ctx, `
		UPDATE idempotency_keys SET locked_at = NULL, lock_token = NULL
		WHERE merchant_id = $1 AND key = $2 AND lock_token = $3 AND recovery_point = 'started'`,
		r.MerchantID, r.Key, token)
	if err != nil {
		return fmt.Errorf("idempotency: unlocking key: %w", err)
	}
	return nil
}

// Purge deletes expired keys and reports how many it removed.
func Purge(ctx context.Context, db store.DB) (int64, error) {
	tag, err := db.Exec(ctx, `DELETE FROM idempotency_keys WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("idempotency: purging: %w", err)
	}
	return tag.RowsAffected(), nil
}
