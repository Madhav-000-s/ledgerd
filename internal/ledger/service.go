package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/money"
	"github.com/Madhav-000-s/ledgerd/internal/platform/id"
)

// Service is the ledger's public surface.
//
// Post is the only write primitive. Capture, Void and Reverse are all defined in terms
// of posting further transactions, because entries are immutable: nothing here ever
// edits history, it only adds to it.
type Service interface {
	// Post validates, locks, writes entries and updates balances inside the caller's
	// transaction. It fails with ErrUnbalanced, ErrInsufficientFunds, ErrMultiCurrency,
	// ErrAccountCurrency or ErrAccountNotFound.
	Post(ctx context.Context, spec TransactionSpec) (*Transaction, error)

	// Capture converts a pending authorization into a posted movement, optionally for
	// less than was authorized, releasing the remainder.
	Capture(ctx context.Context, txnID string, amount *money.Money) (*Transaction, error)

	// Void releases an authorization in full. No posted entry ever exists for it.
	Void(ctx context.Context, txnID string) (*Transaction, error)

	// Reverse posts the mirror image of a posted transaction. This is how a correction
	// is made: never by editing what was written.
	Reverse(ctx context.Context, txnID, reason string) (*Transaction, error)

	// CreateAccount adds an account to the chart of accounts.
	CreateAccount(ctx context.Context, a *Account) (*Account, error)

	// GetAccount returns an account by id.
	GetAccount(ctx context.Context, accountID string) (*Account, error)

	// ListAccounts returns a merchant's chart of accounts.
	ListAccounts(ctx context.Context, merchantID string) ([]*Account, error)

	// Balance returns an account's materialized balance.
	Balance(ctx context.Context, accountID string) (Balance, error)

	// GetTransaction returns a transaction with its entries expanded.
	GetTransaction(ctx context.Context, txnID string) (*Transaction, error)

	// TransactionsByRef returns every transaction belonging to one external object.
	TransactionsByRef(ctx context.Context, merchantID, externalRef string) ([]*Transaction, error)

	// ListEntries returns one page of an account's ledger view and the next cursor.
	ListEntries(ctx context.Context, accountID string, p Page) ([]Entry, string, error)
}

type service struct {
	repo Repository
	now  func() time.Time
}

// Option configures a Service.
type Option func(*service)

// WithClock replaces the clock. Tests use it to make timestamps deterministic.
func WithClock(now func() time.Time) Option {
	return func(s *service) { s.now = now }
}

