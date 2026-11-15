package ledger_test

import (
	"fmt"

	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/money"
)

// Example reproduces the worked example in docs/ledger.md, so the numbers in
// the docs are checked by go test.
func Example() {
	l := ledger.NewMemory()
	usd := func(cents int64) money.Money { return money.New(cents, money.USD) }

	capture, _ := ledger.Capture("pay_1", "m1", usd(10_000), usd(320))
	refund, _ := ledger.Refund("pay_1", "m1", usd(2_500))
	settle, _ := ledger.Settlement("pay_1", "bank-ref-1", usd(10_000), usd(50))
	for _, tx := range []ledger.Tx{capture, refund, settle} {
		if _, err := l.Post(tx); err != nil {
			panic(err)
		}
	}

	total := money.Zero(money.USD)
	for _, k := range l.Accounts() {
		signed := l.Balance(k)
		normal, _ := ledger.NormalBalance(k.Type(), signed)
		fmt.Printf("%-38s signed %7d  normal %s\n", k, signed.Amount(), normal)
		total, _ = total.Add(signed)
	}
	fmt.Println("sum of signed balances:", total.Amount())

	// Output:
	// m1/merchant_payable/USD                signed   -7180  normal 71.80 USD
	// platform/bank_cash/USD                 signed    9950  normal 99.50 USD
	// platform/fee_revenue/USD               signed    -320  normal 3.20 USD
	// platform/network_fee_expense/USD       signed      50  normal 0.50 USD
	// platform/processor_receivable/USD      signed   -2500  normal -25.00 USD
	// sum of signed balances: 0
}
