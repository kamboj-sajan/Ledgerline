package ledger

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kamboj-sajan/ledgerline/internal/money"
)

// Memory is an in-memory ledger with the same rules as the Postgres one. It
// validates each transaction, applies it all-or-nothing, and enforces the
// same uniqueness rules as the schema: an external reference is posted once
// and a transaction is reversed once. It exists so the posting logic can be
// property-tested over millions of operations without a database.
//
// Memory is not safe for concurrent use.
type Memory struct {
	balances     map[AccountKey]money.Money
	txs          map[string]Tx
	order        []string
	externalRefs map[string]string // external_ref -> transaction ID
	reversedBy   map[string]string // original ID -> reversal ID
}

// NewMemory returns an empty ledger.
func NewMemory() *Memory {
	return &Memory{
		balances:     make(map[AccountKey]money.Money),
		txs:          make(map[string]Tx),
		externalRefs: make(map[string]string),
		reversedBy:   make(map[string]string),
	}
}

// Post validates and records t, returning its ID. On error nothing changes.
//
// One difference from Postgres: Memory rejects a transaction that would push
// an account balance outside int64, while Postgres sums in numeric and would
// store it, then fail when the balance is read. Ledgerline caps payment
// amounts far below where that could happen.
func (l *Memory) Post(t Tx) (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}
	if t.ExternalRef != "" {
		if prev, dup := l.externalRefs[t.ExternalRef]; dup {
			return "", fmt.Errorf("%w: %q (transaction %s)", ErrDuplicateExternalRef, t.ExternalRef, prev)
		}
	}
	if t.Kind == KindReversal {
		if _, ok := l.txs[t.ReversesID]; !ok {
			return "", fmt.Errorf("%w: %s", ErrUnknownTransaction, t.ReversesID)
		}
		if prev, done := l.reversedBy[t.ReversesID]; done {
			return "", fmt.Errorf("%w: %s by %s", ErrAlreadyReversed, t.ReversesID, prev)
		}
	}

	// Work out every new balance before changing any, so a transaction that
	// overflows one account leaves the others untouched.
	updated := make(map[AccountKey]money.Money, len(t.Postings))
	for _, p := range t.Postings {
		cur, ok := updated[p.Account]
		if !ok {
			cur = l.Balance(p.Account)
		}
		next, err := cur.Add(p.Amount)
		if err != nil {
			return "", fmt.Errorf("%w: balance of %s", ErrTooLarge, p.Account)
		}
		updated[p.Account] = next
	}

	id := "mem_" + strconv.Itoa(len(l.order)+1)
	for k, b := range updated {
		l.balances[k] = b
	}
	l.txs[id] = cloneTx(t)
	l.order = append(l.order, id)
	if t.ExternalRef != "" {
		l.externalRefs[t.ExternalRef] = id
	}
	if t.Kind == KindReversal {
		l.reversedBy[t.ReversesID] = id
	}
	return id, nil
}

// Balance returns the signed (debit-positive) balance of k. An account with
// no postings has a zero balance.
func (l *Memory) Balance(k AccountKey) money.Money {
	if b, ok := l.balances[k]; ok {
		return b
	}
	return money.Zero(k.Currency)
}

// Accounts returns every account that has a posting, sorted by String().
func (l *Memory) Accounts() []AccountKey {
	keys := make([]AccountKey, 0, len(l.balances))
	for k := range l.balances {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b AccountKey) int { return strings.Compare(a.String(), b.String()) })
	return keys
}

// Transaction returns the transaction stored under id.
func (l *Memory) Transaction(id string) (Tx, bool) {
	t, ok := l.txs[id]
	return cloneTx(t), ok
}

// Len returns the number of posted transactions.
func (l *Memory) Len() int { return len(l.order) }

// cloneTx copies the postings slice so callers cannot change stored history
// by modifying a slice they passed in or got back.
func cloneTx(t Tx) Tx {
	t.Postings = slices.Clone(t.Postings)
	return t
}
