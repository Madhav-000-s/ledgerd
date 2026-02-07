package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/ledger/memory"
	"github.com/Madhav-000-s/ledgerd/internal/money"
)

const merchant = "mer_test"

// fixture is a small chart of accounts plus the service under test.
type fixture struct {
	t     *testing.T
	ctx   context.Context
	store *memory.Store
	svc   ledger.Service

	clearing string // asset,     debit-normal
	payable  string // liability, credit-normal
	fees     string // revenue,   credit-normal
	customer string // liability, credit-normal
	holds    string // liability, credit-normal
	costs    string // expense,   debit-normal
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	store := memory.New()
	f := &fixture{
		t:     t,
		ctx:   context.Background(),
		store: store,
		svc:   ledger.NewService(store, ledger.WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })),
	}

	f.clearing = f.account("platform_clearing", ledger.Asset, true)
	f.payable = f.account("merchant_payable", ledger.Liability, true)
	f.fees = f.account("fee_revenue", ledger.Revenue, true)
	f.customer = f.account("customer_balance", ledger.Liability, false)
	f.holds = f.account("authorization_holds", ledger.Liability, true)
	f.costs = f.account("processing_cost", ledger.Expense, true)

	return f
}

func (f *fixture) account(name string, typ ledger.AccountType, allowNegative bool) string {
	f.t.Helper()

	a, err := f.svc.CreateAccount(f.ctx, &ledger.Account{
		MerchantID:    merchant,
		Name:          name,
		Type:          typ,
		Currency:      money.USD,
		AllowNegative: allowNegative,
	})
	if err != nil {
		f.t.Fatalf("CreateAccount(%s) = %v", name, err)
	}
	return a.ID
}

func (f *fixture) balance(accountID string) ledger.Balance {
	f.t.Helper()

	b, err := f.svc.Balance(f.ctx, accountID)
	if err != nil {
		f.t.Fatalf("Balance(%s) = %v", accountID, err)
	}
	return b
}

func (f *fixture) post(spec ledger.TransactionSpec) *ledger.Transaction {
	f.t.Helper()

	txn, err := f.svc.Post(f.ctx, spec)
	if err != nil {
		f.t.Fatalf("Post = %v", err)
	}
	return txn
}

func usd(n int64) money.Money { return money.MustNew(n, money.USD) }

func entry(accountID string, d ledger.Direction, n int64) ledger.EntrySpec {
	return ledger.EntrySpec{AccountID: accountID, Direction: d, Amount: usd(n)}
}

// ── account model ───────────────────────────────────────────────────────────

