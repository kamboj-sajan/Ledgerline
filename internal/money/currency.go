// Package money represents monetary amounts as int64 minor units paired with
// an ISO 4217 currency. Nothing in this package uses floating point.
package money

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrUnknownCurrency is returned for a code that is not in the supported table.
var ErrUnknownCurrency = errors.New("money: unknown currency")

// Currency is an ISO 4217 currency and its minor-unit exponent.
//
// The zero value is not a valid currency. Get one from ParseCurrency or from
// a package-level variable such as USD; those are the only constructors, so a
// non-zero Currency always has the correct exponent for its code.
type Currency struct {
	code     string
	exponent int
}

// Commonly used currencies. The exponent is the number of digits after the
// decimal point: 1 USD = 100 cents, 1 JPY = 1 yen, 1 BHD = 1000 fils.
var (
	USD = Currency{"USD", 2}
	EUR = Currency{"EUR", 2}
	GBP = Currency{"GBP", 2}
	JPY = Currency{"JPY", 0}
	KRW = Currency{"KRW", 0}
	BHD = Currency{"BHD", 3}
	KWD = Currency{"KWD", 3}
)

// supported is the full table. It is deliberately a curated subset of ISO 4217
// covering all three exponents in use (0, 2 and 3) rather than all ~180 codes.
var supported = []Currency{
	USD, EUR, GBP, JPY, KRW, BHD, KWD,
	{"AED", 2}, {"AUD", 2}, {"BRL", 2}, {"CAD", 2}, {"CHF", 2}, {"CNY", 2},
	{"DKK", 2}, {"HKD", 2}, {"INR", 2}, {"MXN", 2}, {"NOK", 2}, {"NZD", 2},
	{"PLN", 2}, {"SEK", 2}, {"SGD", 2}, {"ZAR", 2},
	{"CLP", 0}, {"ISK", 0}, {"VND", 0},
	{"JOD", 3}, {"OMR", 3}, {"TND", 3},
}

var byCode = func() map[string]Currency {
	m := make(map[string]Currency, len(supported))
	for _, c := range supported {
		m[c.code] = c
	}
	return m
}()

// ParseCurrency looks up an upper-case ISO 4217 code such as "USD".
// Lower-case input is rejected rather than normalised, so a request body has
// exactly one spelling for each currency.
func ParseCurrency(code string) (Currency, error) {
	c, ok := byCode[code]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, code)
	}
	return c, nil
}

// Currencies returns every supported currency, sorted by code.
func Currencies() []Currency {
	out := slices.Clone(supported)
	slices.SortFunc(out, func(a, b Currency) int { return strings.Compare(a.code, b.code) })
	return out
}

// Code returns the ISO 4217 alphabetic code, e.g. "USD".
func (c Currency) Code() string { return c.code }

// Exponent returns the number of minor-unit digits, e.g. 2 for USD.
func (c Currency) Exponent() int { return c.exponent }

// String implements fmt.Stringer.
func (c Currency) String() string { return c.code }

// IsZero reports whether c is the (invalid) zero value.
func (c Currency) IsZero() bool { return c.code == "" }

// MarshalText implements encoding.TextMarshaler, which makes a Currency encode
// as a JSON string and work as a JSON object key.
func (c Currency) MarshalText() ([]byte, error) {
	if c.IsZero() {
		return nil, errors.New("money: cannot marshal zero Currency")
	}
	return []byte(c.code), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (c *Currency) UnmarshalText(b []byte) error {
	parsed, err := ParseCurrency(string(b))
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}
