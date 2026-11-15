package ledgertest

import (
	"pgregory.net/rapid"

	"github.com/kamboj-sajan/ledgerline/internal/ledger"
	"github.com/kamboj-sajan/ledgerline/internal/money"
)

// MemorySystem adapts ledger.Memory to System.
type MemorySystem struct {
	l       *ledger.Memory
	ids     []string
	checked int // ids[:checked] have already been re-validated
}

// NewMemorySystem returns a System backed by a fresh ledger.Memory.
func NewMemorySystem() *MemorySystem { return &MemorySystem{l: ledger.NewMemory()} }

// Post implements System.
func (s *MemorySystem) Post(_ *rapid.T, tx ledger.Tx) (string, error) {
	id, err := s.l.Post(tx)
	if err == nil {
		s.ids = append(s.ids, id)
	}
	return id, err
}

// Balances implements System.
func (s *MemorySystem) Balances(_ *rapid.T) map[ledger.AccountKey]money.Money {
	out := make(map[ledger.AccountKey]money.Money)
	for _, k := range s.l.Accounts() {
		out[k] = s.l.Balance(k)
	}
	return out
}

// CheckGlobal re-validates every transaction stored since the last check
// (each one once, so the whole run stays linear), and checks the transaction
// count matches the number of successful posts.
func (s *MemorySystem) CheckGlobal(t *rapid.T) {
	for _, id := range s.ids[s.checked:] {
		tx, ok := s.l.Transaction(id)
		if !ok {
			t.Fatalf("transaction %s vanished", id)
		}
		if err := tx.Validate(); err != nil {
			t.Fatalf("stored transaction %s is invalid: %v", id, err)
		}
	}
	s.checked = len(s.ids)
	if s.l.Len() != len(s.ids) {
		t.Fatalf("ledger holds %d transactions, %d were posted", s.l.Len(), len(s.ids))
	}
}
