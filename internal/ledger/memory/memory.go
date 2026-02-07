// Package memory is an in-memory ledger.Repository.
//
// It exists so the domain's invariants can be exercised in milliseconds without Docker,
// which is what makes the property suite practical to run on every save. It is a test
// double, not a second implementation of the ledger: it deliberately does NOT emulate
// database transactions or row locking, because a fake that pretended to would let a
// concurrency bug pass here and fail in production.
//
// The concurrency guarantees — ordered lock acquisition, deadlock freedom, no lost
// updates — are properties of Postgres and are tested against real Postgres in the
// integration suite. Nothing about them is asserted here.
package memory

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
)

// Store is an in-memory ledger repository. It is safe for concurrent use at the level
// of individual method calls; composing several into one atomic unit is the caller's
// problem, exactly as it is for the real repository.
type Store struct {
	mu sync.RWMutex

	accounts     map[string]*ledger.Account
	byName       map[string]string // merchantID\x00name -> account id
	balances     map[string]ledger.Balance
	transactions map[string]*ledger.Transaction
	entries      []ledger.Entry   // append-only, ordered by id
	byAccount    map[string][]int // account id -> indices into entries
	nextEntryID  int64
}

var _ ledger.Repository = (*Store)(nil)

// New returns an empty Store.
func New() *Store {
	return &Store{
		accounts:     make(map[string]*ledger.Account),
		byName:       make(map[string]string),
		balances:     make(map[string]ledger.Balance),
		transactions: make(map[string]*ledger.Transaction),
		byAccount:    make(map[string][]int),
		nextEntryID:  1,
	}
}

func nameKey(merchantID, name string) string { return merchantID + "\x00" + name }

func (s *Store) CreateAccount(ctx context.Context, a *ledger.Account) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.accounts[a.ID]; exists {
		return fmt.Errorf("%w: id %s", ledger.ErrAccountExists, a.ID)
	}
	key := nameKey(a.MerchantID, a.Name)
	if _, exists := s.byName[key]; exists {
		return fmt.Errorf("%w: %s/%s", ledger.ErrAccountExists, a.MerchantID, a.Name)
	}

	clone := *a
	s.accounts[a.ID] = &clone
	s.byName[key] = a.ID
	s.balances[a.ID] = ledger.Balance{
		AccountID: a.ID,
		Currency:  a.Currency,
		UpdatedAt: a.CreatedAt,
	}
	return nil
}

func (s *Store) GetAccount(ctx context.Context, accountID string) (*ledger.Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	a, ok := s.accounts[accountID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ledger.ErrAccountNotFound, accountID)
	}
	clone := *a
	return &clone, nil
}

func (s *Store) ListAccounts(ctx context.Context, merchantID string) ([]*ledger.Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*ledger.Account, 0)
	for _, a := range s.accounts {
		if a.MerchantID != merchantID {
			continue
		}
		clone := *a
		out = append(out, &clone)
	}
	sortAccounts(out)
	return out, nil
}

// LockAccounts returns accounts and balances. The name is kept for interface parity;
// no lock is taken, because there is no transaction here for one to be scoped to.
func (s *Store) LockAccounts(ctx context.Context, ids []string) (map[string]ledger.LockedAccount, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]ledger.LockedAccount, len(ids))
	for _, accountID := range ids {
		a, ok := s.accounts[accountID]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ledger.ErrAccountNotFound, accountID)
		}
		out[accountID] = ledger.LockedAccount{Account: *a, Balance: s.balances[accountID]}
	}
	return out, nil
}

func (s *Store) ApplyBalances(ctx context.Context, updates []ledger.BalanceUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, u := range updates {
		bal, ok := s.balances[u.AccountID]
		if !ok {
			return fmt.Errorf("%w: %s", ledger.ErrAccountNotFound, u.AccountID)
		}
		bal.Posted = u.Posted
		bal.Pending = u.Pending
		bal.Version = u.Version + 1
		bal.UpdatedAt = time.Now().UTC()
		s.balances[u.AccountID] = bal
	}
	return nil
}

