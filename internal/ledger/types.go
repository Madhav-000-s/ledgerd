// Package ledger is the double-entry core: accounts, immutable entries, balanced
// transactions, and the invariants that hold them together.
//
// The package is pure Go. It imports no database driver and no HTTP package; it
// declares the repository interface it needs and lets the persistence layer implement
// it. That is what makes the invariants testable in milliseconds without Docker, and it
// is why the property suite can run over thousands of generated transactions.
package ledger

import (
	"fmt"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/money"
)

// Direction is which side of the double entry a posting lands on. The sign of an entry
// lives here rather than in its amount, so that an amount is always positive and
// "negative debit" is not representable.
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// Opposite returns the other side. Reversal and hold release are both defined in terms
// of it.
func (d Direction) Opposite() Direction {
	if d == Debit {
		return Credit
	}
	return Debit
}

// Valid reports whether d is one of the two directions.
func (d Direction) Valid() bool { return d == Debit || d == Credit }

func (d Direction) String() string { return string(d) }

// AccountType is the account's role in the accounting equation. It determines the
// normal balance, which in turn determines whether a debit raises or lowers the balance.
type AccountType string

const (
	Asset     AccountType = "asset"     // platform_cash, clearing
	Liability AccountType = "liability" // merchant_payable:acct_123, customer_balance:cus_9
	Equity    AccountType = "equity"    // retained_earnings
	Revenue   AccountType = "revenue"   // fee_revenue
	Expense   AccountType = "expense"   // processing_cost, chargeback_loss
)

// NormalBalance returns the side on which this type of account normally carries a
// balance. Assets and expenses are debit-normal; everything else is credit-normal.
func (t AccountType) NormalBalance() (Direction, error) {
	switch t {
	case Asset, Expense:
		return Debit, nil
	case Liability, Equity, Revenue:
		return Credit, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidAccountType, string(t))
	}
}

// Valid reports whether t is a known account type.
func (t AccountType) Valid() bool {
	_, err := t.NormalBalance()
	return err == nil
}

// EquationSign is the account type's coefficient in
//
//	Σ assets − Σ liabilities − Σ equity − Σ revenue + Σ expenses == 0
//
// Every balance is stored in its own normal direction, so this restates all of them in
// debit-positive terms before summing. The reconciler uses it for I9.
func (t AccountType) EquationSign() int64 {
	switch t {
	case Asset, Expense:
		return 1
	default:
		return -1
	}
}

func (t AccountType) String() string { return string(t) }

// Status values shared by transactions and entries.
type Status string

const (
	StatusPending Status = "pending"
	StatusPosted  Status = "posted"
	StatusVoided  Status = "voided"
)

// Account is a named bucket in the chart of accounts.
//
// NormalBalance is derived from Type and stored alongside it for query convenience; the
// database carries a CHECK constraint that the two agree, so a row where they disagree
// cannot exist.
type Account struct {
	ID            string
	MerchantID    string
	Name          string
	Type          AccountType
	NormalBalance Direction
	Currency      money.Currency
	AllowNegative bool
	Metadata      map[string]any
	CreatedAt     time.Time
}

// Validate checks the account is internally consistent.
func (a *Account) Validate() error {
	if a.ID == "" {
		return fmt.Errorf("%w: account id is empty", ErrInvalidAccount)
	}
	if a.MerchantID == "" {
		return fmt.Errorf("%w: merchant id is empty", ErrInvalidAccount)
	}
	if a.Name == "" {
		return fmt.Errorf("%w: account name is empty", ErrInvalidAccount)
	}
	if !a.Currency.Valid() {
		return fmt.Errorf("%w: %q", money.ErrUnknownCurrency, string(a.Currency))
	}

	normal, err := a.Type.NormalBalance()
	if err != nil {
		return err
	}
	// I5: normal_balance must match type. Enforced here and by a database CHECK.
	if a.NormalBalance != normal {
		return fmt.Errorf("%w: %s accounts are %s-normal, got %s",
			ErrInvalidAccount, a.Type, normal, a.NormalBalance)
	}
	return nil
}

// SignedDelta converts a posting into the change it makes to this account's balance.
//
// Balances are stored in the account's normal direction, so a debit of 500 raises a
// debit-normal account by 500 and lowers a credit-normal one by the same. This is the
// single place that convention is applied; every balance update goes through it.
func (a *Account) SignedDelta(d Direction, amount int64) int64 {
	if d == a.NormalBalance {
		return amount
	}
	return -amount
}

// Balance is an account's materialized balance.
//
// Both figures are signed and expressed in the account's normal direction, so a
// negative value always means "this account is inverted from what it should be" — which
// is exactly the condition AllowNegative guards.
type Balance struct {
	AccountID string
	Currency  money.Currency
	Posted    int64
	Pending   int64
	Version   int64
	UpdatedAt time.Time
}

// Available is the balance an authorization may draw against.
//
// Pending movements that reduce the balance count immediately; pending movements that
// would raise it do not count until they post. Treating an uncommitted credit as
// spendable is how a ledger lets a customer spend money that has not arrived.
//
// It is computed rather than stored, so that there is no third number that can drift
// away from the other two.
func (b Balance) Available() int64 {
	if b.Pending < 0 {
		return b.Posted + b.Pending
	}
	return b.Posted
}

// Entry is a single posting: one side of one transaction against one account.
// Entries are append-only. A mistake is corrected by posting a compensating entry, so
// that history stays a fact rather than a mutable projection.
type Entry struct {
	ID            int64
	TransactionID string
	AccountID     string
	Direction     Direction
	Amount        int64 // always positive; the sign lives in Direction
	Currency      money.Currency
	Status        Status
	Seq           int16 // ordinal within the transaction
	BalanceAfter  int64 // the account's posted balance immediately after this entry
	CreatedAt     time.Time
}

// Money returns the entry's amount as a Money value.
func (e Entry) Money() money.Money {
	return money.MustNew(e.Amount, e.Currency)
}

// Transaction is a balanced set of entries applied atomically.
type Transaction struct {
	ID          string
	MerchantID  string
	Status      Status
	Currency    money.Currency
	Description string
	ReversesID  string // set when this transaction is the mirror of another
	ExternalRef string // payment or refund id
	Metadata    map[string]any
	CreatedAt   time.Time
	PostedAt    *time.Time

	Entries []Entry
}

// IsPending reports whether the transaction is an uncaptured authorization.
func (t *Transaction) IsPending() bool { return t.Status == StatusPending }

// Total returns the sum of the debits, which for a balanced transaction is also the sum
// of the credits and so is the transaction's magnitude.
func (t *Transaction) Total() int64 {
	var sum int64
	for _, e := range t.Entries {
		if e.Direction == Debit {
			sum += e.Amount
		}
	}
	return sum
}

// Page is a keyset pagination request. Offsets are never used: they degrade linearly
// and can skip or duplicate rows when concurrent inserts land, which is unacceptable
// for a ledger view.
type Page struct {
	Limit         int
	StartingAfter string // opaque cursor
}

// DefaultPageLimit and MaxPageLimit bound a page request.
const (
	DefaultPageLimit = 100
	MaxPageLimit     = 1000
)

// Normalize clamps the limit into range, so that a handler cannot pass an unbounded or
// negative page size down to the repository.
func (p Page) Normalize() Page {
	switch {
	case p.Limit <= 0:
		p.Limit = DefaultPageLimit
	case p.Limit > MaxPageLimit:
		p.Limit = MaxPageLimit
	}
	return p
}
