package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader is the HTTP header carrying the signature.
const SignatureHeader = "Ledgerline-Signature"

// DefaultTolerance is how old a signed timestamp may be before receivers
// reject it, which bounds how long a captured request can be replayed.
const DefaultTolerance = 5 * time.Minute

// Verification errors.
var (
	ErrNoSignature  = errors.New("webhooks: missing or malformed signature header")
	ErrBadSignature = errors.New("webhooks: no signature matches any secret")
	ErrStale        = errors.New("webhooks: signature timestamp outside the tolerance window")
)

// Sign returns hex(HMAC-SHA256(secret, "<t>.<body>")).
func Sign(secret string, t int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(t, 10)))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Header builds "t=<unix>,v1=<sig>[,v1=<sig>]", with one v1 per secret. During
// a rotation the dispatcher passes the new and the previous secret, so a
// receiver that knows either one accepts the delivery.
func Header(t int64, body []byte, secrets ...string) string {
	var b strings.Builder
	b.WriteString("t=")
	b.WriteString(strconv.FormatInt(t, 10))
	for _, s := range secrets {
		b.WriteString(",v1=")
		b.WriteString(Sign(s, t, body))
	}
	return b.String()
}

// Verify checks a signature header against the raw request body. It accepts
// the request if any v1 signature matches any of secrets (compared with
// hmac.Equal, in constant time) and the timestamp is within tolerance of now,
// in either direction. It returns the signed timestamp.
//
// The timestamp is part of the signed message, so it cannot be altered
// without breaking the signature. The tolerance therefore limits replay to a
// short window. Within that window, receivers must also dedupe on event id.
func Verify(header string, body []byte, secrets []string, now time.Time, tolerance time.Duration) (time.Time, error) {
	var (
		ts   int64
		sigs []string
		seen bool
	)
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return time.Time{}, ErrNoSignature
			}
			ts, seen = n, true
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if !seen || len(sigs) == 0 {
		return time.Time{}, ErrNoSignature
	}
	signedAt := time.Unix(ts, 0)
	if d := now.Sub(signedAt); d > tolerance || d < -tolerance {
		return signedAt, fmt.Errorf("%w: signed %s ago", ErrStale, d.Round(time.Second))
	}
	for _, secret := range secrets {
		want := []byte(Sign(secret, ts, body))
		for _, got := range sigs {
			if hmac.Equal(want, []byte(got)) {
				return signedAt, nil
			}
		}
	}
	return signedAt, ErrBadSignature
}
