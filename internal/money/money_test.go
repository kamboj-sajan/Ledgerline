package money_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/kamboj-sajan/ledgerline/internal/money"
)

func TestParseCurrency(t *testing.T) {
	tests := []struct {
		code     string
		exponent int
		wantErr  bool
	}{
		{code: "USD", exponent: 2},
		{code: "JPY", exponent: 0},
		{code: "BHD", exponent: 3},
		{code: "KWD", exponent: 3},
		{code: "KRW", exponent: 0},
		{code: "usd", wantErr: true}, // lower case is rejected, not normalised
		{code: "", wantErr: true},
		{code: "US", wantErr: true},
		{code: "USDD", wantErr: true},
		{code: "XXX", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			c, err := money.ParseCurrency(tt.code)
			if tt.wantErr {
				if !errors.Is(err, money.ErrUnknownCurrency) {
					t.Fatalf("ParseCurrency(%q) error = %v, want ErrUnknownCurrency", tt.code, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCurrency(%q) unexpected error: %v", tt.code, err)
			}
			if c.Code() != tt.code || c.Exponent() != tt.exponent {
				t.Fatalf("ParseCurrency(%q) = %s/%d, want %s/%d", tt.code, c.Code(), c.Exponent(), tt.code, tt.exponent)
			}
		})
	}
}

func TestCurrenciesSortedAndUnique(t *testing.T) {
	all := money.Currencies()
	for i := 1; i < len(all); i++ {
		if all[i-1].Code() >= all[i].Code() {
			t.Fatalf("Currencies() not strictly sorted at %d: %s >= %s", i, all[i-1], all[i])
		}
	}
	for _, c := range all {
		if got, err := money.ParseCurrency(c.Code()); err != nil || got != c {
			t.Fatalf("ParseCurrency(%s) = %v, %v; want round trip", c, got, err)
		}
	}
}

func TestNewPanicsOnZeroCurrency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with zero Currency did not panic")
		}
	}()
	money.New(1, money.Currency{})
}

func TestAddSub(t *testing.T) {
	usd := func(a int64) money.Money { return money.New(a, money.USD) }
	tests := []struct {
		name    string
		a, b    money.Money
		op      string
		want    int64
		wantErr error
	}{
		{name: "add", a: usd(150), b: usd(250), op: "+", want: 400},
		{name: "add negative", a: usd(150), b: usd(-250), op: "+", want: -100},
		{name: "add to max", a: usd(math.MaxInt64 - 1), b: usd(1), op: "+", want: math.MaxInt64},
		{name: "add past max", a: usd(math.MaxInt64), b: usd(1), op: "+", wantErr: money.ErrOverflow},
		{name: "add past min", a: usd(math.MinInt64), b: usd(-1), op: "+", wantErr: money.ErrOverflow},
		{name: "add min plus max", a: usd(math.MinInt64), b: usd(math.MaxInt64), op: "+", want: -1},
		{name: "sub", a: usd(400), b: usd(150), op: "-", want: 250},
		{name: "sub to min", a: usd(math.MinInt64 + 1), b: usd(1), op: "-", want: math.MinInt64},
		{name: "sub past min", a: usd(math.MinInt64), b: usd(1), op: "-", wantErr: money.ErrOverflow},
		{name: "sub min from zero", a: usd(0), b: usd(math.MinInt64), op: "-", wantErr: money.ErrOverflow},
		{name: "sub min from -1", a: usd(-1), b: usd(math.MinInt64), op: "-", want: math.MaxInt64},
		{name: "add currency mismatch", a: usd(1), b: money.New(1, money.EUR), op: "+", wantErr: money.ErrCurrencyMismatch},
		{name: "sub currency mismatch", a: usd(1), b: money.New(1, money.JPY), op: "-", wantErr: money.ErrCurrencyMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got money.Money
			var err error
			if tt.op == "+" {
				got, err = tt.a.Add(tt.b)
			} else {
				got, err = tt.a.Sub(tt.b)
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("%v %s %v: error = %v, want %v", tt.a, tt.op, tt.b, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v %s %v: unexpected error %v", tt.a, tt.op, tt.b, err)
			}
			if got != usd(tt.want) {
				t.Fatalf("%v %s %v = %v, want %v", tt.a, tt.op, tt.b, got, usd(tt.want))
			}
		})
	}
}

