package idempotency_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kamboj-sajan/ledgerline/internal/idempotency"
)

func TestCanonicalize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"sorted keys, no whitespace", `{ "b": 1,  "a": {"d": 2, "c": [3, 1]} }`, `{"a":{"c":[3,1],"d":2},"b":1}`},
		{"array order is kept", `[3,1,2]`, `[3,1,2]`},
		{"large integer is not rounded through float64", `{"amount": 12345678901234567891}`, `{"amount":12345678901234567891}`},
		{"number spelling is kept", `{"a": 100.0}`, `{"a":100.0}`},
		{"string escapes are normalised", `{"name": "café <"}`, `{"name":"café <"}`},
		{"empty body is {}", ``, `{}`},
		{"whitespace-only body is {}", " \n\t", `{}`},
		{"trailing whitespace is fine", "{\"a\":1}\n", `{"a":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := idempotency.Canonicalize([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("Canonicalize(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
	for _, bad := range []string{`{`, `{"a":1} {"b":2}`, `{"a":1} x`, `not json`} {
		if _, err := idempotency.Canonicalize([]byte(bad)); !errors.Is(err, idempotency.ErrInvalidBody) {
			t.Errorf("Canonicalize(%q) error = %v, want ErrInvalidBody", bad, err)
		}
	}
}

func TestFingerprint(t *testing.T) {
	fp := func(method, path, body string) [32]byte {
		t.Helper()
		h, err := idempotency.Fingerprint(method, path, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := fp("POST", "/v1/payments/p1/capture", `{"amount": 100}`)
	if fp("POST", "/v1/payments/p1/capture", "{\n  \"amount\":100\n}") != base {
		t.Error("formatting changed the fingerprint")
	}
	for name, other := range map[string][32]byte{
		"method": fp("PUT", "/v1/payments/p1/capture", `{"amount": 100}`),
		"path":   fp("POST", "/v1/payments/p2/capture", `{"amount": 100}`),
		"body":   fp("POST", "/v1/payments/p1/capture", `{"amount": 101}`),
	} {
		if other == base {
			t.Errorf("changing the %s did not change the fingerprint", name)
		}
	}
	// An empty body and {} are the same request.
	if fp("POST", "/x", "") != fp("POST", "/x", "{}") {
		t.Error(`empty body and {} differ`)
	}
}

func TestValidateKey(t *testing.T) {
	for _, ok := range []string{"a", "order-42/retry", strings.Repeat("k", 255)} {
		if err := idempotency.ValidateKey(ok); err != nil {
			t.Errorf("ValidateKey(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("k", 256), "has space", "tab\t", "é"} {
		if err := idempotency.ValidateKey(bad); !errors.Is(err, idempotency.ErrInvalidKey) {
			t.Errorf("ValidateKey(%q) = %v, want ErrInvalidKey", bad, err)
		}
	}
}
