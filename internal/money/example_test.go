package money_test

import (
	"errors"
	"fmt"

	"github.com/kamboj-sajan/ledgerline/internal/money"
)

func Example() {
	price := money.New(1999, money.USD) // $19.99 as 1999 cents
	fee, _ := price.MulBasisPoints(290) // 2.9%, rounded half away from zero
	net, _ := price.Sub(fee)
	fmt.Println(price, fee, net)

	_, err := price.Add(money.New(500, money.JPY))
	fmt.Println(errors.Is(err, money.ErrCurrencyMismatch))

	// Output:
	// 19.99 USD 0.58 USD 19.41 USD
	// true
}
