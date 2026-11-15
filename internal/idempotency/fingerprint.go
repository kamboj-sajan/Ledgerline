// Package idempotency makes POST requests safe to retry. Each request
// carries an Idempotency-Key. The first request with a key executes and its
// response is stored; later requests with the same key and the same request
// get the stored response back byte for byte, without executing again.
package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrInvalidBody is returned for a request body that is not one JSON value.
var ErrInvalidBody = errors.New("idempotency: request body is not valid JSON")

// ErrInvalidKey is returned for a missing or malformed Idempotency-Key.
var ErrInvalidKey = errors.New("idempotency: invalid Idempotency-Key")

// ValidateKey accepts 1 to 255 printable ASCII characters.
func ValidateKey(key string) error {
	if len(key) == 0 || len(key) > 255 {
		return fmt.Errorf("%w: must be 1 to 255 characters", ErrInvalidKey)
	}
	for i := range len(key) {
		if key[i] < 0x21 || key[i] > 0x7e {
			return fmt.Errorf("%w: must be printable ASCII without spaces", ErrInvalidKey)
		}
	}
	return nil
}

// Canonicalize rewrites a JSON body into one canonical form, so requests that
// differ only in formatting are recognised as the same request: object keys
// sorted, no insignificant whitespace, string escapes normalised. An empty
// body is treated as {}.
//
// Numbers are decoded with UseNumber and re-emitted exactly as written, never
// through float64, so 12345678901234567891 is not rounded into a different
// value (and 100 and 100.0 stay different, as they should: the API rejects
// the second).
func Canonicalize(body []byte) ([]byte, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return []byte("{}"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidBody, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: unexpected data after the JSON value", ErrInvalidBody)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // maps encode with sorted keys
		return nil, fmt.Errorf("%w: %w", ErrInvalidBody, err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Fingerprint identifies a request: SHA-256 of the method, the path and the
// canonical body. The key's scope (the merchant) is part of the primary key,
// not of the hash.
func Fingerprint(method, path string, body []byte) ([32]byte, error) {
	canonical, err := Canonicalize(body)
	if err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{'\n'})
	h.Write([]byte(path))
	h.Write([]byte{'\n'})
	h.Write(canonical)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}