// NewService returns a Service backed by repo.
func NewService(repo Repository, opts ...Option) Service {
	s := &service{repo: repo, now: func() time.Time { return time.Now().UTC() }}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *service) CreateAccount(ctx context.Context, a *Account) (*Account, error) {
	if a.ID == "" {
		a.ID = id.New(id.PrefixAccount)
	}
	if a.NormalBalance == "" {
		normal, err := a.Type.NormalBalance()
		if err != nil {
			return nil, err
		}
		a.NormalBalance = normal
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = s.now()
	}
	if a.Metadata == nil {
		a.Metadata = map[string]any{}
	}

	if err := a.Validate(); err != nil {
		return nil, err
	}
	if err := s.repo.CreateAccount(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *service) GetAccount(ctx context.Context, accountID string) (*Account, error) {
	return s.repo.GetAccount(ctx, accountID)
}

func (s *service) ListAccounts(ctx context.Context, merchantID string) ([]*Account, error) {
	return s.repo.ListAccounts(ctx, merchantID)
}

func (s *service) TransactionsByRef(ctx context.Context, merchantID, externalRef string) ([]*Transaction, error) {
	return s.repo.TransactionsByRef(ctx, merchantID, externalRef)
}

func (s *service) Balance(ctx context.Context, accountID string) (Balance, error) {
	return s.repo.GetBalance(ctx, accountID)
}

func (s *service) GetTransaction(ctx context.Context, txnID string) (*Transaction, error) {
	return s.repo.GetTransaction(ctx, txnID)
}

func (s *service) ListEntries(ctx context.Context, accountID string, p Page) ([]Entry, string, error) {
	if _, err := s.repo.GetAccount(ctx, accountID); err != nil {
		return nil, "", err
	}
	return s.repo.ListEntries(ctx, accountID, p.Normalize())
}

// Post is the critical section described in DESIGN.md §6.1. The ordering of the steps
// is load-bearing:
//
//  1. validate the spec           — cheap, and fails before any lock is taken
//  2. lock every touched account in ascending id order
//  3. validate against the locked state (currency, sufficient funds)
//  4. write the transaction, the entries, and the new balances
//
// Step 2's ordering is what makes deadlock impossible; step 3 must happen after the
// lock or its answer is stale by the time it is used.
func (s *service) Post(ctx context.Context, spec TransactionSpec) (*Transaction, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	locked, err := s.repo.LockAccounts(ctx, spec.AccountIDs())
	if err != nil {
		return nil, err
	}

	txn := &Transaction{
		ID:          id.New(id.PrefixTransaction),
		MerchantID:  spec.MerchantID,
		Currency:    spec.Currency(),
		Description: spec.Description,
		ExternalRef: spec.ExternalRef,
		Metadata:    spec.Metadata,
		CreatedAt:   s.now(),
	}
	if spec.Pending {
		txn.Status = StatusPending
	} else {
		txn.Status = StatusPosted
		posted := txn.CreatedAt
		txn.PostedAt = &posted
	}
	if txn.Metadata == nil {
		txn.Metadata = map[string]any{}
	}

	entries, updates, err := s.apply(spec, locked, txn.ID, txn.CreatedAt)
	if err != nil {
		return nil, err
	}

	if err := s.repo.InsertTransaction(ctx, txn); err != nil {
		return nil, err
	}
	written, err := s.repo.AppendEntries(ctx, entries)
	if err != nil {
		return nil, err
	}
	if err := s.repo.ApplyBalances(ctx, updates); err != nil {
		return nil, err
	}

	txn.Entries = written
	return txn, nil
}

// apply turns a validated spec plus locked state into the entries to append and the
// balances to write. It is where the balance convention and invariant I6 live, and it
// performs no I/O so it can be reasoned about — and tested — in isolation.
func (s *service) apply(
	spec TransactionSpec,
	locked map[string]LockedAccount,
	txnID string,
	at time.Time,
) ([]Entry, []BalanceUpdate, error) {
	// Working copy of every touched balance, so that a transaction touching the same
	// account twice sees its own earlier effect.
	running := make(map[string]Balance, len(locked))
	for accountID, la := range locked {
		running[accountID] = la.Balance
	}

	entries := make([]Entry, 0, len(spec.Entries))

	for i, es := range spec.Entries {
		la, ok := locked[es.AccountID]
		if !ok {
			return nil, nil, fmt.Errorf("%w: %s", ErrAccountNotFound, es.AccountID)
		}
		account := la.Account

		// An entry must be denominated in its account's currency. Without this a USD
		// entry could land on a EUR account and the transaction would still "balance".
		if es.Amount.Currency() != account.Currency {
			return nil, nil, fmt.Errorf("%w: account %s is %s, entry is %s",
				ErrAccountCurrency, account.ID, account.Currency, es.Amount.Currency())
		}

		amount := es.Amount.Amount()
		delta := account.SignedDelta(es.Direction, amount)

		bal := running[es.AccountID]
		var ok2 bool
		if spec.Pending {
			if bal.Pending, ok2 = addInt64(bal.Pending, delta); !ok2 {
				return nil, nil, fmt.Errorf("%w: pending balance of %s", money.ErrOverflow, account.ID)
			}
		} else {
			if bal.Posted, ok2 = addInt64(bal.Posted, delta); !ok2 {
				return nil, nil, fmt.Errorf("%w: posted balance of %s", money.ErrOverflow, account.ID)
			}
		}
		running[es.AccountID] = bal

		entries = append(entries, Entry{
			TransactionID: txnID,
			AccountID:     es.AccountID,
			Direction:     es.Direction,
			Amount:        amount,
			Currency:      es.Amount.Currency(),
			Status:        statusFor(spec.Pending),
			Seq:           int16(i),
			// The account's posted balance immediately after this entry. A pending
			// entry does not move the posted balance, so it records the running value
			// unchanged. This is what lets the reconciler replay entries in id order
			// and bisect to the exact entry where derived and materialized diverged.
			BalanceAfter: bal.Posted,
			CreatedAt:    at,
		})
	}

	// I6, checked once against the final state rather than per entry: a transaction is
	// atomic, so an intermediate dip below zero that the transaction itself corrects is
	// not an overdraft.
	updates := make([]BalanceUpdate, 0, len(running))
	for accountID, bal := range running {
		account := locked[accountID].Account
		if !account.AllowNegative && bal.Available() < 0 {
			return nil, nil, fmt.Errorf("%w: account %s has available balance %d, transaction needs %d more",
				ErrInsufficientFunds, accountID, locked[accountID].Balance.Available(), -bal.Available())
		}
		updates = append(updates, BalanceUpdate{
			AccountID: accountID,
			Posted:    bal.Posted,
			Pending:   bal.Pending,
			Version:   bal.Version,
		})
	}
	sortUpdates(updates)

	return entries, updates, nil
}

// statusFor maps a spec's pending flag onto the status its entries are written with.
//
// Only Pending and Posted are ever written. StatusVoided never appears on an entry:
// entries are immutable, so a hold is released by appending a compensating pending
// entry rather than by changing the original's status. Voided is a transaction-level
// state.
func statusFor(pending bool) Status {
	if pending {
		return StatusPending
	}
	return StatusPosted
}

// Capture converts an authorization into a real movement.
//
// The hold is released in full and the captured amount is posted, both in the same
// transaction. A partial capture therefore releases the whole hold and posts less than
// it, rather than trying to leave part of the hold outstanding — an authorization is
// consumed once.
func (s *service) Capture(ctx context.Context, txnID string, amount *money.Money) (*Transaction, error) {
	auth, err := s.repo.GetTransaction(ctx, txnID)
	if err != nil {
		return nil, err
	}
	if !auth.IsPending() {
		return nil, fmt.Errorf("%w: %s is %s", ErrNotPending, txnID, auth.Status)
	}
	if len(auth.Entries) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoEntries, txnID)
	}

	authorized := auth.Total()
	capture := authorized
	if amount != nil {
		if amount.Currency() != auth.Currency {
			return nil, fmt.Errorf("%w: authorization is %s, capture is %s",
				ErrMultiCurrency, auth.Currency, amount.Currency())
		}
		capture = amount.Amount()
		if capture <= 0 {
			return nil, fmt.Errorf("%w: capture amount is %d", ErrInvalidAmount, capture)
		}
		if capture > authorized {
			return nil, fmt.Errorf("%w: authorized %d, requested %d",
				ErrCaptureExceedsAuthorization, authorized, capture)
		}
	}

	// Release the hold in full: every original entry, mirrored, still pending.
	release := make([]EntrySpec, 0, len(auth.Entries))
	for _, e := range auth.Entries {
		release = append(release, EntrySpec{
			AccountID: e.AccountID,
			Direction: e.Direction.Opposite(),
			Amount:    e.Money(),
		})
	}
	releaseSpec := TransactionSpec{
		MerchantID:  auth.MerchantID,
		Description: "release of authorization " + auth.ID,
		ExternalRef: auth.ExternalRef,
		Entries:     release,
		Pending:     true,
		Metadata:    map[string]any{"releases": auth.ID},
	}
	if _, err := s.Post(ctx, releaseSpec); err != nil {
		return nil, err
	}

	// Post the captured amount in the shape of the original.
	postEntries, err := scaleEntries(auth.Entries, capture, authorized)
	if err != nil {
		return nil, err
	}
	captureSpec := TransactionSpec{
		MerchantID:  auth.MerchantID,
		Description: "capture of authorization " + auth.ID,
		ExternalRef: auth.ExternalRef,
		Entries:     postEntries,
		Metadata:    map[string]any{"captures": auth.ID},
	}
	captured, err := s.Post(ctx, captureSpec)
	if err != nil {
		return nil, err
	}

	posted := s.now()
	if err := s.repo.SetTransactionStatus(ctx, auth.ID, StatusPosted, &posted); err != nil {
		return nil, err
	}

	return captured, nil
}

