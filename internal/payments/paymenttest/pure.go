package paymenttest

import (
	"errors"
	"strconv"

	"pgregory.net/rapid"

	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/money"
	"github.com/kamboj-sajan/ledgerline/internal/payments"
)

// PureSystem runs the payment domain methods against ledger.Memory, with the
// same rules the API applies around them (ownership, default amounts).
type PureSystem struct {
	l        *ledger.Memory
	pays     map[string]*payments.Payment
	declines map[string]bool
	ledgerTx []string
	checked  int
}

// NewPureSystem returns an empty PureSystem.
func NewPureSystem() *PureSystem {
	return &PureSystem{l: ledger.NewMemory(), pays: map[string]*payments.Payment{}, declines: map[string]bool{}}
}

// MerchantID implements System.
func (s *PureSystem) MerchantID(name string) string { return name }

// Create implements System.
func (s *PureSystem) Create(t *rapid.T, merchant string, amount money.Money, declines bool) string {
	id := "pay_" + strconv.Itoa(len(s.pays)+1)
	p, err := payments.New(id, merchant, amount)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s.pays[id], s.declines[id] = &p, declines
	return id
}

func (s *PureSystem) owned(merchant, id string) (*payments.Payment, bool) {
	p, ok := s.pays[id]
	return p, ok && p.MerchantID == merchant
}

// Authorize implements System.
func (s *PureSystem) Authorize(_ *rapid.T, merchant, id string) Outcome {
	p, ok := s.owned(merchant, id)
	if !ok {
		return NotFound
	}
	if _, can := payments.Next(p.Status, payments.EventApprove); !can {
		return InvalidTransition
	}
	if s.declines[id] {
		return outcome(p.MarkDeclined("card_declined", "declined"), Declined)
	}
	return outcome(p.MarkAuthorized("auth_"+id), OK)
}

// Capture implements System.
func (s *PureSystem) Capture(t *rapid.T, merchant, id string, amount *int64) Outcome {
	p, ok := s.owned(merchant, id)
	if !ok {
		return NotFound
	}
	take := p.Authorized
	if amount != nil {
		take = money.New(*amount, p.Amount.Currency())
	}
	work := *p // apply to a copy, keep it only if the ledger accepts the posting
	tx, err := work.Capture(take, Pricing)
	if err != nil {
		return outcome(err, OK)
	}
	s.post(t, tx)
	*p = work
	return OK
}

// Refund implements System.
func (s *PureSystem) Refund(t *rapid.T, merchant, id string, amount *int64) Outcome {
	p, ok := s.owned(merchant, id)
	if !ok {
		return NotFound
	}
	give := p.Remaining()
	if amount != nil {
		give = money.New(*amount, p.Amount.Currency())
	}
	work := *p
	tx, err := work.Refund(give)
	if err != nil {
		return outcome(err, OK)
	}
	s.post(t, tx)
	*p = work
	return OK
}

// Void implements System.
func (s *PureSystem) Void(_ *rapid.T, merchant, id string) Outcome {
	p, ok := s.owned(merchant, id)
	if !ok {
		return NotFound
	}
	return outcome(p.Void(), OK)
}

// Settle implements System.
func (s *PureSystem) Settle(t *rapid.T, id string, gross money.Money) Outcome {
	tx, err := ledger.Settlement(id, "stl_"+id, gross, money.Zero(gross.Currency()))
	if err != nil {
		t.Fatalf("settlement: %v", err)
	}
	jid, err := s.l.Post(tx)
	if errors.Is(err, ledger.ErrDuplicateExternalRef) {
		return DuplicateSettlement
	}
	if err != nil {
		t.Fatalf("settlement: %v", err)
	}
	s.ledgerTx = append(s.ledgerTx, jid)
	return OK
}

func (s *PureSystem) post(t *rapid.T, tx ledger.Tx) {
	jid, err := s.l.Post(tx)
	if err != nil {
		t.Fatalf("ledger rejected %s: %v", tx.Kind, err)
	}
	s.ledgerTx = append(s.ledgerTx, jid)
}

// Payment implements System.
func (s *PureSystem) Payment(t *rapid.T, merchant, id string) Snapshot {
	p, ok := s.owned(merchant, id)
	if !ok {
		t.Fatalf("payment %s not found for %s", id, merchant)
	}
	return Snapshot{Status: p.Status, Authorized: p.Authorized.Amount(), Captured: p.Captured.Amount(),
		Refunded: p.Refunded.Amount(), Fee: p.Fee.Amount()}
}

// Balances implements System.
func (s *PureSystem) Balances(_ *rapid.T) map[ledger.AccountKey]money.Money {
	out := map[ledger.AccountKey]money.Money{}
	for _, k := range s.l.Accounts() {
		out[k] = s.l.Balance(k)
	}
	return out
}

// CheckGlobal re-validates every ledger transaction posted since the last check.
func (s *PureSystem) CheckGlobal(t *rapid.T) {
	for _, jid := range s.ledgerTx[s.checked:] {
		tx, ok := s.l.Transaction(jid)
		if !ok || tx.Validate() != nil {
			t.Fatalf("ledger transaction %s missing or invalid", jid)
		}
	}
	s.checked = len(s.ledgerTx)
}

// outcome maps a domain error to an Outcome; nil maps to success.
func outcome(err error, success Outcome) Outcome {
	switch {
	case err == nil:
		return success
	case errors.Is(err, payments.ErrInvalidTransition):
		return InvalidTransition
	case errors.Is(err, payments.ErrAmountTooLarge):
		return AmountTooLarge
	default:
		return Outcome("unexpected: " + err.Error())
	}
}
