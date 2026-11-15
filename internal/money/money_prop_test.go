package money_test

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"testing"

	"pgregory.net/rapid"

	"github.com/kamboj-sajan/ledgerline/internal/money"
)

// Property tests compare the int64 implementation against math/big, which
// cannot overflow, so any disagreement is a bug in the overflow handling.

// amountGen mixes uniformly random int64 values with values near zero and
// near both limits, where overflow bugs live.
func amountGen() *rapid.Generator[int64] {
	return rapid.OneOf(
		rapid.Int64(),
		rapid.Int64Range(-1000, 1000),
		rapid.Int64Range(math.MaxInt64-1000, math.MaxInt64),
		rapid.Int64Range(math.MinInt64, math.MinInt64+1000),
	)
}

func currencyGen() *rapid.Generator[money.Currency] {
	return rapid.SampledFrom(money.Currencies())
}

func TestPropAddSubMatchBigInt(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := currencyGen().Draw(t, "currency")
		a, b := amountGen().Draw(t, "a"), amountGen().Draw(t, "b")
		x, y := money.New(a, c), money.New(b, c)

		check := func(op string, got money.Money, err error, want *big.Int) {
			if !want.IsInt64() {
				if !errors.Is(err, money.ErrOverflow) {
					t.Fatalf("%d %s %d: got %v, %v; want ErrOverflow (exact %s)", a, op, b, got, err, want)
				}
				return
			}
			if err != nil || got != money.New(want.Int64(), c) {
				t.Fatalf("%d %s %d: got %v, %v; want %s", a, op, b, got, err, want)
			}
		}
		sum, err := x.Add(y)
		check("+", sum, err, new(big.Int).Add(big.NewInt(a), big.NewInt(b)))
		diff, err := x.Sub(y)
		check("-", diff, err, new(big.Int).Sub(big.NewInt(a), big.NewInt(b)))
	})
}

func TestPropAddCommutesAndSubInverts(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := currencyGen().Draw(t, "currency")
		x, y := money.New(amountGen().Draw(t, "a"), c), money.New(amountGen().Draw(t, "b"), c)
		xy, errXY := x.Add(y)
		yx, errYX := y.Add(x)
		if (errXY == nil) != (errYX == nil) || xy != yx {
			t.Fatalf("Add not commutative: %v+%v=%v,%v but %v+%v=%v,%v", x, y, xy, errXY, y, x, yx, errYX)
		}
		if errXY == nil {
			back, err := xy.Sub(y)
			if err != nil || back != x {
				t.Fatalf("(%v + %v) - %v = %v, %v; want %v", x, y, y, back, err, x)
			}
		}
	})
}

func TestPropCrossCurrencyAlwaysFails(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c1 := currencyGen().Draw(t, "c1")
		c2 := currencyGen().Filter(func(c money.Currency) bool { return c != c1 }).Draw(t, "c2")
		x, y := money.New(amountGen().Draw(t, "a"), c1), money.New(amountGen().Draw(t, "b"), c2)
		if _, err := x.Add(y); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("Add(%v, %v) error = %v", x, y, err)
		}
		if _, err := x.Sub(y); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("Sub(%v, %v) error = %v", x, y, err)
		}
		if _, err := x.Cmp(y); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("Cmp(%v, %v) error = %v", x, y, err)
		}
	})
}

func TestPropMulBasisPointsMatchesBigInt(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := amountGen().Draw(t, "amount")
		bps := rapid.OneOf(rapid.Int64Range(0, 20_000), rapid.Int64Range(0, math.MaxInt64)).Draw(t, "bps")
		got, err := money.New(a, money.USD).MulBasisPoints(bps)

		// Exact: round(|a| × bps / 10000) half away from zero, then reapply the sign.
		mag := new(big.Int).Abs(big.NewInt(a))
		mag.Mul(mag, big.NewInt(bps))
		mag.Add(mag, big.NewInt(5_000))
		mag.Quo(mag, big.NewInt(10_000))
		if a < 0 {
			mag.Neg(mag)
		}
		if !mag.IsInt64() {
			if !errors.Is(err, money.ErrOverflow) {
				t.Fatalf("%d × %d bps = %v, %v; want ErrOverflow (exact %s)", a, bps, got, err, mag)
			}
			return
		}
		if err != nil || got.Amount() != mag.Int64() {
			t.Fatalf("%d × %d bps = %v, %v; want %s", a, bps, got, err, mag)
		}
	})
}

func TestPropFormatParseRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		m := money.New(amountGen().Draw(t, "amount"), currencyGen().Draw(t, "currency"))
		back, err := money.ParseMajor(m.FormatMajor(), m.Currency())
		if err != nil || back != m {
			t.Fatalf("ParseMajor(FormatMajor(%d %s)=%q) = %v, %v", m.Amount(), m.Currency(), m.FormatMajor(), back, err)
		}
	})
}

func TestPropJSONRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		m := money.New(amountGen().Draw(t, "amount"), currencyGen().Draw(t, "currency"))
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var back money.Money
		if err := json.Unmarshal(b, &back); err != nil || back != m {
			t.Fatalf("JSON round trip of %v via %s = %v, %v", m, b, back, err)
		}
	})
}
