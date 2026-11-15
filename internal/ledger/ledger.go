// Package ledger is Ledgerline's double-entry bookkeeping: the chart of
// accounts, the postings each kind of money movement produces, validation of
// journal transactions, an in-memory ledger for property tests, and the
// Postgres ledger used by the application.
//
// Sign convention: a posting's amount is positive for a debit and negative
// for a credit. A journal transaction therefore balances exactly when its
// postings sum to zero in each currency, and an account's balance is the sum
// of its postings.
package ledger

import (
	"errors"
	"fmt"
	"slices"

	"github.com/kamboj-sajan/ledgerline/internal/money"
)

// Validation and posting errors. Each error returned by this package wraps
// exactly one of these, so callers can use errors.Is.
var (
	ErrInvalidKind          = errors.New("ledger: invalid transaction kind")
	ErrTooFewPostings       = errors.New("ledger: a transaction needs at least 2 postings")
	ErrZeroPosting          = errors.New("ledger: posting amount is zero")
	ErrUnbalanced           = errors.New("ledger: postings do not sum to zero")
	ErrAccountCurrency      = errors.New("ledger: posting currency differs from account currency")
	ErrUnknownAccount       = errors.New("ledger: account is not in the chart of accounts")
	ErrTooLarge             = errors.New("ledger: amounts overflow int64")
	ErrInvalidAmount        = errors.New("ledger: invalid amount")
	ErrDuplicateExternalRef = errors.New("ledger: external reference already posted")
	ErrAlreadyReversed      = errors.New("ledger: transaction already reversed")
	ErrUnknownTransaction   = errors.New("ledger: no such transaction")
)

// AccountType is the accounting class of an account.
type AccountType string

// Account types.
const (
	Asset     AccountType = "asset"
	Liability AccountType = "liability"
	Revenue   AccountType = "revenue"
	Expense   AccountType = "expense"
)

// AccountCode names an account in the chart of accounts.
type AccountCode string

// The chart of accounts. Every account belongs to exactly one currency.
const (
	// ProcessorReceivable (asset): money the card network owes us for
	// captured payments that it has not yet settled.
	ProcessorReceivable AccountCode = "processor_receivable"
	// BankCash (asset): money that has arrived in our bank account.
	BankCash AccountCode = "bank_cash"
	// MerchantPayable (liability, one per merchant): money we owe the merchant.
	MerchantPayable AccountCode = "merchant_payable"
	// FeeRevenue (revenue): our processing fees.
	FeeRevenue AccountCode = "fee_revenue"
	// NetworkFeeExpense (expense): fees the network deducts at settlement.
	NetworkFeeExpense AccountCode = "network_fee_expense"
)

type chartEntry struct {
	typ         AccountType
	perMerchant bool
}

// chart must agree with the accounts_chart CHECK constraint in
// migrations/00002_ledger.sql; TestChartMatchesMigration enforces that.
var chart = map[AccountCode]chartEntry{
	ProcessorReceivable: {Asset, false},
	BankCash:            {Asset, false},
	MerchantPayable:     {Liability, true},
	FeeRevenue:          {Revenue, false},
	NetworkFeeExpense:   {Expense, false},
}

// Codes returns every account code, sorted.
func Codes() []AccountCode {
	codes := make([]AccountCode, 0, len(chart))
	for c := range chart {
		codes = append(codes, c)
	}
	slices.Sort(codes)
	return codes
}

// Type returns the code's account type, or "" for an unknown code.
func (c AccountCode) Type() AccountType { return chart[c].typ }

// PerMerchant reports whether each merchant has its own account with this code.
func (c AccountCode) PerMerchant() bool { return chart[c].perMerchant }

// AccountKey identifies an account by what it is (whose, which code, which
// currency) rather than by its database ID. Accounts are created on first use.
type AccountKey struct {
	MerchantID string // "" for platform accounts
	Code       AccountCode
	Currency   money.Currency
}

// Receivable is the platform's processor_receivable account in c.
func Receivable(c money.Currency) AccountKey {
	return AccountKey{Code: ProcessorReceivable, Currency: c}
}

// Bank is the platform's bank_cash account in c.
func Bank(c money.Currency) AccountKey { return AccountKey{Code: BankCash, Currency: c} }

// Fees is the platform's fee_revenue account in c.
func Fees(c money.Currency) AccountKey { return AccountKey{Code: FeeRevenue, Currency: c} }

// NetworkFees is the platform's network_fee_expense account in c.
func NetworkFees(c money.Currency) AccountKey {
	return AccountKey{Code: NetworkFeeExpense, Currency: c}
}

// Payable is merchantID's merchant_payable account in c.
func Payable(merchantID string, c money.Currency) AccountKey {
	return AccountKey{MerchantID: merchantID, Code: MerchantPayable, Currency: c}
}

// Type returns the account's type.
func (k AccountKey) Type() AccountType { return k.Code.Type() }

func (k AccountKey) String() string {
	owner := k.MerchantID
	if owner == "" {
		owner = "platform"
	}
	return owner + "/" + string(k.Code) + "/" + k.Currency.Code()
}

