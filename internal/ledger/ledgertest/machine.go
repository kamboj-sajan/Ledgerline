// Package ledgertest is a stateful property test for ledger implementations.
//
// Run drives a System through a random sequence of ledger operations, valid
// and invalid, across several merchants and currencies. After every step it
// compares the System with a reference model that is deliberately simple: it
// tracks each account's expected balance with plain int64 arithmetic derived
// from the operation's inputs (for a capture of X with fee f: receivable +X,
// merchant payable -(X-f), fee revenue -f). It never calls the posting
// builders it is checking.
//
// The same machine runs against ledger.Memory (fast, for millions of
// operations) and against Postgres (slower, but it exercises the real SQL and
// triggers).
package ledgertest

import (
	"errors"
	"fmt"
	"math/big"

	"pgregory.net/rapid"

	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/money"
)

// System is a ledger under test.
type System interface {
	// Post records tx, returning its ID or an error if tx was rejected.
	Post(t *rapid.T, tx ledger.Tx) (string, error)
	// Balances returns the signed balance of every account the system knows.
	Balances(t *rapid.T) map[ledger.AccountKey]money.Money
	// CheckGlobal checks whole-ledger invariants the System can see but the
	// model cannot, such as every stored transaction balancing. It calls
	// t.Fatalf on failure.
	CheckGlobal(t *rapid.T)
}

// Stats counts the operations Run executed. Ops = Posted + Rejected.
type Stats struct {
	Ops      int64
	Posted   int64
	Rejected int64
}

var (
	merchants  = []string{"m1", "m2", "m3"}
	currencies = []money.Currency{money.USD, money.EUR, money.JPY, money.BHD}
)

// Merchants returns the merchant IDs Run uses. A database-backed System must
// create them first, because accounts reference merchants.
func Merchants() []string { return merchants }

// maxAmount keeps sums far from int64 limits over any realistic run: 10^10
// minor units ($100M) × a few thousand operations is still below 10^14.
const maxAmount = 10_000_000_000

type payment struct {
	id       string
	merchant string
	currency money.Currency
	captured int64
	refunded int64
	settled  bool
}

type postedTx struct {
	id       string
	tx       ledger.Tx                   // as sent, used to build its reversal
	delta    map[ledger.AccountKey]int64 // the model's own view of its effect
	reversed bool
}

type machine struct {
	sys      System
	stats    *Stats
	balances map[ledger.AccountKey]int64
	payments []*payment
	posted   []*postedTx
}

// Run executes one random sequence of operations against sys. Call it from
// inside rapid.Check, with a fresh System each time.
func Run(t *rapid.T, sys System, stats *Stats) {
	m := &machine{sys: sys, stats: stats, balances: make(map[ledger.AccountKey]int64)}
	t.Repeat(map[string]func(*rapid.T){
		"capture":         m.capture,
		"refund":          m.refund,
		"settle":          m.settle,
		"settle again":    m.settleAgain,
		"reverse":         m.reverse,
		"reverse again":   m.reverseAgain,
		"malformed post":  m.malformed,
		"invalid builder": m.invalidBuilder,
		"":                m.check,
	})
}

func (m *machine) capture(t *rapid.T) {
	merchant := rapid.SampledFrom(merchants).Draw(t, "merchant")
	c := rapid.SampledFrom(currencies).Draw(t, "currency")
	amount := rapid.Int64Range(1, maxAmount).Draw(t, "amount")
	// Edge cases on purpose: no fee, and a fee that takes everything.
	fee := rapid.OneOf(rapid.Just(int64(0)), rapid.Just(amount), rapid.Int64Range(0, amount)).Draw(t, "fee")

	p := &payment{id: fmt.Sprintf("pay_%d", len(m.payments)+1), merchant: merchant, currency: c, captured: amount}
	tx, err := ledger.Capture(p.id, merchant, money.New(amount, c), money.New(fee, c))
	if err != nil {
		t.Fatalf("Capture(%d, fee %d %s) rejected valid input: %v", amount, fee, c, err)
	}
	m.mustPost(t, tx, map[ledger.AccountKey]int64{
		ledger.Receivable(c):        amount,
		ledger.Payable(merchant, c): -(amount - fee),
		ledger.Fees(c):              -fee,
	})
	m.payments = append(m.payments, p)
}

