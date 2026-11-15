package ledger_test

import (
	"errors"
	"io/fs"
	"math"
	"regexp"
	"slices"
	"testing"

	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/money"
	"github.com/kamboj-sajan/ledgerline/migrations"
)

func usd(a int64) money.Money { return money.New(a, money.USD) }

// postingsOf renders postings as "account=amount" strings for compact comparisons.
func postingsOf(tx ledger.Tx) []string {
	out := make([]string, len(tx.Postings))
	for i, p := range tx.Postings {
		out[i] = p.Account.String() + "=" + p.Amount.String()
	}
	return out
}

func TestCapture(t *testing.T) {
	tests := []struct {
		name    string
		amount  money.Money
		fee     money.Money
		want    []string
		wantErr error
	}{
		{
			name: "100.00 with 3.20 fee", amount: usd(10_000), fee: usd(320),
			want: []string{
				"platform/processor_receivable/USD=100.00 USD",
				"m1/merchant_payable/USD=-96.80 USD",
				"platform/fee_revenue/USD=-3.20 USD",
			},
		},
		{
			name: "zero fee omits the fee line", amount: usd(500), fee: usd(0),
			want: []string{"platform/processor_receivable/USD=5.00 USD", "m1/merchant_payable/USD=-5.00 USD"},
		},
		{
			name: "fee equal to amount omits the merchant line", amount: usd(30), fee: usd(30),
			want: []string{"platform/processor_receivable/USD=0.30 USD", "platform/fee_revenue/USD=-0.30 USD"},
		},
		{
			name: "zero-decimal currency", amount: money.New(1000, money.JPY), fee: money.New(36, money.JPY),
			want: []string{
				"platform/processor_receivable/JPY=1000 JPY",
				"m1/merchant_payable/JPY=-964 JPY",
				"platform/fee_revenue/JPY=-36 JPY",
			},
		},
		{name: "zero amount", amount: usd(0), fee: usd(0), wantErr: ledger.ErrInvalidAmount},
		{name: "negative amount", amount: usd(-1), fee: usd(0), wantErr: ledger.ErrInvalidAmount},
		{name: "negative fee", amount: usd(100), fee: usd(-1), wantErr: ledger.ErrInvalidAmount},
		{name: "fee above amount", amount: usd(100), fee: usd(101), wantErr: ledger.ErrInvalidAmount},
		{name: "fee in another currency", amount: usd(100), fee: money.New(1, money.EUR), wantErr: ledger.ErrInvalidAmount},
		{name: "zero Money", amount: money.Money{}, fee: usd(0), wantErr: ledger.ErrInvalidAmount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := ledger.Capture("pay_1", "m1", tt.amount, tt.fee)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tx.Kind != ledger.KindCapture || tx.PaymentID != "pay_1" {
				t.Fatalf("kind/payment = %s/%s", tx.Kind, tx.PaymentID)
			}
			if got := postingsOf(tx); !slices.Equal(got, tt.want) {
				t.Fatalf("postings =\n%q\nwant\n%q", got, tt.want)
			}
			if err := tx.Validate(); err != nil {
				t.Fatalf("Capture produced an invalid transaction: %v", err)
			}
		})
	}
}

func TestRefund(t *testing.T) {
	tx, err := ledger.Refund("pay_1", "m1", usd(2_500))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"m1/merchant_payable/USD=25.00 USD", "platform/processor_receivable/USD=-25.00 USD"}
	if got := postingsOf(tx); !slices.Equal(got, want) {
		t.Fatalf("postings = %q, want %q", got, want)
	}
	if err := tx.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []money.Money{usd(0), usd(-5), {}} {
		if _, err := ledger.Refund("pay_1", "m1", bad); !errors.Is(err, ledger.ErrInvalidAmount) {
			t.Errorf("Refund(%v) error = %v, want ErrInvalidAmount", bad, err)
		}
	}
}