// Void releases an authorization in full. No posted entry ever exists for it, which is
// the difference between a void and a refund.
func (s *service) Void(ctx context.Context, txnID string) (*Transaction, error) {
	auth, err := s.repo.GetTransaction(ctx, txnID)
	if err != nil {
		return nil, err
	}
	if !auth.IsPending() {
		return nil, fmt.Errorf("%w: %s is %s", ErrNotPending, txnID, auth.Status)
	}

	release := make([]EntrySpec, 0, len(auth.Entries))
	for _, e := range auth.Entries {
		release = append(release, EntrySpec{
			AccountID: e.AccountID,
			Direction: e.Direction.Opposite(),
			Amount:    e.Money(),
		})
	}

	voided, err := s.Post(ctx, TransactionSpec{
		MerchantID:  auth.MerchantID,
		Description: "void of authorization " + auth.ID,
		ExternalRef: auth.ExternalRef,
		Entries:     release,
		Pending:     true,
		Metadata:    map[string]any{"voids": auth.ID},
	})
	if err != nil {
		return nil, err
	}

	if err := s.repo.SetTransactionStatus(ctx, auth.ID, StatusVoided, nil); err != nil {
		return nil, err
	}
	return voided, nil
}

// Reverse posts the mirror image of a posted transaction.
//
// This is the only correction mechanism. Nothing updates or deletes an entry, so the
// history of a mistake and of its correction are both permanent facts — which is what
// makes the ledger auditable rather than merely current.
func (s *service) Reverse(ctx context.Context, txnID, reason string) (*Transaction, error) {
	orig, err := s.repo.GetTransaction(ctx, txnID)
	if err != nil {
		return nil, err
	}
	if orig.Status != StatusPosted {
		return nil, fmt.Errorf("%w: %s is %s", ErrNotPosted, txnID, orig.Status)
	}

	// The database carries a unique index on reverses_id; checking here turns the race
	// loser's constraint violation into a precise error for the common case.
	if existing, err := s.repo.FindReversal(ctx, txnID); err == nil && existing != nil {
		return nil, fmt.Errorf("%w: %s is reversed by %s", ErrAlreadyReversed, txnID, existing.ID)
	}

	mirror := make([]EntrySpec, 0, len(orig.Entries))
	for _, e := range orig.Entries {
		mirror = append(mirror, EntrySpec{
			AccountID: e.AccountID,
			Direction: e.Direction.Opposite(),
			Amount:    e.Money(),
		})
	}

	description := "reversal of " + orig.ID
	if reason != "" {
		description += ": " + reason
	}

	spec := TransactionSpec{
		MerchantID:  orig.MerchantID,
		Description: description,
		ExternalRef: orig.ExternalRef,
		Entries:     mirror,
		Metadata:    map[string]any{"reverses": orig.ID, "reason": reason},
	}

	if err := spec.Validate(); err != nil {
		return nil, err
	}

	locked, err := s.repo.LockAccounts(ctx, spec.AccountIDs())
	if err != nil {
		return nil, err
	}

	txn := &Transaction{
		ID:          id.New(id.PrefixTransaction),
		MerchantID:  spec.MerchantID,
		Status:      StatusPosted,
		Currency:    spec.Currency(),
		Description: spec.Description,
		ReversesID:  orig.ID,
		ExternalRef: spec.ExternalRef,
		Metadata:    spec.Metadata,
		CreatedAt:   s.now(),
	}
	posted := txn.CreatedAt
	txn.PostedAt = &posted

	entries, updates, err := s.apply(spec, locked, txn.ID, txn.CreatedAt)
	if err != nil {
		return nil, err
	}
	if err := s.repo.InsertTransaction(ctx, txn); err != nil {
		return nil, err
	}
	written, err := s.repo.AppendEntries(ctx, entries)
	if err != nil {
		return nil, err
	}
	if err := s.repo.ApplyBalances(ctx, updates); err != nil {
		return nil, err
	}

	txn.Entries = written
	return txn, nil
}