func TestNeg(t *testing.T) {
	if got, err := money.New(5, money.USD).Neg(); err != nil || got.Amount() != -5 {
		t.Fatalf("Neg(5) = %v, %v", got, err)
	}
	if got, err := money.New(math.MaxInt64, money.USD).Neg(); err != nil || got.Amount() != -math.MaxInt64 {
		t.Fatalf("Neg(MaxInt64) = %v, %v", got, err)
	}
	if _, err := money.New(math.MinInt64, money.USD).Neg(); !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("Neg(MinInt64) error = %v, want ErrOverflow", err)
	}
}

func TestCmp(t *testing.T) {
	a, b := money.New(1, money.USD), money.New(2, money.USD)
	for _, tt := range []struct {
		x, y money.Money
		want int
	}{{a, b, -1}, {b, a, 1}, {a, a, 0}} {
		if got, err := tt.x.Cmp(tt.y); err != nil || got != tt.want {
			t.Errorf("Cmp(%v, %v) = %d, %v; want %d", tt.x, tt.y, got, err, tt.want)
		}
	}
	if _, err := a.Cmp(money.New(1, money.EUR)); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Fatalf("Cmp across currencies error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestSum(t *testing.T) {
	got, err := money.Sum(money.USD, money.New(1, money.USD), money.New(2, money.USD), money.New(-4, money.USD))
	if err != nil || got != money.New(-1, money.USD) {
		t.Fatalf("Sum = %v, %v; want -0.01 USD", got, err)
	}
	if got, err := money.Sum(money.JPY); err != nil || got != money.Zero(money.JPY) {
		t.Fatalf("empty Sum = %v, %v; want 0 JPY", got, err)
	}
	if _, err := money.Sum(money.USD, money.New(1, money.EUR)); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Fatalf("Sum mismatch error = %v", err)
	}
	if _, err := money.Sum(money.USD, money.New(math.MaxInt64, money.USD), money.New(1, money.USD)); !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("Sum overflow error = %v", err)
	}
}

func TestMulBasisPoints(t *testing.T) {
	tests := []struct {
		name    string
		amount  int64
		bps     int64
		want    int64
		wantErr error
	}{
		{name: "2.9% of $100", amount: 10_000, bps: 290, want: 290},
		{name: "2.9% of $10.01 rounds 29.029 down", amount: 1001, bps: 290, want: 29},
		{name: "exact half rounds up", amount: 50, bps: 100, want: 1},        // 0.5 -> 1
		{name: "just under half rounds down", amount: 49, bps: 100, want: 0}, // 0.49 -> 0
		{name: "negative half rounds away from zero", amount: -50, bps: 100, want: -1},
		{name: "zero rate", amount: 12345, bps: 0, want: 0},
		{name: "100%", amount: 12345, bps: 10_000, want: 12345},
		{name: "200%", amount: 12345, bps: 20_000, want: 24690},
		// Exact value 267477789068788498.403 (checked with bc), so it rounds down.
		{name: "product exceeds int64 but result fits", amount: math.MaxInt64, bps: 290, want: 267_477_789_068_788_498},
		{name: "max at 100%", amount: math.MaxInt64, bps: 10_000, want: math.MaxInt64},
		{name: "min at 100%", amount: math.MinInt64, bps: 10_000, want: math.MinInt64},
		{name: "result overflows", amount: math.MaxInt64, bps: 10_001, wantErr: money.ErrOverflow},
		{name: "quotient exceeds 64 bits", amount: math.MaxInt64, bps: math.MaxInt64, wantErr: money.ErrOverflow},
		{name: "negative rate", amount: 100, bps: -1, wantErr: money.ErrInvalidAmount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := money.New(tt.amount, money.USD).MulBasisPoints(tt.bps)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Amount() != tt.want {
				t.Fatalf("MulBasisPoints(%d, %d) = %d, want %d", tt.amount, tt.bps, got.Amount(), tt.want)
			}
		})
	}
}

func TestFormatMajor(t *testing.T) {
	tests := []struct {
		m    money.Money
		want string
	}{
		{money.New(1234, money.USD), "12.34"},
		{money.New(5, money.USD), "0.05"},
		{money.New(-5, money.USD), "-0.05"},
		{money.New(0, money.USD), "0.00"},
		{money.New(1234, money.JPY), "1234"},
		{money.New(-7, money.JPY), "-7"},
		{money.New(1, money.BHD), "0.001"},
		{money.New(1234567, money.BHD), "1234.567"},
		{money.New(math.MaxInt64, money.USD), "92233720368547758.07"},
		{money.New(math.MinInt64, money.USD), "-92233720368547758.08"},
	}
	for _, tt := range tests {
		if got := tt.m.FormatMajor(); got != tt.want {
			t.Errorf("FormatMajor(%d %s) = %q, want %q", tt.m.Amount(), tt.m.Currency(), got, tt.want)
		}
	}
	if got := money.New(1234, money.USD).String(); got != "12.34 USD" {
		t.Errorf("String() = %q", got)
	}
}