func (k AccountKey) validate() error {
	entry, ok := chart[k.Code]
	switch {
	case !ok:
		return fmt.Errorf("%w: code %q", ErrUnknownAccount, k.Code)
	case k.Currency.IsZero():
		return fmt.Errorf("%w: %s has no currency", ErrUnknownAccount, k)
	case entry.perMerchant && k.MerchantID == "":
		return fmt.Errorf("%w: %s needs a merchant", ErrUnknownAccount, k.Code)
	case !entry.perMerchant && k.MerchantID != "":
		return fmt.Errorf("%w: %s is a platform account but has merchant %q", ErrUnknownAccount, k.Code, k.MerchantID)
	}
	return nil
}

// Posting is one line of a journal transaction: a signed amount on one
// account. Positive is a debit, negative is a credit.
type Posting struct {
	Account AccountKey
	Amount  money.Money
}

// Kind is why a journal transaction happened.
type Kind string

// Transaction kinds. Authorization has no kind: an authorization moves no
// money, it only reserves the cardholder's credit at their bank, so there is
// nothing to record in our books until capture.
const (
	KindCapture    Kind = "capture"
	KindRefund     Kind = "refund"
	KindSettlement Kind = "settlement"
	KindReversal   Kind = "reversal"
)

func (k Kind) valid() bool {
	switch k {
	case KindCapture, KindRefund, KindSettlement, KindReversal:
		return true
	}
	return false
}

// Tx is a journal transaction: postings recorded together, atomically.
type Tx struct {
	Kind        Kind
	PaymentID   string // the payment this belongs to; "" if none
	ExternalRef string // optional reference from outside, unique across the ledger
	ReversesID  string // ID of the transaction this reverses; set only on reversals
	Postings    []Posting
}

// Validate checks the rules the database also enforces: a known kind, at
// least two postings, every posting non-zero and in its account's currency,
// and postings summing to zero per currency.
//
// Debits and credits are totalled separately, each with overflow checking,
// so the result depends only on the totals and never on posting order. A
// transaction whose debits (or credits) in one currency exceed int64 is
// rejected with ErrTooLarge even if it balances. Postgres sums in numeric and
// would accept it, so the Go check is the stricter of the two.
func (t Tx) Validate() error {
	if !t.Kind.valid() {
		return fmt.Errorf("%w: %q", ErrInvalidKind, t.Kind)
	}
	if (t.Kind == KindReversal) != (t.ReversesID != "") {
		return fmt.Errorf("%w: a reversal, and only a reversal, must name the transaction it reverses", ErrInvalidKind)
	}
	if len(t.Postings) < 2 {
		return fmt.Errorf("%w: got %d", ErrTooFewPostings, len(t.Postings))
	}

	type totals struct{ debits, credits money.Money }
	var currencies []money.Currency // first-seen order, for deterministic errors
	sums := make(map[money.Currency]totals)
	for i, p := range t.Postings {
		if err := p.Account.validate(); err != nil {
			return fmt.Errorf("posting %d: %w", i, err)
		}
		c := p.Account.Currency
		if p.Amount.Currency() != c {
			return fmt.Errorf("%w: posting %d is %s on account %s", ErrAccountCurrency, i, p.Amount, p.Account)
		}
		if p.Amount.IsZero() {
			return fmt.Errorf("%w: posting %d on %s", ErrZeroPosting, i, p.Account)
		}
		s, seen := sums[c]
		if !seen {
			currencies = append(currencies, c)
			s = totals{debits: money.Zero(c), credits: money.Zero(c)}
		}
		var err error
		if p.Amount.Sign() > 0 {
			s.debits, err = s.debits.Add(p.Amount)
		} else {
			s.credits, err = s.credits.Add(p.Amount)
		}
		if err != nil {
			return fmt.Errorf("%w: %s postings", ErrTooLarge, c)
		}
		sums[c] = s
	}
	for _, c := range currencies {
		s := sums[c]
		// A positive plus a negative int64 can never overflow.
		if net, _ := s.debits.Add(s.credits); !net.IsZero() {
			return fmt.Errorf("%w: %s debits %s, credits %s", ErrUnbalanced, c, s.debits, s.credits)
		}
	}
	return nil
}

// Capture records captured funds: the network now owes us the gross amount,
// we owe the merchant the gross minus our fee, and the fee is our revenue.
//
//	debit  processor_receivable  amount
//	credit merchant_payable      amount - fee   (omitted if zero)
//	credit fee_revenue           fee            (omitted if zero)
func Capture(paymentID, merchantID string, amount, fee money.Money) (Tx, error) {
	if err := checkPositive("capture amount", amount); err != nil {
		return Tx{}, err
	}
	if err := checkPart("fee", fee, amount); err != nil {
		return Tx{}, err
	}
	c := amount.Currency()
	net, _ := amount.Sub(fee) // 0 <= fee <= amount, so this cannot fail
	postings := []Posting{{Receivable(c), amount}}
	if !net.IsZero() {
		postings = append(postings, Posting{Payable(merchantID, c), mustNeg(net)})
	}
	if !fee.IsZero() {
		postings = append(postings, Posting{Fees(c), mustNeg(fee)})
	}
	return Tx{Kind: KindCapture, PaymentID: paymentID, Postings: postings}, nil
}