// I5: the normal balance is a function of the type, and an account whose stored
// normal balance disagrees with its type must not be constructible.
func TestNormalBalanceFollowsAccountType(t *testing.T) {
	tests := []struct {
		typ  ledger.AccountType
		want ledger.Direction
	}{
		{ledger.Asset, ledger.Debit},
		{ledger.Expense, ledger.Debit},
		{ledger.Liability, ledger.Credit},
		{ledger.Equity, ledger.Credit},
		{ledger.Revenue, ledger.Credit},
	}

	for _, tc := range tests {
		got, err := tc.typ.NormalBalance()
		if err != nil {
			t.Errorf("%s.NormalBalance() = %v", tc.typ, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s.NormalBalance() = %s, want %s", tc.typ, got, tc.want)
		}
	}

	if _, err := ledger.AccountType("gizmo").NormalBalance(); !errors.Is(err, ledger.ErrInvalidAccountType) {
		t.Errorf("unknown type error = %v, want ErrInvalidAccountType", err)
	}
}

func TestAccountValidateRejectsMismatchedNormalBalance(t *testing.T) {
	a := &ledger.Account{
		ID: "acct_1", MerchantID: merchant, Name: "cash",
		Type: ledger.Asset, NormalBalance: ledger.Credit, // assets are debit-normal
		Currency: money.USD,
	}
	if err := a.Validate(); !errors.Is(err, ledger.ErrInvalidAccount) {
		t.Errorf("Validate = %v, want ErrInvalidAccount", err)
	}
}

// A debit raises a debit-normal account and lowers a credit-normal one. Every balance
// update in the system goes through this one function.
func TestSignedDelta(t *testing.T) {
	asset := &ledger.Account{Type: ledger.Asset, NormalBalance: ledger.Debit}
	liability := &ledger.Account{Type: ledger.Liability, NormalBalance: ledger.Credit}

	if got := asset.SignedDelta(ledger.Debit, 500); got != 500 {
		t.Errorf("asset debit delta = %d, want 500", got)
	}
	if got := asset.SignedDelta(ledger.Credit, 500); got != -500 {
		t.Errorf("asset credit delta = %d, want -500", got)
	}
	if got := liability.SignedDelta(ledger.Credit, 500); got != 500 {
		t.Errorf("liability credit delta = %d, want 500", got)
	}
	if got := liability.SignedDelta(ledger.Debit, 500); got != -500 {
		t.Errorf("liability debit delta = %d, want -500", got)
	}
}

// Available must not count an uncommitted credit. Treating a pending inbound amount as
// spendable is how a ledger lets a customer spend money that has not arrived.
func TestAvailableIgnoresPositivePending(t *testing.T) {
	tests := []struct {
		name    string
		posted  int64
		pending int64
		want    int64
	}{
		{"no pending", 1000, 0, 1000},
		{"hold reduces immediately", 1000, -400, 600},
		{"incoming pending does not count", 1000, 400, 1000},
		{"hold can exceed posted", 100, -400, -300},
	}

	for _, tc := range tests {
		b := ledger.Balance{Posted: tc.posted, Pending: tc.pending}
		if got := b.Available(); got != tc.want {
			t.Errorf("%s: Available() = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// ── posting ─────────────────────────────────────────────────────────────────

// The charge from DESIGN.md §5.1: $100.00 with a 2.9% + 30c fee.
func TestPostChargeWithPlatformFee(t *testing.T) {
	f := newFixture(t)

	txn := f.post(ledger.TransactionSpec{
		MerchantID:  merchant,
		Description: "charge",
		Entries: []ledger.EntrySpec{
			entry(f.clearing, ledger.Debit, 10000),
			entry(f.payable, ledger.Credit, 9680),
			entry(f.fees, ledger.Credit, 320),
		},
	})

	if txn.Status != ledger.StatusPosted {
		t.Errorf("status = %s, want posted", txn.Status)
	}
	if txn.PostedAt == nil {
		t.Error("PostedAt is nil for a posted transaction")
	}
	if len(txn.Entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(txn.Entries))
	}

	// Balances are stored in each account's normal direction, so all three are positive.
	if got := f.balance(f.clearing).Posted; got != 10000 {
		t.Errorf("clearing posted = %d, want 10000", got)
	}
	if got := f.balance(f.payable).Posted; got != 9680 {
		t.Errorf("payable posted = %d, want 9680", got)
	}
	if got := f.balance(f.fees).Posted; got != 320 {
		t.Errorf("fees posted = %d, want 320", got)
	}
}

// I1: a transaction whose debits and credits disagree must be refused before anything
// is written.
func TestPostRejectsUnbalanced(t *testing.T) {
	f := newFixture(t)

	_, err := f.svc.Post(f.ctx, ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			entry(f.clearing, ledger.Debit, 10000),
			entry(f.payable, ledger.Credit, 9999),
		},
	})
	if !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("Post = %v, want ErrUnbalanced", err)
	}

	if got := f.balance(f.clearing).Posted; got != 0 {
		t.Errorf("clearing posted = %d after a rejected transaction, want 0", got)
	}
	if n := len(f.store.AllEntries()); n != 0 {
		t.Errorf("%d entries written for a rejected transaction, want 0", n)
	}
}

// I2: one currency per transaction. A mixed transaction has no meaningful balance check.
func TestPostRejectsMultiCurrency(t *testing.T) {
	f := newFixture(t)

	_, err := f.svc.Post(f.ctx, ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			{AccountID: f.clearing, Direction: ledger.Debit, Amount: money.MustNew(10000, money.USD)},
			{AccountID: f.payable, Direction: ledger.Credit, Amount: money.MustNew(10000, money.EUR)},
		},
	})
	if !errors.Is(err, ledger.ErrMultiCurrency) {
		t.Fatalf("Post = %v, want ErrMultiCurrency", err)
	}
}

