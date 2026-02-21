package ledger

import (
	"context"
	"time"
)

// LockedAccount is an account together with its balance, both read under a lock held
// for the remainder of the enclosing database transaction.
type LockedAccount struct {
	Account Account
	Balance Balance
}

// BalanceUpdate is a new materialized balance for one account.
//
// Version carries the value that was read under the lock. The persistence layer writes
// with it in the WHERE clause, so a lost update turns into an error rather than a
// silently discarded write — belt and braces alongside the row lock that should already
// have prevented it.
type BalanceUpdate struct {
	AccountID string
	Posted    int64
	Pending   int64
	Version   int64
}

// Repository is the persistence seam.
//
// It is declared here, by the domain, and implemented by the postgres package. The
// dependency therefore points inward: ledger knows nothing about pgx, sqlc or SQL, and
// the whole invariant suite can run against an in-memory implementation in milliseconds.
//
// Every method is expected to run inside a database transaction supplied by the caller
// through ctx. The repository does not open transactions of its own — composing several
// repository calls into one atomic unit is the service layer's job, and is what makes
// "the ledger write and the outbox write commit together" possible.
type Repository interface {
	// CreateAccount inserts a new account. It returns ErrAccountExists if the
	// (merchant, name) pair is taken.
	CreateAccount(ctx context.Context, a *Account) error

	// GetAccount returns an account by id, or ErrAccountNotFound.
	GetAccount(ctx context.Context, id string) (*Account, error)

	// ListAccounts returns every account belonging to a merchant, sorted by id.
	ListAccounts(ctx context.Context, merchantID string) ([]*Account, error)

	// LockAccounts loads the named accounts and their balances, taking a row lock on
	// each. Implementations MUST acquire the locks in ascending account-id order: that
	// total order is what makes deadlock structurally impossible, and it is the reason
	// TransactionSpec.AccountIDs sorts before returning.
	//
	// It returns ErrAccountNotFound if any id is unknown.
	LockAccounts(ctx context.Context, ids []string) (map[string]LockedAccount, error)

	// ApplyBalances writes new materialized balances.
	ApplyBalances(ctx context.Context, updates []BalanceUpdate) error

	// GetBalance reads a balance without locking.
	GetBalance(ctx context.Context, accountID string) (Balance, error)

	// InsertTransaction inserts the transaction row.
	InsertTransaction(ctx context.Context, t *Transaction) error

	// SetTransactionStatus moves a transaction between statuses. Transactions are
	// mutable in this one respect; entries never are.
	SetTransactionStatus(ctx context.Context, id string, status Status, postedAt *time.Time) error

	// GetTransaction returns a transaction with its entries, or ErrTransactionNotFound.
	GetTransaction(ctx context.Context, id string) (*Transaction, error)

	// FindReversal returns the transaction that reverses reversesID, if one exists. It
	// returns ErrTransactionNotFound when there is none.
	FindReversal(ctx context.Context, reversesID string) (*Transaction, error)

	// TransactionsByRef returns every transaction belonging to one external object, in
	// creation order. A payment's public state is derived from these, so it cannot
	// disagree with the books.
	TransactionsByRef(ctx context.Context, merchantID, externalRef string) ([]*Transaction, error)

	// AppendEntries inserts entries and returns them with their assigned ids. There is
	// deliberately no update or delete: the append-only property is what makes the
	// ledger auditable, and it is enforced again in the database by a rule.
	AppendEntries(ctx context.Context, entries []Entry) ([]Entry, error)

	// ListEntries returns one page of an account's entries, newest first, along with
	// the cursor for the next page. An empty cursor means there are no more.
	ListEntries(ctx context.Context, accountID string, p Page) ([]Entry, string, error)

	// EntriesByTransaction returns every entry of a transaction, in seq order.
	EntriesByTransaction(ctx context.Context, txnID string) ([]Entry, error)
}
