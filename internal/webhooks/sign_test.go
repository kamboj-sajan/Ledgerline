package webhooks_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kamboj-sajan/ledgerline/internal/webhooks"
)

func TestSignatureVerification(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"payment.captured"}`)
	now := time.Unix(1_800_000_000, 0)
	ts := now.Unix()
	good := webhooks.Header(ts, body, "whsec_a")
	tolerance := webhooks.DefaultTolerance

	tests := []struct {
		name    string
		header  string
		body    []byte
		secrets []string
		now     time.Time
		wantErr error
	}{
		{"valid", good, body, []string{"whsec_a"}, now, nil},
		{"valid, receiver also knows another secret", good, body, []string{"whsec_other", "whsec_a"}, now, nil},
		{"tampered body", good, []byte(`{"id":"evt_1","type":"payment.refunded"}`), []string{"whsec_a"}, now, webhooks.ErrBadSignature},
		{"wrong secret", good, body, []string{"whsec_b"}, now, webhooks.ErrBadSignature},
		{"timestamp just inside the window", good, body, []string{"whsec_a"}, now.Add(tolerance), nil},
		{"stale timestamp", good, body, []string{"whsec_a"}, now.Add(tolerance + time.Second), webhooks.ErrStale},
		{"timestamp from the future", good, body, []string{"whsec_a"}, now.Add(-tolerance - time.Second), webhooks.ErrStale},
		{
			// Moving the timestamp forward to dodge the staleness check
			// breaks the signature, because t is part of the signed message.
			"re-dated timestamp", strings.Replace(good, "t="+strconv.FormatInt(ts, 10), "t="+strconv.FormatInt(ts+600, 10), 1),
			body, []string{"whsec_a"}, now.Add(10 * time.Minute), webhooks.ErrBadSignature,
		},
		{"missing header", "", body, []string{"whsec_a"}, now, webhooks.ErrNoSignature},
		{"no v1", "t=" + strconv.FormatInt(ts, 10), body, []string{"whsec_a"}, now, webhooks.ErrNoSignature},
		{"bad timestamp", "t=abc,v1=00", body, []string{"whsec_a"}, now, webhooks.ErrNoSignature},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := webhooks.Verify(tt.header, tt.body, tt.secrets, tt.now, tolerance)
			if tt.wantErr == nil && err != nil {
				t.Fatalf("Verify = %v, want nil", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestSignatureDuringRotation: during a rotation the header carries one v1 per
// active secret, so receivers on the old secret and receivers already on
// the new one both accept it.
func TestSignatureDuringRotation(t *testing.T) {
	body := []byte(`{}`)
	now := time.Now()
	h := webhooks.Header(now.Unix(), body, "whsec_new", "whsec_old")
	if strings.Count(h, "v1=") != 2 {
		t.Fatalf("header %q, want two v1 signatures", h)
	}
	for _, receiverSecret := range []string{"whsec_new", "whsec_old"} {
		if _, err := webhooks.Verify(h, body, []string{receiverSecret}, now, webhooks.DefaultTolerance); err != nil {
			t.Errorf("receiver with %s: %v", receiverSecret, err)
		}
	}
	if _, err := webhooks.Verify(h, body, []string{"whsec_unrelated"}, now, webhooks.DefaultTolerance); !errors.Is(err, webhooks.ErrBadSignature) {
		t.Errorf("unrelated secret: %v", err)
	}
}

func TestSignMatchesSpec(t *testing.T) {
	// HMAC-SHA256(key "secret", message "1700000000.{}"), computed
	// independently with: printf '1700000000.{}' | openssl dgst -sha256 -hmac secret
	const want = "b8569b78799ff9e3cbff0fc2d63a33a2b57f3282abd07c37ae5e8e7d79a5f163"
	if got := webhooks.Sign("secret", 1_700_000_000, []byte("{}")); got != want {
		t.Fatalf("Sign = %s, want %s", got, want)
	}
}