func (m *machine) refund(t *rapid.T) {
	p := pick(t, m.payments, func(p *payment) bool { return p.refunded < p.captured })
	r := rapid.Int64Range(1, p.captured-p.refunded).Draw(t, "refund")
	tx, err := ledger.Refund(p.id, p.merchant, money.New(r, p.currency))
	if err != nil {
		t.Fatalf("Refund(%d %s) rejected valid input: %v", r, p.currency, err)
	}
	m.mustPost(t, tx, map[ledger.AccountKey]int64{
		ledger.Payable(p.merchant, p.currency): r,
		ledger.Receivable(p.currency):          -r,
	})
	p.refunded += r
}

func settlementRef(p *payment) string { return "stl_" + p.id }

func (m *machine) settle(t *rapid.T) {
	p := pick(t, m.payments, func(p *payment) bool { return !p.settled })
	nf := rapid.Int64Range(0, p.captured/100).Draw(t, "network fee")
	tx, err := ledger.Settlement(p.id, settlementRef(p), money.New(p.captured, p.currency), money.New(nf, p.currency))
	if err != nil {
		t.Fatalf("Settlement rejected valid input: %v", err)
	}
	m.mustPost(t, tx, map[ledger.AccountKey]int64{
		ledger.Bank(p.currency):        p.captured - nf,
		ledger.NetworkFees(p.currency): nf,
		ledger.Receivable(p.currency):  -p.captured,
	})
	p.settled = true
}

// settleAgain replays a settlement reference, as a re-run of reconciliation
// would. It must be rejected however the amounts differ.
func (m *machine) settleAgain(t *rapid.T) {
	p := pick(t, m.payments, func(p *payment) bool { return p.settled })
	gross := rapid.Int64Range(1, maxAmount).Draw(t, "gross")
	tx, err := ledger.Settlement(p.id, settlementRef(p), money.New(gross, p.currency), money.Zero(p.currency))
	if err != nil {
		t.Fatalf("Settlement rejected valid input: %v", err)
	}
	m.mustReject(t, tx, ledger.ErrDuplicateExternalRef)
}

func (m *machine) reverse(t *rapid.T) {
	pt := pick(t, m.posted, func(pt *postedTx) bool { return !pt.reversed && pt.tx.Kind != ledger.KindReversal })
	tx, err := ledger.Reverse(pt.id, pt.tx)
	if err != nil {
		t.Fatalf("Reverse(%s) failed: %v", pt.id, err)
	}
	delta := make(map[ledger.AccountKey]int64, len(pt.delta))
	for k, d := range pt.delta {
		delta[k] = -d
	}
	m.mustPost(t, tx, delta)
	pt.reversed = true
}

func (m *machine) reverseAgain(t *rapid.T) {
	pt := pick(t, m.posted, func(pt *postedTx) bool { return pt.reversed })
	tx, err := ledger.Reverse(pt.id, pt.tx)
	if err != nil {
		t.Fatalf("Reverse(%s) failed: %v", pt.id, err)
	}
	m.mustReject(t, tx, ledger.ErrAlreadyReversed)
}

// malformed posts a transaction that breaks one ledger rule. Every System
// must reject it and change nothing.
func (m *machine) malformed(t *rapid.T) {
	c := rapid.SampledFrom(currencies).Draw(t, "currency")
	other := rapid.SampledFrom(currencies).Filter(func(o money.Currency) bool { return o != c }).Draw(t, "other currency")
	merchant := rapid.SampledFrom(merchants).Draw(t, "merchant")
	amt := rapid.Int64Range(1, maxAmount).Draw(t, "amount")
	recv, pay := ledger.Receivable(c), ledger.Payable(merchant, c)
	post := func(k ledger.AccountKey, a int64, cur money.Currency) ledger.Posting {
		return ledger.Posting{Account: k, Amount: money.New(a, cur)}
	}

	var postings []ledger.Posting
	switch kind := rapid.SampledFrom([]string{
		"single posting", "unbalanced", "zero amount", "posting currency differs from account",
		"unknown account code", "merchant account without merchant",
	}).Draw(t, "malformation"); kind {
	case "single posting":
		postings = []ledger.Posting{post(recv, amt, c)}
	case "unbalanced":
		postings = []ledger.Posting{post(recv, amt, c), post(pay, -(amt + 1), c)}
	case "zero amount":
		postings = []ledger.Posting{post(recv, 0, c), post(pay, 0, c)}
	case "posting currency differs from account":
		postings = []ledger.Posting{post(recv, amt, other), post(pay, -amt, other)}
	case "unknown account code":
		postings = []ledger.Posting{post(ledger.AccountKey{Code: "suspense", Currency: c}, amt, c), post(pay, -amt, c)}
	case "merchant account without merchant":
		postings = []ledger.Posting{post(recv, amt, c), post(ledger.Payable("", c), -amt, c)}
	}
	m.mustReject(t, ledger.Tx{Kind: ledger.KindCapture, PaymentID: "pay_malformed", Postings: postings}, nil)
}