// I3: the sign of a posting lives in its direction, never in its amount.
func TestPostRejectsNonPositiveAmounts(t *testing.T) {
	f := newFixture(t)

	for _, amount := range []int64{0, -100} {
		_, err := f.svc.Post(f.ctx, ledger.TransactionSpec{
			MerchantID: merchant,
			Entries: []ledger.EntrySpec{
				entry(f.clearing, ledger.Debit, amount),
				entry(f.payable, ledger.Credit, amount),
			},
		})
		if !errors.Is(err, ledger.ErrInvalidAmount) && !errors.Is(err, ledger.ErrNoEntries) {
			t.Errorf("Post with amount %d = %v, want ErrInvalidAmount", amount, err)
		}
	}
}

func TestPostRejectsEmptyAndUnknownAccounts(t *testing.T) {
	f := newFixture(t)

	if _, err := f.svc.Post(f.ctx, ledger.TransactionSpec{MerchantID: merchant}); !errors.Is(err, ledger.ErrNoEntries) {
		t.Errorf("Post with no entries = %v, want ErrNoEntries", err)
	}

	_, err := f.svc.Post(f.ctx, ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			entry("acct_does_not_exist", ledger.Debit, 100),
			entry(f.payable, ledger.Credit, 100),
		},
	})
	if !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Errorf("Post with an unknown account = %v, want ErrAccountNotFound", err)
	}
}

// An entry must be denominated in its account's currency, or a USD entry could land on
// a EUR account and the transaction would still "balance".
func TestPostRejectsCurrencyMismatchWithAccount(t *testing.T) {
	f := newFixture(t)

	eur, err := f.svc.CreateAccount(f.ctx, &ledger.Account{
		MerchantID: merchant, Name: "eur_clearing", Type: ledger.Asset, Currency: money.EUR,
	})
	if err != nil {
		t.Fatalf("CreateAccount = %v", err)
	}

	_, err = f.svc.Post(f.ctx, ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			entry(f.clearing, ledger.Debit, 100),
			entry(eur.ID, ledger.Credit, 100), // USD entry against a EUR account
		},
	})
	if !errors.Is(err, ledger.ErrAccountCurrency) {
		t.Errorf("Post = %v, want ErrAccountCurrency", err)
	}
}

// I6: an account that does not allow a negative balance must not be taken below zero.
func TestPostRejectsOverdraft(t *testing.T) {
	f := newFixture(t)

	// Fund the customer with 5000.
	f.post(ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			entry(f.clearing, ledger.Debit, 5000),
			entry(f.customer, ledger.Credit, 5000),
		},
	})

	_, err := f.svc.Post(f.ctx, ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			entry(f.customer, ledger.Debit, 5001), // one more than they have
			entry(f.payable, ledger.Credit, 5001),
		},
	})
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("Post = %v, want ErrInsufficientFunds", err)
	}

	if got := f.balance(f.customer).Posted; got != 5000 {
		t.Errorf("customer posted = %d after a rejected overdraft, want 5000", got)
	}

	// Spending exactly the balance is allowed.
	f.post(ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			entry(f.customer, ledger.Debit, 5000),
			entry(f.payable, ledger.Credit, 5000),
		},
	})
	if got := f.balance(f.customer).Posted; got != 0 {
		t.Errorf("customer posted = %d, want 0", got)
	}
}

// Accounts flagged allow_negative are exempt: the platform's own clearing account goes
// negative in the ordinary course of business.
func TestPostAllowsNegativeWhenFlagged(t *testing.T) {
	f := newFixture(t)

	f.post(ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			entry(f.payable, ledger.Debit, 10000), // takes payable to -10000
			entry(f.clearing, ledger.Credit, 10000),
		},
	})

	if got := f.balance(f.payable).Posted; got != -10000 {
		t.Errorf("payable posted = %d, want -10000", got)
	}
}

// A transaction that touches the same account twice must see its own earlier effect,
// not the balance as it stood at the start.
func TestPostSameAccountTwice(t *testing.T) {
	f := newFixture(t)

	txn := f.post(ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			entry(f.clearing, ledger.Debit, 700),
			entry(f.clearing, ledger.Debit, 300),
			entry(f.payable, ledger.Credit, 1000),
		},
	})

	if got := f.balance(f.clearing).Posted; got != 1000 {
		t.Errorf("clearing posted = %d, want 1000", got)
	}
	// balance_after must show the running value, not the same number twice.
	if got := txn.Entries[0].BalanceAfter; got != 700 {
		t.Errorf("entry 0 balance_after = %d, want 700", got)
	}
	if got := txn.Entries[1].BalanceAfter; got != 1000 {
		t.Errorf("entry 1 balance_after = %d, want 1000", got)
	}
}

