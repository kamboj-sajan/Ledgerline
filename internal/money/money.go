package money

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"
)

var (
	// ErrCurrencyMismatch is returned by arithmetic and comparison across currencies.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	// ErrOverflow is returned when a result does not fit in int64 minor units.
	ErrOverflow = errors.New("money: amount overflows int64")
	// ErrInvalidAmount is returned for malformed amounts (bad decimal strings,
	// non-integer JSON, negative rates).
	ErrInvalidAmount = errors.New("money: invalid amount")
)

// Money is an amount in a currency's minor unit (cents for USD, yen for JPY,
// fils for BHD). It is a small immutable value type: pass it by value, and
// every operation returns a new Money.
//
// Money values are comparable with ==, which compares amount and currency.
type Money struct {
	amount   int64
	currency Currency
}

// New returns amount minor units of c.
//
// It panics if c is the zero Currency. That can only happen through a
// programming error (writing Currency{} by hand); untrusted input reaches a
// Currency through ParseCurrency, which returns an error instead.
func New(amount int64, c Currency) Money {
	if c.IsZero() {
		panic("money: New called with zero Currency")
	}
	return Money{amount: amount, currency: c}
}

// Zero returns a zero amount of c.
func Zero(c Currency) Money { return New(0, c) }

// Amount returns the amount in minor units.
func (m Money) Amount() int64 { return m.amount }

// Currency returns the currency.
func (m Money) Currency() Currency { return m.currency }

// IsZero reports whether the amount is zero.
func (m Money) IsZero() bool { return m.amount == 0 }

// Sign returns -1, 0 or +1.
func (m Money) Sign() int {
	switch {
	case m.amount < 0:
		return -1
	case m.amount > 0:
		return 1
	default:
		return 0
	}
}

func (m Money) checkSameCurrency(o Money) error {
	if m.currency != o.currency {
		return fmt.Errorf("%w: %s and %s", ErrCurrencyMismatch, m.currency.code, o.currency.code)
	}
	return nil
}

// Add returns m + o. It fails on a currency mismatch or int64 overflow.
func (m Money) Add(o Money) (Money, error) {
	if err := m.checkSameCurrency(o); err != nil {
		return Money{}, err
	}
	a, b := m.amount, o.amount
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return Money{}, fmt.Errorf("%w: %d + %d", ErrOverflow, a, b)
	}
	return Money{amount: a + b, currency: m.currency}, nil
}

// Sub returns m - o. It fails on a currency mismatch or int64 overflow.
func (m Money) Sub(o Money) (Money, error) {
	if err := m.checkSameCurrency(o); err != nil {
		return Money{}, err
	}
	a, b := m.amount, o.amount
	if (b < 0 && a > math.MaxInt64+b) || (b > 0 && a < math.MinInt64+b) {
		return Money{}, fmt.Errorf("%w: %d - %d", ErrOverflow, a, b)
	}
	return Money{amount: a - b, currency: m.currency}, nil
}

// Neg returns -m. It fails only for math.MinInt64, whose negation does not fit.
func (m Money) Neg() (Money, error) {
	if m.amount == math.MinInt64 {
		return Money{}, fmt.Errorf("%w: -(%d)", ErrOverflow, m.amount)
	}
	return Money{amount: -m.amount, currency: m.currency}, nil
}

// Cmp returns -1, 0 or +1 as m is less than, equal to or greater than o.
// Comparing different currencies is an error, not an ordering.
func (m Money) Cmp(o Money) (int, error) {
	if err := m.checkSameCurrency(o); err != nil {
		return 0, err
	}
	switch {
	case m.amount < o.amount:
		return -1, nil
	case m.amount > o.amount:
		return 1, nil
	default:
		return 0, nil
	}
}

// Sum adds amounts of currency c. An empty list sums to Zero(c).
func Sum(c Currency, ms ...Money) (Money, error) {
	total := Zero(c)
	for _, m := range ms {
		var err error
		if total, err = total.Add(m); err != nil {
			return Money{}, err
		}
	}
	return total, nil
}

const basisPointsPerUnit = 10_000

// MulBasisPoints returns m × bps / 10 000, rounded half away from zero.
// 1 basis point is 0.01%, so a 2.9% fee is MulBasisPoints(290).
//
// The product is computed in 128 bits, so it cannot overflow before the
// division even when m × bps exceeds int64; only a final result outside int64
// is an error. Rounding is symmetric: -0.5 rounds to -1 just as 0.5 rounds to 1.
func (m Money) MulBasisPoints(bps int64) (Money, error) {
	if bps < 0 {
		return Money{}, fmt.Errorf("%w: negative basis points %d", ErrInvalidAmount, bps)
	}
	hi, lo := bits.Mul64(absUint64(m.amount), uint64(bps))
	// Round half away from zero: add half the divisor to the magnitude, then truncate.
	lo, carry := bits.Add64(lo, basisPointsPerUnit/2, 0)
	hi += carry
	if hi >= basisPointsPerUnit {
		// The quotient would need more than 64 bits (bits.Div64 panics here).
		return Money{}, fmt.Errorf("%w: %d × %d bps", ErrOverflow, m.amount, bps)
	}
	q, _ := bits.Div64(hi, lo, basisPointsPerUnit)
	amount, ok := fromMagnitude(q, m.amount < 0)
	if !ok {
		return Money{}, fmt.Errorf("%w: %d × %d bps", ErrOverflow, m.amount, bps)
	}
	return Money{amount: amount, currency: m.currency}, nil
}

