package payments_test

import (
	"errors"
	"testing"

	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/money"
	"github.com/kamboj-sajan/ledgerline/internal/payments"
)

func usd(a int64) money.Money { return money.New(a, money.USD) }

var pricing = payments.Pricing{BasisPoints: 290, Fixed: 30}

// TestTransitionTable goes through every (from, to) pair of statuses.
func TestTransitionTable(t *testing.T) {
	allowed := map[[2]payments.Status]bool{
		{payments.StatusCreated, payments.StatusAuthorized}:  true,
		{payments.StatusCreated, payments.StatusFailed}:      true,
		{payments.StatusAuthorized, payments.StatusCaptured}: true,
		{payments.StatusAuthorized, payments.StatusFailed}:   true,
		{payments.StatusCaptured, payments.StatusCaptured}:   true, // partial refund
		{payments.StatusCaptured, payments.StatusRefunded}:   true,
	}
	pairs := 0
	for _, from := range payments.Statuses {
		for _, to := range payments.Statuses {
			pairs++
			if got, want := payments.CanTransition(from, to), allowed[[2]payments.Status{from, to}]; got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
	if pairs != 25 {
		t.Fatalf("checked %d pairs, want 25", pairs)
	}
}

// TestEventTable goes through every (event, from) pair.
func TestEventTable(t *testing.T) {
	want := map[payments.Event]map[payments.Status]payments.Status{
		payments.EventApprove:       {payments.StatusCreated: payments.StatusAuthorized},
		payments.EventDecline:       {payments.StatusCreated: payments.StatusFailed},
		payments.EventCapture:       {payments.StatusAuthorized: payments.StatusCaptured},
		payments.EventVoid:          {payments.StatusAuthorized: payments.StatusFailed},
		payments.EventExpire:        {payments.StatusAuthorized: payments.StatusFailed},
		payments.EventPartialRefund: {payments.StatusCaptured: payments.StatusCaptured},
		payments.EventFullRefund:    {payments.StatusCaptured: payments.StatusRefunded},
	}
	for _, e := range payments.Events {
		for _, from := range payments.Statuses {
			to, ok := payments.Next(from, e)
			wantTo, wantOK := want[e][from]
			if ok != wantOK || to != wantTo {
				t.Errorf("Next(%s, %s) = %s, %v; want %s, %v", from, e, to, ok, wantTo, wantOK)
			}
		}
	}
	for _, s := range payments.Statuses {
		terminal := s == payments.StatusRefunded || s == payments.StatusFailed
		if s.Terminal() != terminal {
			t.Errorf("%s.Terminal() = %v, want %v", s, s.Terminal(), terminal)
		}
	}
}

// inState returns a payment of 100.00 USD driven into status s through the
// domain operations themselves.
func inState(t *testing.T, s payments.Status) payments.Payment {
	t.Helper()
	p, err := payments.New("pay_1", "m1", usd(10_000))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	switch s {
	case payments.StatusCreated:
	case payments.StatusAuthorized:
		must(p.MarkAuthorized("auth_1"))
	case payments.StatusCaptured:
		must(p.MarkAuthorized("auth_1"))
		_, err = p.Capture(usd(10_000), pricing)
		must(err)
	case payments.StatusRefunded:
		must(p.MarkAuthorized("auth_1"))
		_, err = p.Capture(usd(10_000), pricing)
		must(err)
		_, err = p.Refund(usd(10_000))
		must(err)
	case payments.StatusFailed:
		must(p.MarkDeclined("card_declined", "declined"))
	}
	if p.Status != s {
		t.Fatalf("set-up reached %s, want %s", p.Status, s)
	}
	return p
}

// TestOperationsFollowTheTable runs every operation against a payment in
// every state. Each must succeed exactly when the table allows its event,
// and otherwise fail with ErrInvalidTransition and leave the payment as it was.
func TestOperationsFollowTheTable(t *testing.T) {
	ops := []struct {
		name  string
		event payments.Event
		run   func(p *payments.Payment) error
	}{
		{"approve", payments.EventApprove, func(p *payments.Payment) error { return p.MarkAuthorized("auth") }},
		{"decline", payments.EventDecline, func(p *payments.Payment) error { return p.MarkDeclined("card_declined", "no") }},
		{"capture", payments.EventCapture, func(p *payments.Payment) error { _, err := p.Capture(usd(5_000), pricing); return err }},
		{"void", payments.EventVoid, func(p *payments.Payment) error { return p.Void() }},
		{"expire", payments.EventExpire, func(p *payments.Payment) error { return p.Expire() }},
		{"partial refund", payments.EventPartialRefund, func(p *payments.Payment) error { _, err := p.Refund(usd(1)); return err }},
	}
	for _, from := range payments.Statuses {
		for _, op := range ops {
			t.Run(op.name+" from "+string(from), func(t *testing.T) {
				p := inState(t, from)
				before := p
				err := op.run(&p)
				wantTo, allowed := payments.Next(from, op.event)
				if !allowed {
					if !errors.Is(err, payments.ErrInvalidTransition) {
						t.Fatalf("error = %v, want ErrInvalidTransition", err)
					}
					var te *payments.TransitionError
					if !errors.As(err, &te) || te.From != from {
						t.Fatalf("error %v does not report from=%s", err, from)
					}
					if p.Status != before.Status || p.Captured != before.Captured || p.Refunded != before.Refunded {
						t.Fatalf("rejected %s changed the payment: %+v", op.name, p)
					}
					return
				}
				if err != nil {
					t.Fatalf("allowed %s failed: %v", op.name, err)
				}
				if p.Status != wantTo {
					t.Fatalf("status = %s, want %s", p.Status, wantTo)
				}
			})
		}
	}
}

func TestCapture(t *testing.T) {
	tests := []struct {
		name    string
		amount  money.Money
		wantErr error
		wantFee int64
	}{
		{name: "full", amount: usd(10_000), wantFee: 320},   // 290 + 30
		{name: "partial", amount: usd(4_000), wantFee: 146}, // 116 + 30
		{name: "tiny, fee capped at amount", amount: usd(10), wantFee: 10},
		{name: "more than authorized", amount: usd(10_001), wantErr: payments.ErrAmountTooLarge},
		{name: "zero", amount: usd(0), wantErr: payments.ErrInvalidAmount},
		{name: "negative", amount: usd(-1), wantErr: payments.ErrInvalidAmount},
		{name: "other currency", amount: money.New(100, money.EUR), wantErr: payments.ErrInvalidAmount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := inState(t, payments.StatusAuthorized)
			tx, err := p.Capture(tt.amount, pricing)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				if p.Status != payments.StatusAuthorized {
					t.Fatalf("status changed to %s on a rejected capture", p.Status)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.Status != payments.StatusCaptured || p.Captured != tt.amount || p.Fee.Amount() != tt.wantFee {
				t.Fatalf("after capture: status %s captured %v fee %v", p.Status, p.Captured, p.Fee)
			}
			if tx.Kind != ledger.KindCapture || tx.PaymentID != "pay_1" || tx.Validate() != nil {
				t.Fatalf("bad ledger transaction %+v", tx)
			}
		})
	}
}

func TestRefunds(t *testing.T) {
	p := inState(t, payments.StatusCaptured)

	if _, err := p.Refund(usd(10_001)); !errors.Is(err, payments.ErrAmountTooLarge) {
		t.Fatalf("over-refund error = %v", err)
	}
	if _, err := p.Refund(usd(3_000)); err != nil || p.Status != payments.StatusCaptured || p.Refunded != usd(3_000) {
		t.Fatalf("first partial refund: %v, status %s, refunded %v", err, p.Status, p.Refunded)
	}
	if _, err := p.Refund(usd(7_001)); !errors.Is(err, payments.ErrAmountTooLarge) {
		t.Fatalf("refund past the remaining amount: error = %v", err)
	}
	if p.Remaining() != usd(7_000) {
		t.Fatalf("Remaining() = %v", p.Remaining())
	}
	tx, err := p.Refund(usd(7_000))
	if err != nil || p.Status != payments.StatusRefunded || p.Refunded != p.Captured {
		t.Fatalf("final refund: %v, status %s, refunded %v", err, p.Status, p.Refunded)
	}
	if tx.Kind != ledger.KindRefund || tx.Validate() != nil {
		t.Fatalf("bad ledger transaction %+v", tx)
	}
	if _, err := p.Refund(usd(1)); !errors.Is(err, payments.ErrInvalidTransition) {
		t.Fatalf("refund of a refunded payment: error = %v, want ErrInvalidTransition", err)
	}
}

func TestStateIsCheckedBeforeAmount(t *testing.T) {
	// A capture that is both in the wrong state and too large reports the
	// state problem (409), not the amount problem (400).
	p := inState(t, payments.StatusCreated)
	if _, err := p.Capture(usd(999_999), pricing); !errors.Is(err, payments.ErrInvalidTransition) {
		t.Fatalf("error = %v, want ErrInvalidTransition", err)
	}
}

func TestNewValidatesAmount(t *testing.T) {
	for _, a := range []money.Money{usd(0), usd(-1), usd(payments.MaxAmount + 1), {}} {
		if _, err := payments.New("pay_1", "m1", a); !errors.Is(err, payments.ErrInvalidAmount) {
			t.Errorf("New(%v) error = %v, want ErrInvalidAmount", a, err)
		}
	}
	p, err := payments.New("pay_1", "m1", usd(payments.MaxAmount))
	if err != nil || p.Status != payments.StatusCreated || !p.Authorized.IsZero() {
		t.Fatalf("New(max) = %+v, %v", p, err)
	}
}

func TestPricingFee(t *testing.T) {
	tests := []struct {
		pricing payments.Pricing
		amount  money.Money
		want    int64
	}{
		{payments.Pricing{BasisPoints: 290, Fixed: 30}, usd(10_000), 320},
		{payments.Pricing{BasisPoints: 290, Fixed: 30}, usd(1_001), 59}, // 29.029 rounds to 29, plus 30
		{payments.Pricing{BasisPoints: 290, Fixed: 30}, usd(50), 31},    // 1.45 rounds to 1, plus 30
		{payments.Pricing{BasisPoints: 290, Fixed: 30}, usd(20), 20},    // 30.58 capped at 20
		{payments.Pricing{BasisPoints: 0, Fixed: 0}, usd(10_000), 0},
		{payments.Pricing{BasisPoints: 10_000, Fixed: 0}, usd(10_000), 10_000},
		{payments.Pricing{BasisPoints: 360, Fixed: 0}, money.New(1_000, money.JPY), 36},
	}
	for _, tt := range tests {
		got, err := tt.pricing.Fee(tt.amount)
		if err != nil || got.Amount() != tt.want || got.Currency() != tt.amount.Currency() {
			t.Errorf("%+v.Fee(%v) = %v, %v; want %d", tt.pricing, tt.amount, got, err, tt.want)
		}
	}
}