// balance_after is what lets the reconciler bisect to the exact entry where derived and
// materialized diverged, so it must replay to the final balance in id order.
func TestBalanceAfterReplaysToFinalBalance(t *testing.T) {
	f := newFixture(t)

	for i := 0; i < 20; i++ {
		f.post(ledger.TransactionSpec{
			MerchantID: merchant,
			Entries: []ledger.EntrySpec{
				entry(f.clearing, ledger.Debit, int64(100+i)),
				entry(f.payable, ledger.Credit, int64(100+i)),
			},
		})
	}

	var last int64
	for _, e := range f.store.AllEntries() {
		if e.AccountID == f.clearing {
			last = e.BalanceAfter
		}
	}
	if got := f.balance(f.clearing).Posted; got != last {
		t.Errorf("final balance %d does not match the last entry's balance_after %d", got, last)
	}
}

// ── authorization, capture and void ─────────────────────────────────────────

// A hold reduces availability immediately while leaving the posted balance untouched.
func TestAuthorizeMovesPendingNotPosted(t *testing.T) {
	f := newFixture(t)

	f.post(ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			entry(f.clearing, ledger.Debit, 20000),
			entry(f.customer, ledger.Credit, 20000),
		},
	})

	auth := f.post(ledger.TransactionSpec{
		MerchantID: merchant,
		Pending:    true,
		Entries: []ledger.EntrySpec{
			entry(f.customer, ledger.Debit, 10000),
			entry(f.holds, ledger.Credit, 10000),
		},
	})

	if auth.Status != ledger.StatusPending {
		t.Errorf("status = %s, want pending", auth.Status)
	}
	if auth.PostedAt != nil {
		t.Error("PostedAt is set on a pending authorization")
	}

	bal := f.balance(f.customer)
	if bal.Posted != 20000 {
		t.Errorf("posted = %d, want 20000 (a hold must not move it)", bal.Posted)
	}
	if bal.Pending != -10000 {
		t.Errorf("pending = %d, want -10000", bal.Pending)
	}
	if bal.Available() != 10000 {
		t.Errorf("available = %d, want 10000", bal.Available())
	}
}

func TestCaptureFull(t *testing.T) {
	f := newFixture(t)
	auth := f.authorize(20000, 10000)

	captured, err := f.svc.Capture(f.ctx, auth.ID, nil)
	if err != nil {
		t.Fatalf("Capture = %v", err)
	}
	if captured.Total() != 10000 {
		t.Errorf("captured total = %d, want 10000", captured.Total())
	}

	bal := f.balance(f.customer)
	if bal.Posted != 10000 {
		t.Errorf("posted = %d, want 10000", bal.Posted)
	}
	// The hold is released in full, so nothing pending remains.
	if bal.Pending != 0 {
		t.Errorf("pending = %d, want 0", bal.Pending)
	}
	if bal.Available() != 10000 {
		t.Errorf("available = %d, want 10000", bal.Available())
	}

	after, err := f.svc.GetTransaction(f.ctx, auth.ID)
	if err != nil {
		t.Fatalf("GetTransaction = %v", err)
	}
	if after.Status != ledger.StatusPosted {
		t.Errorf("authorization status = %s, want posted", after.Status)
	}
}

func TestCapturePartialReleasesRemainder(t *testing.T) {
	f := newFixture(t)
	auth := f.authorize(20000, 10000)

	amount := usd(6000)
	captured, err := f.svc.Capture(f.ctx, auth.ID, &amount)
	if err != nil {
		t.Fatalf("Capture = %v", err)
	}
	if captured.Total() != 6000 {
		t.Errorf("captured total = %d, want 6000", captured.Total())
	}

	bal := f.balance(f.customer)
	if bal.Posted != 14000 {
		t.Errorf("posted = %d, want 14000", bal.Posted)
	}
	if bal.Pending != 0 {
		t.Errorf("pending = %d, want 0 (the whole hold is consumed)", bal.Pending)
	}
	if bal.Available() != 14000 {
		t.Errorf("available = %d, want 14000", bal.Available())
	}
}

func TestCaptureRejectsMoreThanAuthorized(t *testing.T) {
	f := newFixture(t)
	auth := f.authorize(20000, 10000)

	amount := usd(10001)
	if _, err := f.svc.Capture(f.ctx, auth.ID, &amount); !errors.Is(err, ledger.ErrCaptureExceedsAuthorization) {
		t.Errorf("Capture = %v, want ErrCaptureExceedsAuthorization", err)
	}
}