// scaleEntries rewrites a set of balanced entries to a smaller total, keeping them
// balanced.
//
// Debits and credits are allocated independently, each across the original amounts as
// ratios. Because the original debits and credits both sum to authorized, each side
// sums to capture afterwards, so the result balances by construction rather than by
// arithmetic that happens to work out. Allocate guarantees each side sums exactly, so
// no rounding crumb is left behind.
func scaleEntries(entries []Entry, capture, authorized int64) ([]EntrySpec, error) {
	if authorized <= 0 {
		return nil, fmt.Errorf("%w: authorized total is %d", ErrInvalidAmount, authorized)
	}
	if capture == authorized {
		out := make([]EntrySpec, 0, len(entries))
		for _, e := range entries {
			out = append(out, EntrySpec{AccountID: e.AccountID, Direction: e.Direction, Amount: e.Money()})
		}
		return out, nil
	}

	var (
		debitIdx, creditIdx     []int
		debitRatio, creditRatio []int64
		currency                money.Currency
	)
	for i, e := range entries {
		currency = e.Currency
		if e.Direction == Debit {
			debitIdx = append(debitIdx, i)
			debitRatio = append(debitRatio, e.Amount)
		} else {
			creditIdx = append(creditIdx, i)
			creditRatio = append(creditRatio, e.Amount)
		}
	}

	total := money.MustNew(capture, currency)
	debitParts := total.Allocate(debitRatio)
	creditParts := total.Allocate(creditRatio)

	out := make([]EntrySpec, len(entries))
	for n, i := range debitIdx {
		out[i] = EntrySpec{AccountID: entries[i].AccountID, Direction: Debit, Amount: debitParts[n]}
	}
	for n, i := range creditIdx {
		out[i] = EntrySpec{AccountID: entries[i].AccountID, Direction: Credit, Amount: creditParts[n]}
	}

	// A part can round to zero when the capture is small relative to the number of
	// entries. Zero-amount entries are invalid (I3), so drop them; both sides still sum
	// to capture, so the transaction still balances.
	kept := make([]EntrySpec, 0, len(out))
	for _, es := range out {
		if es.Amount.Amount() != 0 {
			kept = append(kept, es)
		}
	}
	return kept, nil
}

// sortUpdates puts balance writes in ascending account order, matching the order the
// locks were taken in.
func sortUpdates(us []BalanceUpdate) {
	for i := 1; i < len(us); i++ {
		for j := i; j > 0 && us[j].AccountID < us[j-1].AccountID; j-- {
			us[j], us[j-1] = us[j-1], us[j]
		}
	}
}
