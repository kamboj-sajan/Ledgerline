// Package paymenttest is a stateful property test of the payment lifecycle.
//
// Run drives a System through random sequences of create, authorize
// (approved or declined), capture, refund, void and settle, across two
// merchants and three currencies (exponents 0, 2 and 3), including
// operations that must be rejected: the wrong merchant, the wrong state,
// amounts above what is authorized or unrefunded, a second settlement.
// After every step it compares every payment and every ledger balance with a
// simple reference model that computes statuses, amounts and fees with its own
// arithmetic, and runs the System's global checks (the audits).
//
// The same machine runs against the pure domain with ledger.Memory (fast,
// counted toward make prop-long) and against the HTTP API on Postgres.
package paymenttest

import (
	"math/big"

	"pgregory.net/rapid"

	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/money"
	"github.com/kamboj-sajan/ledgerline/internal/payments"
)

// Outcome is what an operation did, in terms both Systems can report.
type Outcome string

// Outcomes.
const (
	OK                  Outcome = "ok"
	Declined            Outcome = "declined"
	InvalidTransition   Outcome = "invalid_state_transition"
	AmountTooLarge      Outcome = "amount_too_large"
	NotFound            Outcome = "not_found"
	DuplicateSettlement Outcome = "duplicate_settlement"
)

// Snapshot is a payment's money state.
type Snapshot struct {
	Status                              payments.Status
	Authorized, Captured, Refunded, Fee int64
}

// System is a payments implementation under test. Merchants are named "m1"
// and "m2"; MerchantID maps a name to the ID the System uses in ledger keys.
// A nil amount means "use the default" (everything authorized, everything
// unrefunded).
type System interface {
	MerchantID(name string) string
	Create(t *rapid.T, merchant string, amount money.Money, declines bool) string
	Authorize(t *rapid.T, merchant, id string) Outcome
	Capture(t *rapid.T, merchant, id string, amount *int64) Outcome
	Refund(t *rapid.T, merchant, id string, amount *int64) Outcome
	Void(t *rapid.T, merchant, id string) Outcome
	// Settle posts the settlement of a payment's captured amount to the ledger.
	Settle(t *rapid.T, id string, gross money.Money) Outcome
	Payment(t *rapid.T, merchant, id string) Snapshot
	Balances(t *rapid.T) map[ledger.AccountKey]money.Money
	CheckGlobal(t *rapid.T)
}

// Stats counts operations. Ops = Accepted + Rejected.
type Stats struct {
	Ops, Accepted, Rejected int64
}

// Pricing both Systems must use: 2.9% + 30 minor units.
var Pricing = payments.Pricing{BasisPoints: 290, Fixed: 30}

var (
	merchantNames = []string{"m1", "m2"}
	currencies    = []money.Currency{money.USD, money.JPY, money.BHD}
)

const maxPayment = 1_000_000

type modelPayment struct {
	id                                  string
	merchant                            string
	currency                            money.Currency
	amount                              int64
	declines                            bool
	status                              payments.Status
	authorized, captured, refunded, fee int64
	settled                             bool
}

type machine struct {
	sys      System
	stats    *Stats
	pays     []*modelPayment
	balances map[ledger.AccountKey]int64
}

// Run executes one random sequence against sys. Call it inside rapid.Check
// with a fresh System each time.
func Run(t *rapid.T, sys System, stats *Stats) {
	m := &machine{sys: sys, stats: stats, balances: map[ledger.AccountKey]int64{}}
	t.Repeat(map[string]func(*rapid.T){
		"create":    m.create,
		"authorize": m.authorize,
		"capture":   m.capture,
		"refund":    m.refund,
		"void":      m.void,
		"settle":    m.settle,
		"":          m.check,
	})
}

// modelFee is the fee formula written out independently of payments.Pricing:
// round half up of amount × 290 / 10000, plus 30, capped at the amount.
func modelFee(amount int64) int64 {
	fee := (amount*290+5_000)/10_000 + 30
	return min(fee, amount)
}

func (m *machine) create(t *rapid.T) {
	p := &modelPayment{
		merchant: rapid.SampledFrom(merchantNames).Draw(t, "merchant"),
		currency: rapid.SampledFrom(currencies).Draw(t, "currency"),
		amount:   rapid.Int64Range(1, maxPayment).Draw(t, "amount"),
		declines: rapid.Float64Range(0, 1).Draw(t, "decline roll") < 0.2,
		status:   payments.StatusCreated,
	}
	p.id = m.sys.Create(t, p.merchant, money.New(p.amount, p.currency), p.declines)
	m.pays = append(m.pays, p)
	m.count(OK)
}

// target picks a payment and, sometimes, the wrong merchant to ask with.
func (m *machine) target(t *rapid.T) (*modelPayment, string) {
	if len(m.pays) == 0 {
		t.Skip("no payments yet")
	}
	p := rapid.SampledFrom(m.pays).Draw(t, "payment")
	asker := p.merchant
	if rapid.Float64Range(0, 1).Draw(t, "wrong merchant roll") < 0.1 {
		asker = "m1"
		if p.merchant == "m1" {
			asker = "m2"
		}
	}
	return p, asker
}

func (m *machine) authorize(t *rapid.T) {
	p, asker := m.target(t)
	var want Outcome
	switch {
	case asker != p.merchant:
		want = NotFound
	case p.status != payments.StatusCreated:
		want = InvalidTransition
	case p.declines:
		want = Declined
	default:
		want = OK
	}
	m.expect(t, "authorize", p, want, m.sys.Authorize(t, asker, p.id))
	switch want {
	case OK:
		p.status, p.authorized = payments.StatusAuthorized, p.amount
	case Declined:
		p.status = payments.StatusFailed
	}
}