func TestCaptureRejectsNonPending(t *testing.T) {
	f := newFixture(t)

	posted := f.post(ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			entry(f.clearing, ledger.Debit, 100),
			entry(f.payable, ledger.Credit, 100),
		},
	})

	if _, err := f.svc.Capture(f.ctx, posted.ID, nil); !errors.Is(err, ledger.ErrNotPending) {
		t.Errorf("Capture of a posted transaction = %v, want ErrNotPending", err)
	}
}

// A void leaves no posted entry at all. That is the difference between a void and a
// refund, and it is visible in the books.
func TestVoidReleasesHoldAndPostsNothing(t *testing.T) {
	f := newFixture(t)
	auth := f.authorize(20000, 10000)

	if _, err := f.svc.Void(f.ctx, auth.ID); err != nil {
		t.Fatalf("Void = %v", err)
	}

	bal := f.balance(f.customer)
	if bal.Posted != 20000 {
		t.Errorf("posted = %d, want 20000", bal.Posted)
	}
	if bal.Pending != 0 {
		t.Errorf("pending = %d, want 0", bal.Pending)
	}
	if bal.Available() != 20000 {
		t.Errorf("available = %d, want 20000", bal.Available())
	}

	after, _ := f.svc.GetTransaction(f.ctx, auth.ID)
	if after.Status != ledger.StatusVoided {
		t.Errorf("authorization status = %s, want voided", after.Status)
	}

	// No entry anywhere may be a posted movement against the customer.
	for _, e := range f.store.AllEntries() {
		if e.AccountID == f.customer && e.Status == ledger.StatusPosted && e.Direction == ledger.Debit {
			t.Errorf("a void produced a posted debit entry %d", e.ID)
		}
	}
}

// authorize funds the customer and places a hold, returning the authorization.
func (f *fixture) authorize(fund, hold int64) *ledger.Transaction {
	f.t.Helper()

	f.post(ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			entry(f.clearing, ledger.Debit, fund),
			entry(f.customer, ledger.Credit, fund),
		},
	})
	return f.post(ledger.TransactionSpec{
		MerchantID: merchant,
		Pending:    true,
		Entries: []ledger.EntrySpec{
			entry(f.customer, ledger.Debit, hold),
			entry(f.holds, ledger.Credit, hold),
		},
	})
}

// ── reversal ────────────────────────────────────────────────────────────────

func TestReverseRestoresBalances(t *testing.T) {
	f := newFixture(t)

	before := map[string]int64{
		f.clearing: f.balance(f.clearing).Posted,
		f.payable:  f.balance(f.payable).Posted,
		f.fees:     f.balance(f.fees).Posted,
	}

	txn := f.post(ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			entry(f.clearing, ledger.Debit, 10000),
			entry(f.payable, ledger.Credit, 9680),
			entry(f.fees, ledger.Credit, 320),
		},
	})

	rev, err := f.svc.Reverse(f.ctx, txn.ID, "customer disputed")
	if err != nil {
		t.Fatalf("Reverse = %v", err)
	}
	if rev.ReversesID != txn.ID {
		t.Errorf("ReversesID = %q, want %q", rev.ReversesID, txn.ID)
	}

	for accountID, want := range before {
		if got := f.balance(accountID).Posted; got != want {
			t.Errorf("account %s balance = %d after reversal, want %d", accountID, got, want)
		}
	}

	// Nothing was edited: the original entries are still there alongside the mirror.
	if n := len(f.store.AllEntries()); n != 6 {
		t.Errorf("%d entries after posting and reversing, want 6", n)
	}
}

func TestReverseRejectsDoubleReversal(t *testing.T) {
	f := newFixture(t)

	txn := f.post(ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			entry(f.clearing, ledger.Debit, 100),
			entry(f.payable, ledger.Credit, 100),
		},
	})

	if _, err := f.svc.Reverse(f.ctx, txn.ID, ""); err != nil {
		t.Fatalf("first Reverse = %v", err)
	}
	if _, err := f.svc.Reverse(f.ctx, txn.ID, ""); !errors.Is(err, ledger.ErrAlreadyReversed) {
		t.Errorf("second Reverse = %v, want ErrAlreadyReversed", err)
	}
}