// absUint64 returns |a| as a uint64. It is exact for math.MinInt64, whose
// magnitude (2^63) does not fit in int64.
func absUint64(a int64) uint64 {
	if a >= 0 {
		return uint64(a)
	}
	return uint64(-(a + 1)) + 1
}

// fromMagnitude is the inverse of absUint64. It reports false if the signed
// value does not fit in int64.
func fromMagnitude(mag uint64, negative bool) (int64, bool) {
	const minInt64Magnitude = uint64(math.MaxInt64) + 1
	switch {
	case !negative && mag <= math.MaxInt64:
		return int64(mag), true
	case negative && mag < minInt64Magnitude:
		return -int64(mag), true
	case negative && mag == minInt64Magnitude:
		return math.MinInt64, true
	default:
		return 0, false
	}
}

// FormatMajor renders the amount in major units with exactly Exponent()
// decimal places: 1234 USD is "12.34", 1234 JPY is "1234", -5 BHD is "-0.005".
func (m Money) FormatMajor() string {
	digits := strconv.FormatUint(absUint64(m.amount), 10)
	if exp := m.currency.exponent; exp > 0 {
		if len(digits) <= exp {
			digits = strings.Repeat("0", exp-len(digits)+1) + digits
		}
		digits = digits[:len(digits)-exp] + "." + digits[len(digits)-exp:]
	}
	if m.amount < 0 {
		return "-" + digits
	}
	return digits
}

// String implements fmt.Stringer, e.g. "12.34 USD". It is for logs and
// humans; APIs carry the integer amount and the code separately.
func (m Money) String() string {
	return m.FormatMajor() + " " + m.currency.code
}

// ParseMajor parses a decimal string in major units, such as "12.34", into
// Money of currency c, exactly and without floating point.
//
// Accepted: an optional leading '-', at least one integer digit, and at most
// c.Exponent() fraction digits ("12", "12.3" and "12.34" are all fine for USD;
// "12.345", ".5", "5.", "+5", "1e3" and "1,000" are not).
func ParseMajor(s string, c Currency) (Money, error) {
	if c.IsZero() {
		return Money{}, fmt.Errorf("%w: zero Currency", ErrUnknownCurrency)
	}
	invalid := fmt.Errorf("%w: %q is not a valid %s amount", ErrInvalidAmount, s, c.code)

	unsigned, negative := strings.CutPrefix(s, "-")
	intPart, fracPart, hasDot := strings.Cut(unsigned, ".")
	if intPart == "" || (hasDot && fracPart == "") || len(fracPart) > c.exponent {
		return Money{}, invalid
	}

	// Read integer and fraction digits as one stream of minor units, padding
	// the fraction with zeros: "12.3" in USD reads as the digits 1,2,3,0.
	digits := intPart + fracPart + strings.Repeat("0", c.exponent-len(fracPart))
	limit := uint64(math.MaxInt64)
	if negative {
		limit++ // |math.MinInt64| is one more than math.MaxInt64
	}
	var mag uint64
	for i := range len(digits) {
		ch := digits[i]
		if ch < '0' || ch > '9' {
			return Money{}, invalid
		}
		d := uint64(ch - '0')
		if mag > (limit-d)/10 {
			return Money{}, fmt.Errorf("%w: %q", ErrOverflow, s)
		}
		mag = mag*10 + d
	}
	amount, _ := fromMagnitude(mag, negative) // mag <= limit, so this always fits
	return Money{amount: amount, currency: c}, nil
}

// moneyJSON is the wire form. Pointer fields let UnmarshalJSON tell a missing
// field apart from an explicit zero.
type moneyJSON struct {
	Amount   *int64    `json:"amount"`
	Currency *Currency `json:"currency"`
}

// MarshalJSON implements json.Marshaler: {"amount":1234,"currency":"USD"}.
// The amount is a JSON integer, never a float or a decimal string.
func (m Money) MarshalJSON() ([]byte, error) {
	if m.currency.IsZero() {
		return nil, errors.New("money: cannot marshal Money with zero Currency")
	}
	return json.Marshal(moneyJSON{Amount: &m.amount, Currency: &m.currency})
}

// UnmarshalJSON implements json.Unmarshaler. It is strict: both fields are
// required, unknown fields are rejected, and the amount must be a JSON integer
// that fits in int64 (1234.0, 1e3 and "1234" are all rejected). Unlike the
// usual Go convention, JSON null is an error rather than a no-op, because a
// missing amount must never silently become zero.
func (m *Money) UnmarshalJSON(b []byte) error {
	var raw moneyJSON
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidAmount, err)
	}
	if raw.Amount == nil || raw.Currency == nil {
		return fmt.Errorf("%w: both amount and currency are required", ErrInvalidAmount)
	}
	*m = Money{amount: *raw.Amount, currency: *raw.Currency}
	return nil
}
