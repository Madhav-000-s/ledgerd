package ledger

import "errors"

// Sentinel errors. Callers match with errors.Is, and the HTTP layer maps each to a
// (type, code, status) triple in exactly one table, so that adding an error here cannot
// scatter new branches through the handlers.
var (
	// ErrUnbalanced means debits and credits did not agree (I1).
	ErrUnbalanced = errors.New("ledger: transaction is unbalanced")

	// ErrMultiCurrency means a transaction touched more than one currency (I2). A
	// cross-currency movement is two single-currency transactions linked by an FX
	// record, never one mixed transaction.
	ErrMultiCurrency = errors.New("ledger: transaction spans multiple currencies")

	// ErrInsufficientFunds means the movement would take an account below zero and the
	// account does not allow that (I6).
	ErrInsufficientFunds = errors.New("ledger: insufficient funds")

	// ErrInvalidAmount means an entry amount was zero or negative (I3). The sign of a
	// posting lives in its direction, never in its amount.
	ErrInvalidAmount = errors.New("ledger: entry amount must be positive")

	// ErrNoEntries means a transaction had no postings at all.
	ErrNoEntries = errors.New("ledger: transaction has no entries")

	// ErrAccountNotFound means an entry referenced an account that does not exist.
	ErrAccountNotFound = errors.New("ledger: account not found")

	// ErrTransactionNotFound means a capture, void or reversal named a transaction that
	// does not exist.
	ErrTransactionNotFound = errors.New("ledger: transaction not found")

	// ErrAccountExists means an account with that merchant and name already exists.
	ErrAccountExists = errors.New("ledger: account already exists")

	// ErrInvalidAccount means the account failed its own validation, including the
	// normal-balance/type agreement of I5.
	ErrInvalidAccount = errors.New("ledger: invalid account")

	// ErrInvalidAccountType means the account type is not one of the five.
	ErrInvalidAccountType = errors.New("ledger: invalid account type")

	// ErrInvalidDirection means a posting was neither a debit nor a credit.
	ErrInvalidDirection = errors.New("ledger: invalid direction")

	// ErrAccountCurrency means an entry's currency did not match its account's.
	ErrAccountCurrency = errors.New("ledger: entry currency does not match account")

	// ErrNotPending means a capture or void named a transaction that is not an
	// outstanding authorization.
	ErrNotPending = errors.New("ledger: transaction is not pending")

	// ErrNotPosted means a reversal named a transaction that was never posted.
	ErrNotPosted = errors.New("ledger: transaction is not posted")

	// ErrAlreadyReversed means the transaction already has a mirror. The database
	// enforces this too, with a unique index on reverses_id.
	ErrAlreadyReversed = errors.New("ledger: transaction already reversed")

	// ErrCaptureExceedsAuthorization means a capture asked for more than was held.
	ErrCaptureExceedsAuthorization = errors.New("ledger: capture exceeds authorized amount")

	// ErrInvalidCursor means a pagination cursor was malformed.
	ErrInvalidCursor = errors.New("ledger: invalid cursor")
)