// Refund returns money to the cardholder out of what we owe the merchant.
// Our fee is not refunded (see docs/adr/0002-fees-on-refunds.md), so a full
// refund leaves the merchant's balance negative by the fee.
//
//	debit  merchant_payable      amount
//	credit processor_receivable  amount
func Refund(paymentID, merchantID string, amount money.Money) (Tx, error) {
	if err := checkPositive("refund amount", amount); err != nil {
		return Tx{}, err
	}
	c := amount.Currency()
	return Tx{Kind: KindRefund, PaymentID: paymentID, Postings: []Posting{
		{Payable(merchantID, c), amount},
		{Receivable(c), mustNeg(amount)},
	}}, nil
}

// Settlement records the network paying us for a capture: cash lands in the
// bank net of the network's fee, and the receivable is cleared. externalRef
// is the bank's reference for the payout line, and it is required: it is what
// makes settlement idempotent (the database rejects a second use of it).
//
//	debit  bank_cash             gross - networkFee   (omitted if zero)
//	debit  network_fee_expense   networkFee           (omitted if zero)
//	credit processor_receivable  gross
func Settlement(paymentID, externalRef string, gross, networkFee money.Money) (Tx, error) {
	if externalRef == "" {
		return Tx{}, fmt.Errorf("%w: settlement needs an external reference", ErrInvalidAmount)
	}
	if err := checkPositive("settlement amount", gross); err != nil {
		return Tx{}, err
	}
	if err := checkPart("network fee", networkFee, gross); err != nil {
		return Tx{}, err
	}
	c := gross.Currency()
	cash, _ := gross.Sub(networkFee) // 0 <= fee <= gross
	var postings []Posting
	if !cash.IsZero() {
		postings = append(postings, Posting{Bank(c), cash})
	}
	if !networkFee.IsZero() {
		postings = append(postings, Posting{NetworkFees(c), networkFee})
	}
	postings = append(postings, Posting{Receivable(c), mustNeg(gross)})
	return Tx{Kind: KindSettlement, PaymentID: paymentID, ExternalRef: externalRef, Postings: postings}, nil
}

// Reverse returns a transaction that exactly undoes original, which was
// stored under originalID: the same accounts with every amount negated.
// Ledger rows are never edited; a mistake is corrected by a reversal plus,
// if needed, a new correct transaction. A reversal cannot itself be reversed.
func Reverse(originalID string, original Tx) (Tx, error) {
	if originalID == "" {
		return Tx{}, fmt.Errorf("%w: missing original transaction ID", ErrUnknownTransaction)
	}
	if original.Kind == KindReversal {
		return Tx{}, fmt.Errorf("%w: a reversal cannot be reversed", ErrInvalidKind)
	}
	postings := make([]Posting, len(original.Postings))
	for i, p := range original.Postings {
		neg, err := p.Amount.Neg()
		if err != nil {
			return Tx{}, fmt.Errorf("%w: %w", ErrTooLarge, err)
		}
		postings[i] = Posting{p.Account, neg}
	}
	return Tx{Kind: KindReversal, PaymentID: original.PaymentID, ReversesID: originalID, Postings: postings}, nil
}

// NormalBalance converts a debit-positive balance into the account's natural
// sign. Assets and expenses grow with debits; liabilities and revenue grow
// with credits. A merchant_payable whose signed balance is -9680 USD has a
// normal balance of 96.80 USD: we owe the merchant $96.80.
func NormalBalance(t AccountType, signed money.Money) (money.Money, error) {
	switch t {
	case Asset, Expense:
		return signed, nil
	case Liability, Revenue:
		return signed.Neg()
	default:
		return money.Money{}, fmt.Errorf("%w: account type %q", ErrUnknownAccount, t)
	}
}

func checkPositive(what string, m money.Money) error {
	if m.Currency().IsZero() || m.Sign() <= 0 {
		return fmt.Errorf("%w: %s must be positive, got %v", ErrInvalidAmount, what, m)
	}
	return nil
}

// checkPart requires 0 <= part <= whole, in the same currency.
func checkPart(what string, part, whole money.Money) error {
	cmp, err := part.Cmp(whole)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrInvalidAmount, what, err)
	}
	if part.Sign() < 0 || cmp > 0 {
		return fmt.Errorf("%w: %s %v must be between 0 and %v", ErrInvalidAmount, what, part, whole)
	}
	return nil
}

// mustNeg negates an amount known to be in (0, MaxInt64], which cannot fail.
func mustNeg(m money.Money) money.Money {
	n, err := m.Neg()
	if err != nil {
		panic("ledger: negating a positive amount failed: " + err.Error())
	}
	return n
}