func TestSettlement(t *testing.T) {
	tests := []struct {
		name    string
		ref     string
		gross   money.Money
		fee     money.Money
		want    []string
		wantErr error
	}{
		{
			name: "with network fee", ref: "bank-1", gross: usd(10_000), fee: usd(50),
			want: []string{
				"platform/bank_cash/USD=99.50 USD",
				"platform/network_fee_expense/USD=0.50 USD",
				"platform/processor_receivable/USD=-100.00 USD",
			},
		},
		{
			name: "no network fee", ref: "bank-1", gross: usd(10_000), fee: usd(0),
			want: []string{"platform/bank_cash/USD=100.00 USD", "platform/processor_receivable/USD=-100.00 USD"},
		},
		{name: "missing reference", ref: "", gross: usd(100), fee: usd(0), wantErr: ledger.ErrInvalidAmount},
		{name: "fee above gross", ref: "bank-1", gross: usd(100), fee: usd(101), wantErr: ledger.ErrInvalidAmount},
		{name: "zero gross", ref: "bank-1", gross: usd(0), fee: usd(0), wantErr: ledger.ErrInvalidAmount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := ledger.Settlement("pay_1", tt.ref, tt.gross, tt.fee)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := postingsOf(tx); !slices.Equal(got, tt.want) {
				t.Fatalf("postings = %q, want %q", got, tt.want)
			}
			if tx.ExternalRef != tt.ref {
				t.Fatalf("ExternalRef = %q", tx.ExternalRef)
			}
			if err := tx.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReverse(t *testing.T) {
	capture, err := ledger.Capture("pay_1", "m1", usd(10_000), usd(320))
	if err != nil {
		t.Fatal(err)
	}
	rev, err := ledger.Reverse("jtx_1", capture)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"platform/processor_receivable/USD=-100.00 USD",
		"m1/merchant_payable/USD=96.80 USD",
		"platform/fee_revenue/USD=3.20 USD",
	}
	if got := postingsOf(rev); !slices.Equal(got, want) {
		t.Fatalf("postings = %q, want %q", got, want)
	}
	if rev.Kind != ledger.KindReversal || rev.ReversesID != "jtx_1" || rev.PaymentID != "pay_1" || rev.ExternalRef != "" {
		t.Fatalf("reversal metadata = %+v", rev)
	}
	if err := rev.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reverse("jtx_2", rev); !errors.Is(err, ledger.ErrInvalidKind) {
		t.Fatalf("reversing a reversal: error = %v, want ErrInvalidKind", err)
	}
	if _, err := ledger.Reverse("", capture); !errors.Is(err, ledger.ErrUnknownTransaction) {
		t.Fatalf("reversing without an ID: error = %v", err)
	}
}

func TestValidate(t *testing.T) {
	recv, pay := ledger.Receivable(money.USD), ledger.Payable("m1", money.USD)
	p := func(k ledger.AccountKey, m money.Money) ledger.Posting { return ledger.Posting{Account: k, Amount: m} }
	tx := func(ps ...ledger.Posting) ledger.Tx { return ledger.Tx{Kind: ledger.KindCapture, Postings: ps} }

	tests := []struct {
		name    string
		tx      ledger.Tx
		wantErr error // nil means valid
	}{
		{"balanced", tx(p(recv, usd(5)), p(pay, usd(-5))), nil},
		{
			"balanced per currency",
			tx(p(recv, usd(5)), p(pay, usd(-5)),
				p(ledger.Receivable(money.EUR), money.New(2, money.EUR)), p(ledger.Payable("m1", money.EUR), money.New(-2, money.EUR))),
			nil,
		},
		{
			"debits totalling exactly MaxInt64",
			tx(p(recv, usd(math.MaxInt64-1)), p(pay, usd(-math.MaxInt64)), p(recv, usd(1))),
			nil,
		},
		{
			// Balanced, but the debit total exceeds int64. Go rejects it;
			// Postgres (summing in numeric) would accept it. See Validate.
			"debits totalling more than MaxInt64",
			tx(p(recv, usd(math.MaxInt64)), p(pay, usd(-math.MaxInt64)), p(recv, usd(1)), p(pay, usd(-1))),
			ledger.ErrTooLarge,
		},
		{"no postings", tx(), ledger.ErrTooFewPostings},
		{"one posting", tx(p(recv, usd(5))), ledger.ErrTooFewPostings},
		{"unbalanced", tx(p(recv, usd(5)), p(pay, usd(-4))), ledger.ErrUnbalanced},
		{"two debits", tx(p(recv, usd(5)), p(pay, usd(5))), ledger.ErrUnbalanced},
		{
			"zero overall but not per currency",
			tx(p(recv, usd(5)), p(ledger.Payable("m1", money.EUR), money.New(-5, money.EUR))),
			ledger.ErrUnbalanced,
		},
		{"zero posting", tx(p(recv, usd(0)), p(pay, usd(0))), ledger.ErrZeroPosting},
		{"posting currency differs from account", tx(p(recv, money.New(5, money.EUR)), p(pay, usd(-5))), ledger.ErrAccountCurrency},
		{
			"unknown account code",
			tx(p(ledger.AccountKey{Code: "suspense", Currency: money.USD}, usd(5)), p(pay, usd(-5))),
			ledger.ErrUnknownAccount,
		},
		{"merchant account without merchant", tx(p(recv, usd(5)), p(ledger.Payable("", money.USD), usd(-5))), ledger.ErrUnknownAccount},
		{
			"platform account with merchant",
			tx(p(ledger.AccountKey{MerchantID: "m1", Code: ledger.FeeRevenue, Currency: money.USD}, usd(5)), p(pay, usd(-5))),
			ledger.ErrUnknownAccount,
		},
		{"debits overflow", tx(p(recv, usd(math.MaxInt64)), p(recv, usd(1)), p(pay, usd(-1))), ledger.ErrTooLarge},
		{"unknown kind", ledger.Tx{Kind: "gift", Postings: []ledger.Posting{p(recv, usd(5)), p(pay, usd(-5))}}, ledger.ErrInvalidKind},
		{"reversal without target", ledger.Tx{Kind: ledger.KindReversal, Postings: []ledger.Posting{p(recv, usd(5)), p(pay, usd(-5))}}, ledger.ErrInvalidKind},
		{"target on a non-reversal", ledger.Tx{Kind: ledger.KindCapture, ReversesID: "x", Postings: []ledger.Posting{p(recv, usd(5)), p(pay, usd(-5))}}, ledger.ErrInvalidKind},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.tx.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestNormalBalance(t *testing.T) {
	tests := []struct {
		typ    ledger.AccountType
		signed int64
		want   int64
	}{
		{ledger.Asset, 100, 100},
		{ledger.Expense, 100, 100},
		{ledger.Liability, -9_680, 9_680},
		{ledger.Revenue, -320, 320},
		{ledger.Liability, 320, -320}, // a merchant that owes us money
	}
	for _, tt := range tests {
		got, err := ledger.NormalBalance(tt.typ, usd(tt.signed))
		if err != nil || got != usd(tt.want) {
			t.Errorf("NormalBalance(%s, %d) = %v, %v; want %d", tt.typ, tt.signed, got, err, tt.want)
		}
	}
	if _, err := ledger.NormalBalance("equity", usd(1)); !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Errorf("unknown type: error = %v", err)
	}
}

func TestMemoryUniquenessRules(t *testing.T) {
	l := ledger.NewMemory()
	capture, _ := ledger.Capture("pay_1", "m1", usd(10_000), usd(320))
	captureID, err := l.Post(capture)
	if err != nil {
		t.Fatal(err)
	}

	settle, _ := ledger.Settlement("pay_1", "bank-1", usd(10_000), usd(0))
	if _, err := l.Post(settle); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Post(settle); !errors.Is(err, ledger.ErrDuplicateExternalRef) {
		t.Fatalf("second settlement: error = %v, want ErrDuplicateExternalRef", err)
	}

	rev, _ := ledger.Reverse(captureID, capture)
	if _, err := l.Post(rev); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Post(rev); !errors.Is(err, ledger.ErrAlreadyReversed) {
		t.Fatalf("second reversal: error = %v, want ErrAlreadyReversed", err)
	}
	ghost, _ := ledger.Reverse("mem_999", capture)
	if _, err := l.Post(ghost); !errors.Is(err, ledger.ErrUnknownTransaction) {
		t.Fatalf("reversal of unknown transaction: error = %v", err)
	}
	if l.Len() != 3 {
		t.Fatalf("Len() = %d, want 3 (capture, settlement, reversal)", l.Len())
	}
}

func TestMemoryPostIsAllOrNothing(t *testing.T) {
	l := ledger.NewMemory()
	big, _ := ledger.Capture("pay_1", "m1", usd(math.MaxInt64), usd(0))
	if _, err := l.Post(big); err != nil {
		t.Fatal(err)
	}
	before := l.Balance(ledger.Payable("m2", money.USD))

	// Valid on its own, but the receivable cannot go above MaxInt64. The
	// merchant line comes second, so this checks that it is not applied.
	more, _ := ledger.Capture("pay_2", "m2", usd(1), usd(0))
	if _, err := l.Post(more); !errors.Is(err, ledger.ErrTooLarge) {
		t.Fatalf("overflowing post: error = %v, want ErrTooLarge", err)
	}
	if got := l.Balance(ledger.Payable("m2", money.USD)); got != before {
		t.Fatalf("m2 payable changed to %v after a rejected post", got)
	}
	if l.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", l.Len())
	}
}

func TestMemoryStoresACopy(t *testing.T) {
	l := ledger.NewMemory()
	tx, _ := ledger.Capture("pay_1", "m1", usd(100), usd(0))
	id, err := l.Post(tx)
	if err != nil {
		t.Fatal(err)
	}
	tx.Postings[0].Amount = usd(999) // the caller changes its slice afterwards
	stored, _ := l.Transaction(id)
	if stored.Postings[0].Amount != usd(100) {
		t.Fatal("Memory kept a reference to the caller's postings slice")
	}
}

// TestChartMatchesMigration keeps the Go chart of accounts and the SQL CHECK
// constraint from drifting apart.
func TestChartMatchesMigration(t *testing.T) {
	sql, err := fs.ReadFile(migrations.FS, "00002_ledger.sql")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\(code = '(\w+)'\s+AND type = '(\w+)'\s+AND merchant_id IS (NOT )?NULL\)`)
	matches := re.FindAllStringSubmatch(string(sql), -1)
	if len(matches) == 0 {
		t.Fatal("found no chart entries in 00002_ledger.sql; did the CHECK's format change?")
	}
	var sqlCodes []ledger.AccountCode
	for _, m := range matches {
		code := ledger.AccountCode(m[1])
		sqlCodes = append(sqlCodes, code)
		if got := string(code.Type()); got != m[2] {
			t.Errorf("%s: Go type %q, SQL type %q", code, got, m[2])
		}
		if got, want := code.PerMerchant(), m[3] == "NOT "; got != want {
			t.Errorf("%s: Go PerMerchant=%v, SQL says %v", code, got, want)
		}
	}
	slices.Sort(sqlCodes)
	if !slices.Equal(sqlCodes, ledger.Codes()) {
		t.Errorf("SQL chart %v, Go chart %v", sqlCodes, ledger.Codes())
	}
}