// optionalAmount draws nil ("use the default") or an amount up to 20% above limit.
func optionalAmount(t *rapid.T, limit int64) *int64 {
	if rapid.Bool().Draw(t, "use default amount") {
		return nil
	}
	a := rapid.Int64Range(1, max(1, limit+limit/5+1)).Draw(t, "amount")
	return &a
}

func (m *machine) capture(t *rapid.T) {
	p, asker := m.target(t)
	amount := optionalAmount(t, p.authorized)
	take := p.authorized
	if amount != nil {
		take = *amount
	}
	var want Outcome
	switch {
	case asker != p.merchant:
		want = NotFound
	case p.status != payments.StatusAuthorized:
		want = InvalidTransition
	case take > p.authorized:
		want = AmountTooLarge
	default:
		want = OK
	}
	m.expect(t, "capture", p, want, m.sys.Capture(t, asker, p.id, amount))
	if want == OK {
		p.status, p.captured, p.fee = payments.StatusCaptured, take, modelFee(take)
		m.add(ledger.Receivable(p.currency), take)
		m.add(ledger.Payable(m.sys.MerchantID(p.merchant), p.currency), -(take - p.fee))
		m.add(ledger.Fees(p.currency), -p.fee)
	}
}

func (m *machine) refund(t *rapid.T) {
	p, asker := m.target(t)
	remaining := p.captured - p.refunded
	amount := optionalAmount(t, remaining)
	give := remaining
	if amount != nil {
		give = *amount
	}
	var want Outcome
	switch {
	case asker != p.merchant:
		want = NotFound
	case p.status != payments.StatusCaptured:
		want = InvalidTransition
	case give > remaining:
		want = AmountTooLarge
	default:
		want = OK
	}
	m.expect(t, "refund", p, want, m.sys.Refund(t, asker, p.id, amount))
	if want == OK {
		p.refunded += give
		if p.refunded == p.captured {
			p.status = payments.StatusRefunded
		}
		m.add(ledger.Payable(m.sys.MerchantID(p.merchant), p.currency), give)
		m.add(ledger.Receivable(p.currency), -give)
	}
}

func (m *machine) void(t *rapid.T) {
	p, asker := m.target(t)
	var want Outcome
	switch {
	case asker != p.merchant:
		want = NotFound
	case p.status != payments.StatusAuthorized:
		want = InvalidTransition
	default:
		want = OK
	}
	m.expect(t, "void", p, want, m.sys.Void(t, asker, p.id))
	if want == OK {
		p.status = payments.StatusFailed
	}
}

func (m *machine) settle(t *rapid.T) {
	var captured []*modelPayment
	for _, p := range m.pays {
		if p.captured > 0 {
			captured = append(captured, p)
		}
	}
	if len(captured) == 0 {
		t.Skip("nothing captured yet")
	}
	p := rapid.SampledFrom(captured).Draw(t, "payment")
	want := OK
	if p.settled {
		want = DuplicateSettlement
	}
	m.expect(t, "settle", p, want, m.sys.Settle(t, p.id, money.New(p.captured, p.currency)))
	if want == OK {
		p.settled = true
		m.add(ledger.Bank(p.currency), p.captured)
		m.add(ledger.Receivable(p.currency), -p.captured)
	}
}

func (m *machine) add(k ledger.AccountKey, d int64) { m.balances[k] += d }

func (m *machine) expect(t *rapid.T, op string, p *modelPayment, want, got Outcome) {
	if got != want {
		t.Fatalf("%s on %s (status %s, authorized %d, captured %d, refunded %d): got %s, model says %s",
			op, p.id, p.status, p.authorized, p.captured, p.refunded, got, want)
	}
	m.count(want)
}

func (m *machine) count(o Outcome) {
	m.stats.Ops++
	if o == OK {
		m.stats.Accepted++
	} else {
		m.stats.Rejected++
	}
}

// check compares every payment and every balance with the model, then runs
// the System's global checks.
func (m *machine) check(t *rapid.T) {
	for _, p := range m.pays {
		got := m.sys.Payment(t, p.merchant, p.id)
		want := Snapshot{Status: p.status, Authorized: p.authorized, Captured: p.captured, Refunded: p.refunded, Fee: p.fee}
		if got != want {
			t.Fatalf("payment %s = %+v, model says %+v", p.id, got, want)
		}
		if got.Refunded > got.Captured || got.Captured > got.Authorized || got.Authorized > p.amount {
			t.Fatalf("payment %s amounts out of order: %+v", p.id, got)
		}
	}
	got := m.sys.Balances(t)
	for k, want := range m.balances {
		if b := got[k]; b.Amount() != want {
			t.Fatalf("balance of %s = %d, model says %d", k, b.Amount(), want)
		}
	}
	sums := map[money.Currency]*big.Int{}
	for k, b := range got {
		if _, ok := m.balances[k]; !ok && !b.IsZero() {
			t.Fatalf("account %s has %v but the model never touched it", k, b)
		}
		if sums[k.Currency] == nil {
			sums[k.Currency] = new(big.Int)
		}
		sums[k.Currency].Add(sums[k.Currency], big.NewInt(b.Amount()))
	}
	for c, s := range sums {
		if s.Sign() != 0 {
			t.Fatalf("global %s sum = %s, want 0", c, s)
		}
	}
	m.sys.CheckGlobal(t)
}