func (s *Store) GetBalance(ctx context.Context, accountID string) (ledger.Balance, error) {
	if err := ctx.Err(); err != nil {
		return ledger.Balance{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	bal, ok := s.balances[accountID]
	if !ok {
		return ledger.Balance{}, fmt.Errorf("%w: %s", ledger.ErrAccountNotFound, accountID)
	}
	return bal, nil
}

func (s *Store) InsertTransaction(ctx context.Context, t *ledger.Transaction) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.transactions[t.ID]; exists {
		return fmt.Errorf("ledger: transaction %s already exists", t.ID)
	}
	clone := *t
	clone.Entries = nil // entries live in the entry log, not on the transaction row
	s.transactions[t.ID] = &clone
	return nil
}

func (s *Store) SetTransactionStatus(ctx context.Context, txnID string, status ledger.Status, postedAt *time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.transactions[txnID]
	if !ok {
		return fmt.Errorf("%w: %s", ledger.ErrTransactionNotFound, txnID)
	}
	t.Status = status
	if postedAt != nil {
		at := *postedAt
		t.PostedAt = &at
	}
	return nil
}

func (s *Store) GetTransaction(ctx context.Context, txnID string) (*ledger.Transaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	t, ok := s.transactions[txnID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ledger.ErrTransactionNotFound, txnID)
	}

	clone := *t
	for _, e := range s.entries {
		if e.TransactionID == txnID {
			clone.Entries = append(clone.Entries, e)
		}
	}
	return &clone, nil
}

func (s *Store) FindReversal(ctx context.Context, reversesID string) (*ledger.Transaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, t := range s.transactions {
		if t.ReversesID == reversesID {
			clone := *t
			return &clone, nil
		}
	}
	return nil, fmt.Errorf("%w: nothing reverses %s", ledger.ErrTransactionNotFound, reversesID)
}

// AppendEntries is append-only by construction: there is no method here that can modify
// or remove an entry once written, mirroring the database rule that makes the same
// guarantee against a manual psql session.
func (s *Store) AppendEntries(ctx context.Context, entries []ledger.Entry) ([]ledger.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]ledger.Entry, 0, len(entries))
	for _, e := range entries {
		e.ID = s.nextEntryID
		s.nextEntryID++

		s.byAccount[e.AccountID] = append(s.byAccount[e.AccountID], len(s.entries))
		s.entries = append(s.entries, e)
		out = append(out, e)
	}
	return out, nil
}

// ListEntries returns one page newest-first, using the (created_at, id) keyset rather
// than an offset.
func (s *Store) ListEntries(ctx context.Context, accountID string, p ledger.Page) ([]ledger.Entry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	p = p.Normalize()

	var after *ledger.Cursor
	if p.StartingAfter != "" {
		c, err := ledger.ParseCursor(p.StartingAfter)
		if err != nil {
			return nil, "", err
		}
		after = &c
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	idx := s.byAccount[accountID]
	out := make([]ledger.Entry, 0, p.Limit)

	// Newest first: walk the account's entries from the most recent backwards.
	for i := len(idx) - 1; i >= 0; i-- {
		e := s.entries[idx[i]]
		if after != nil && e.ID >= after.ID {
			continue
		}
		if len(out) == p.Limit {
			// One more row exists, so there is a next page.
			return out, ledger.CursorFor(out[len(out)-1]).String(), nil
		}
		out = append(out, e)
	}
	return out, "", nil
}

func (s *Store) EntriesByTransaction(ctx context.Context, txnID string) ([]ledger.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]ledger.Entry, 0, 2)
	for _, e := range s.entries {
		if e.TransactionID == txnID {
			out = append(out, e)
		}
	}
	return out, nil
}

// ── helpers used by tests to assert the invariants ──────────────────────────

// AllEntries returns every entry ever written, in id order. The reconciler's job, done
// by hand in a test.
func (s *Store) AllEntries() []ledger.Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]ledger.Entry, len(s.entries))
	copy(out, s.entries)
	return out
}

// AllAccounts returns every account, sorted by id.
func (s *Store) AllAccounts() []*ledger.Account {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*ledger.Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		clone := *a
		out = append(out, &clone)
	}
	sortAccounts(out)
	return out
}

// DerivedBalance recomputes an account's posted balance from the entry log, which is
// the source of truth. Comparing it against the materialized balance is invariant I7.
func (s *Store) DerivedBalance(accountID string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	account, ok := s.accounts[accountID]
	if !ok {
		return 0
	}

	var sum int64
	for _, i := range s.byAccount[accountID] {
		e := s.entries[i]
		if e.Status != ledger.StatusPosted {
			continue
		}
		sum += account.SignedDelta(e.Direction, e.Amount)
	}
	return sum
}

// DerivedPending recomputes an account's pending balance from the entry log.
func (s *Store) DerivedPending(accountID string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	account, ok := s.accounts[accountID]
	if !ok {
		return 0
	}

	var sum int64
	for _, i := range s.byAccount[accountID] {
		e := s.entries[i]
		if e.Status != ledger.StatusPending {
			continue
		}
		sum += account.SignedDelta(e.Direction, e.Amount)
	}
	return sum
}

func sortAccounts(as []*ledger.Account) {
	for i := 1; i < len(as); i++ {
		for j := i; j > 0 && as[j].ID < as[j-1].ID; j-- {
			as[j], as[j-1] = as[j-1], as[j]
		}
	}
}