func TestReverseRejectsPending(t *testing.T) {
	f := newFixture(t)
	auth := f.authorize(20000, 10000)

	if _, err := f.svc.Reverse(f.ctx, auth.ID, ""); !errors.Is(err, ledger.ErrNotPosted) {
		t.Errorf("Reverse of a pending transaction = %v, want ErrNotPosted", err)
	}
}

// ── pagination ──────────────────────────────────────────────────────────────

func TestListEntriesPaginates(t *testing.T) {
	f := newFixture(t)

	const n = 25
	for i := 0; i < n; i++ {
		f.post(ledger.TransactionSpec{
			MerchantID: merchant,
			Entries: []ledger.EntrySpec{
				entry(f.clearing, ledger.Debit, int64(i+1)),
				entry(f.payable, ledger.Credit, int64(i+1)),
			},
		})
	}

	var (
		seen   []int64
		cursor string
	)
	for pages := 0; ; pages++ {
		if pages > n {
			t.Fatal("pagination did not terminate")
		}
		batch, next, err := f.svc.ListEntries(f.ctx, f.clearing, ledger.Page{Limit: 10, StartingAfter: cursor})
		if err != nil {
			t.Fatalf("ListEntries = %v", err)
		}
		for _, e := range batch {
			seen = append(seen, e.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}

	if len(seen) != n {
		t.Fatalf("saw %d entries across all pages, want %d", len(seen), n)
	}
	// Newest first, and no row seen twice.
	for i := 1; i < len(seen); i++ {
		if seen[i] >= seen[i-1] {
			t.Fatalf("entries are not strictly newest-first at position %d: %v", i, seen)
		}
	}
}

func TestListEntriesRejectsBadCursor(t *testing.T) {
	f := newFixture(t)

	if _, _, err := f.svc.ListEntries(f.ctx, f.clearing, ledger.Page{StartingAfter: "not-a-cursor"}); !errors.Is(err, ledger.ErrInvalidCursor) {
		t.Errorf("ListEntries = %v, want ErrInvalidCursor", err)
	}
}

func TestPageNormalize(t *testing.T) {
	tests := []struct{ in, want int }{
		{0, ledger.DefaultPageLimit},
		{-5, ledger.DefaultPageLimit},
		{50, 50},
		{ledger.MaxPageLimit + 1, ledger.MaxPageLimit},
	}
	for _, tc := range tests {
		if got := (ledger.Page{Limit: tc.in}).Normalize().Limit; got != tc.want {
			t.Errorf("Page{Limit: %d}.Normalize() = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// ── cursors ─────────────────────────────────────────────────────────────────

func TestCursorRoundTrip(t *testing.T) {
	want := ledger.Cursor{CreatedAt: time.Unix(1_700_000_000, 123_456_789).UTC(), ID: 4242}

	got, err := ledger.ParseCursor(want.String())
	if err != nil {
		t.Fatalf("ParseCursor = %v", err)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || got.ID != want.ID {
		t.Errorf("round trip gave %+v, want %+v", got, want)
	}
}

func TestParseCursorRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "!!!", "YWJj", "MTIz"} {
		if _, err := ledger.ParseCursor(s); !errors.Is(err, ledger.ErrInvalidCursor) {
			t.Errorf("ParseCursor(%q) = %v, want ErrInvalidCursor", s, err)
		}
	}
}

// ── the append-only guarantee ───────────────────────────────────────────────

// The repository interface offers no way to modify or remove an entry. This test is a
// guard against someone adding one: if history becomes mutable, the ledger stops being
// auditable and every other guarantee here is worth less.
func TestEntriesAreAppendOnly(t *testing.T) {
	f := newFixture(t)

	txn := f.post(ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			entry(f.clearing, ledger.Debit, 100),
			entry(f.payable, ledger.Credit, 100),
		},
	})

	before := f.store.AllEntries()

	if _, err := f.svc.Reverse(f.ctx, txn.ID, "mistake"); err != nil {
		t.Fatalf("Reverse = %v", err)
	}

	after := f.store.AllEntries()
	if len(after) <= len(before) {
		t.Fatalf("reversal did not append: %d entries before, %d after", len(before), len(after))
	}
	for i, e := range before {
		if after[i] != e {
			t.Errorf("entry %d changed: %+v became %+v", i, e, after[i])
		}
	}
}