func TestParseMajor(t *testing.T) {
	tests := []struct {
		in      string
		c       money.Currency
		want    int64
		wantErr error
	}{
		{in: "12.34", c: money.USD, want: 1234},
		{in: "12.3", c: money.USD, want: 1230},
		{in: "12", c: money.USD, want: 1200},
		{in: "0.05", c: money.USD, want: 5},
		{in: "-0.05", c: money.USD, want: -5},
		{in: "007.50", c: money.USD, want: 750},
		{in: "-0", c: money.USD, want: 0},
		{in: "1234", c: money.JPY, want: 1234},
		{in: "1.234", c: money.BHD, want: 1234},
		{in: "92233720368547758.07", c: money.USD, want: math.MaxInt64},
		{in: "-92233720368547758.08", c: money.USD, want: math.MinInt64},
		{in: "92233720368547758.08", c: money.USD, wantErr: money.ErrOverflow},
		{in: "-92233720368547758.09", c: money.USD, wantErr: money.ErrOverflow},
		{in: "99999999999999999999999", c: money.JPY, wantErr: money.ErrOverflow},
		{in: "12.345", c: money.USD, wantErr: money.ErrInvalidAmount}, // too many decimals
		{in: "12.5", c: money.JPY, wantErr: money.ErrInvalidAmount},   // JPY has no minor digits
		{in: "", c: money.USD, wantErr: money.ErrInvalidAmount},
		{in: "-", c: money.USD, wantErr: money.ErrInvalidAmount},
		{in: ".5", c: money.USD, wantErr: money.ErrInvalidAmount},
		{in: "5.", c: money.USD, wantErr: money.ErrInvalidAmount},
		{in: "+5", c: money.USD, wantErr: money.ErrInvalidAmount},
		{in: "--5", c: money.USD, wantErr: money.ErrInvalidAmount},
		{in: "1e3", c: money.USD, wantErr: money.ErrInvalidAmount},
		{in: "1,000", c: money.USD, wantErr: money.ErrInvalidAmount},
		{in: " 5", c: money.USD, wantErr: money.ErrInvalidAmount},
		{in: "1.2.3", c: money.BHD, wantErr: money.ErrInvalidAmount},
	}
	for _, tt := range tests {
		t.Run(tt.in+"_"+tt.c.Code(), func(t *testing.T) {
			got, err := money.ParseMajor(tt.in, tt.c)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ParseMajor(%q) error = %v, want %v", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMajor(%q) unexpected error: %v", tt.in, err)
			}
			if got != money.New(tt.want, tt.c) {
				t.Fatalf("ParseMajor(%q) = %d, want %d", tt.in, got.Amount(), tt.want)
			}
		})
	}
}

func TestJSON(t *testing.T) {
	b, err := json.Marshal(money.New(1234, money.USD))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"amount":1234,"currency":"USD"}` {
		t.Fatalf("Marshal = %s", b)
	}
	if _, err := json.Marshal(money.Money{}); err == nil {
		t.Fatal("Marshal of zero Money succeeded; want error")
	}

	var m money.Money
	if err := json.Unmarshal([]byte(`{"currency":"BHD","amount":-1}`), &m); err != nil || m != money.New(-1, money.BHD) {
		t.Fatalf("Unmarshal = %v, %v", m, err)
	}

	rejected := []string{
		`{"amount":12.5,"currency":"USD"}`,                // float
		`{"amount":1234.0,"currency":"USD"}`,              // float spelled as integer
		`{"amount":1e3,"currency":"USD"}`,                 // exponent
		`{"amount":"1234","currency":"USD"}`,              // string
		`{"amount":9223372036854775808,"currency":"USD"}`, // MaxInt64 + 1
		`{"amount":1234,"currency":"usd"}`,                // lower-case code
		`{"amount":1234,"currency":"XXX"}`,                // unknown code
		`{"amount":1234}`,                                 // missing currency
		`{"currency":"USD"}`,                              // missing amount
		`{"amount":1234,"currency":"USD","fee":1}`,        // unknown field
		`null`, // must not silently become zero
	}
	for _, in := range rejected {
		var m money.Money
		if err := json.Unmarshal([]byte(in), &m); err == nil {
			t.Errorf("Unmarshal(%s) = %v, want error", in, m)
		}
	}
}