// invalidBuilder feeds the posting builders input they must refuse.
func (m *machine) invalidBuilder(t *rapid.T) {
	c := rapid.SampledFrom(currencies).Draw(t, "currency")
	other := rapid.SampledFrom(currencies).Filter(func(o money.Currency) bool { return o != c }).Draw(t, "other currency")
	amt := rapid.Int64Range(1, maxAmount).Draw(t, "amount")
	var err error
	switch kind := rapid.SampledFrom([]string{
		"fee above amount", "non-positive capture", "fee in another currency",
		"non-positive refund", "settlement without reference", "network fee above gross",
	}).Draw(t, "invalid input"); kind {
	case "fee above amount":
		_, err = ledger.Capture("pay_x", "m1", money.New(amt, c), money.New(amt+1, c))
	case "non-positive capture":
		_, err = ledger.Capture("pay_x", "m1", money.New(-rapid.Int64Range(0, maxAmount).Draw(t, "neg"), c), money.Zero(c))
	case "fee in another currency":
		_, err = ledger.Capture("pay_x", "m1", money.New(amt, c), money.New(1, other))
	case "non-positive refund":
		_, err = ledger.Refund("pay_x", "m1", money.New(-rapid.Int64Range(0, maxAmount).Draw(t, "neg"), c))
	case "settlement without reference":
		_, err = ledger.Settlement("pay_x", "", money.New(amt, c), money.Zero(c))
	case "network fee above gross":
		_, err = ledger.Settlement("pay_x", "ref", money.New(amt, c), money.New(amt+1, c))
	}
	if !errors.Is(err, ledger.ErrInvalidAmount) {
		t.Fatalf("builder accepted invalid input (error = %v)", err)
	}
	m.stats.Ops++
	m.stats.Rejected++
}

func (m *machine) mustPost(t *rapid.T, tx ledger.Tx, delta map[ledger.AccountKey]int64) {
	id, err := m.sys.Post(t, tx)
	if err != nil {
		t.Fatalf("valid %s rejected: %v", tx.Kind, err)
	}
	for k, d := range delta {
		m.balances[k] += d
	}
	m.posted = append(m.posted, &postedTx{id: id, tx: tx, delta: delta})
	m.stats.Ops++
	m.stats.Posted++
}

// mustReject posts tx and requires an error: errors.Is(err, want) if want is
// non-nil, any error otherwise. The model does not change, and the next check
// proves the System did not change either.
func (m *machine) mustReject(t *rapid.T, tx ledger.Tx, want error) {
	_, err := m.sys.Post(t, tx)
	if err == nil {
		t.Fatalf("invalid %s transaction was accepted: %+v", tx.Kind, tx)
	}
	if want != nil && !errors.Is(err, want) {
		t.Fatalf("rejected with %v, want %v", err, want)
	}
	m.stats.Ops++
	m.stats.Rejected++
}

// check runs after every step. It compares every balance with the model,
// checks the global sum per currency is zero, then runs the System's own
// whole-ledger checks.
func (m *machine) check(t *rapid.T) {
	got := m.sys.Balances(t)
	for k, want := range m.balances {
		if b := got[k]; b.Amount() != want {
			t.Fatalf("balance of %s = %d, model says %d", k, b.Amount(), want)
		}
	}
	sums := make(map[money.Currency]*big.Int)
	for k, b := range got {
		if _, known := m.balances[k]; !known && !b.IsZero() {
			t.Fatalf("account %s has balance %v but the model never touched it", k, b)
		}
		if sums[k.Currency] == nil {
			sums[k.Currency] = new(big.Int)
		}
		sums[k.Currency].Add(sums[k.Currency], big.NewInt(b.Amount()))
	}
	for c, s := range sums {
		if s.Sign() != 0 {
			t.Fatalf("global sum of %s balances = %s, want 0", c, s)
		}
	}
	m.sys.CheckGlobal(t)
}

// pick draws an element satisfying ok, or skips the action if none does
// (rapid then picks a different action).
func pick[T any](t *rapid.T, items []T, ok func(T) bool) T {
	var candidates []T
	for _, it := range items {
		if ok(it) {
			candidates = append(candidates, it)
		}
	}
	if len(candidates) == 0 {
		t.Skip("no candidates")
	}
	return rapid.SampledFrom(candidates).Draw(t, "target")
}
