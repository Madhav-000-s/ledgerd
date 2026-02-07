package ledger_test

import (
	"errors"
	"math"
	"testing"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/money"
)

func TestDirection(t *testing.T) {
	if ledger.Debit.Opposite() != ledger.Credit {
		t.Error("Debit.Opposite() is not Credit")
	}
	if ledger.Credit.Opposite() != ledger.Debit {
		t.Error("Credit.Opposite() is not Debit")
	}
	if !ledger.Debit.Valid() || !ledger.Credit.Valid() {
		t.Error("a direction constant is not Valid")
	}
	if ledger.Direction("sideways").Valid() {
		t.Error("an unknown direction reported Valid")
	}
	if ledger.Debit.String() != "debit" {
		t.Errorf("Debit.String() = %q", ledger.Debit.String())
	}
}

func TestAccountTypeValid(t *testing.T) {
	for _, typ := range []ledger.AccountType{
		ledger.Asset, ledger.Liability, ledger.Equity, ledger.Revenue, ledger.Expense,
	} {
		if !typ.Valid() {
			t.Errorf("%s.Valid() = false", typ)
		}
	}
	if ledger.AccountType("gizmo").Valid() {
		t.Error("an unknown account type reported Valid")
	}
	if ledger.Asset.String() != "asset" {
		t.Errorf("Asset.String() = %q", ledger.Asset.String())
	}
}

// The equation sign restates every balance in debit-positive terms, which is what makes
// the trial balance and the accounting equation the same sum.
func TestEquationSign(t *testing.T) {
	tests := []struct {
		typ  ledger.AccountType
		want int64
	}{
		{ledger.Asset, 1},
		{ledger.Expense, 1},
		{ledger.Liability, -1},
		{ledger.Equity, -1},
		{ledger.Revenue, -1},
	}
	for _, tc := range tests {
		if got := tc.typ.EquationSign(); got != tc.want {
			t.Errorf("%s.EquationSign() = %d, want %d", tc.typ, got, tc.want)
		}
	}
}

func TestAccountValidate(t *testing.T) {
	valid := func() *ledger.Account {
		return &ledger.Account{
			ID: "acct_1", MerchantID: merchant, Name: "cash",
			Type: ledger.Asset, NormalBalance: ledger.Debit, Currency: money.USD,
		}
	}

	if err := valid().Validate(); err != nil {
		t.Fatalf("a valid account failed validation: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*ledger.Account)
		want   error
	}{
		{"no id", func(a *ledger.Account) { a.ID = "" }, ledger.ErrInvalidAccount},
		{"no merchant", func(a *ledger.Account) { a.MerchantID = "" }, ledger.ErrInvalidAccount},
		{"no name", func(a *ledger.Account) { a.Name = "" }, ledger.ErrInvalidAccount},
		{"unknown currency", func(a *ledger.Account) { a.Currency = "XYZ" }, money.ErrUnknownCurrency},
		{"unknown type", func(a *ledger.Account) { a.Type = "gizmo" }, ledger.ErrInvalidAccountType},
		{"mismatched normal", func(a *ledger.Account) { a.NormalBalance = ledger.Credit }, ledger.ErrInvalidAccount},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := valid()
			tc.mutate(a)
			if err := a.Validate(); !errors.Is(err, tc.want) {
				t.Errorf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSpecValidate(t *testing.T) {
	ok := func() ledger.TransactionSpec {
		return ledger.TransactionSpec{
			MerchantID: merchant,
			Entries: []ledger.EntrySpec{
				{AccountID: "acct_a", Direction: ledger.Debit, Amount: usd(100)},
				{AccountID: "acct_b", Direction: ledger.Credit, Amount: usd(100)},
			},
		}
	}

	if err := ok().Validate(); err != nil {
		t.Fatalf("a valid spec failed validation: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*ledger.TransactionSpec)
		want   error
	}{
		{
			"no merchant",
			func(s *ledger.TransactionSpec) { s.MerchantID = "" },
			ledger.ErrInvalidAccount,
		},
		{
			"no entries",
			func(s *ledger.TransactionSpec) { s.Entries = nil },
			ledger.ErrNoEntries,
		},
		{
			"entry without an account",
			func(s *ledger.TransactionSpec) { s.Entries[0].AccountID = "" },
			ledger.ErrInvalidAccount,
		},
		{
			"invalid direction",
			func(s *ledger.TransactionSpec) { s.Entries[0].Direction = "sideways" },
			ledger.ErrInvalidDirection,
		},
		{
			"zero amount",
			func(s *ledger.TransactionSpec) { s.Entries[0].Amount = usd(0) },
			ledger.ErrInvalidAmount,
		},
		{
			"negative amount",
			func(s *ledger.TransactionSpec) { s.Entries[0].Amount = usd(-100) },
			ledger.ErrInvalidAmount,
		},
		{
			"unbalanced",
			func(s *ledger.TransactionSpec) { s.Entries[1].Amount = usd(99) },
			ledger.ErrUnbalanced,
		},
		{
			"mixed currency",
			func(s *ledger.TransactionSpec) { s.Entries[1].Amount = money.MustNew(100, money.EUR) },
			ledger.ErrMultiCurrency,
		},
		{
			"unknown currency on the first entry",
			func(s *ledger.TransactionSpec) {
				s.Entries[0].Amount = money.Money{}
				s.Entries[1].Amount = money.Money{}
			},
			ledger.ErrInvalidAmount,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := ok()
			tc.mutate(&s)
			if err := s.Validate(); !errors.Is(err, tc.want) {
				t.Errorf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

// A wrapped total would balance against an equally wrapped other side and pass the
// check while being nonsense, so the sums are accumulated with overflow detection.
func TestSpecValidateDetectsOverflow(t *testing.T) {
	s := ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			{AccountID: "acct_a", Direction: ledger.Debit, Amount: usd(math.MaxInt64)},
			{AccountID: "acct_a", Direction: ledger.Debit, Amount: usd(1)},
			{AccountID: "acct_b", Direction: ledger.Credit, Amount: usd(math.MaxInt64)},
			{AccountID: "acct_b", Direction: ledger.Credit, Amount: usd(1)},
		},
	}
	if err := s.Validate(); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("Validate() = %v, want ErrOverflow", err)
	}
}

// AccountIDs is the lock acquisition order. Sorting is what makes deadlock structurally
// impossible, so the sort and the deduplication both matter.
func TestSpecAccountIDsAreSortedAndDeduplicated(t *testing.T) {
	s := ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			{AccountID: "acct_c", Direction: ledger.Debit, Amount: usd(50)},
			{AccountID: "acct_a", Direction: ledger.Debit, Amount: usd(50)},
			{AccountID: "acct_c", Direction: ledger.Credit, Amount: usd(50)},
			{AccountID: "acct_b", Direction: ledger.Credit, Amount: usd(50)},
		},
	}

	got := s.AccountIDs()
	want := []string{"acct_a", "acct_b", "acct_c"}

	if len(got) != len(want) {
		t.Fatalf("AccountIDs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AccountIDs() = %v, want %v", got, want)
		}
	}
}

func TestSpecCurrency(t *testing.T) {
	empty := ledger.TransactionSpec{}
	if got := empty.Currency(); got != "" {
		t.Errorf("Currency() of an empty spec = %q, want \"\"", got)
	}

	s := ledger.TransactionSpec{
		Entries: []ledger.EntrySpec{{AccountID: "acct_a", Direction: ledger.Debit, Amount: money.MustNew(1, money.JPY)}},
	}
	if got := s.Currency(); got != money.JPY {
		t.Errorf("Currency() = %q, want JPY", got)
	}
}

func TestServiceGetAccount(t *testing.T) {
	f := newFixture(t)

	got, err := f.svc.GetAccount(f.ctx, f.clearing)
	if err != nil {
		t.Fatalf("GetAccount = %v", err)
	}
	if got.ID != f.clearing {
		t.Errorf("id = %q, want %q", got.ID, f.clearing)
	}
	if got.Type != ledger.Asset || got.NormalBalance != ledger.Debit {
		t.Errorf("got %s/%s, want asset/debit", got.Type, got.NormalBalance)
	}

	if _, err := f.svc.GetAccount(f.ctx, "acct_missing"); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Errorf("GetAccount(missing) = %v, want ErrAccountNotFound", err)
	}
}

func TestCreateAccountRejectsDuplicateName(t *testing.T) {
	f := newFixture(t)

	_, err := f.svc.CreateAccount(f.ctx, &ledger.Account{
		MerchantID: merchant, Name: "platform_clearing", Type: ledger.Asset, Currency: money.USD,
	})
	if !errors.Is(err, ledger.ErrAccountExists) {
		t.Errorf("CreateAccount with a taken name = %v, want ErrAccountExists", err)
	}
}

func TestCreateAccountRejectsInvalidType(t *testing.T) {
	f := newFixture(t)

	_, err := f.svc.CreateAccount(f.ctx, &ledger.Account{
		MerchantID: merchant, Name: "weird", Type: "gizmo", Currency: money.USD,
	})
	if !errors.Is(err, ledger.ErrInvalidAccountType) {
		t.Errorf("CreateAccount = %v, want ErrInvalidAccountType", err)
	}
}

func TestListEntriesRejectsUnknownAccount(t *testing.T) {
	f := newFixture(t)

	if _, _, err := f.svc.ListEntries(f.ctx, "acct_missing", ledger.Page{}); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Errorf("ListEntries = %v, want ErrAccountNotFound", err)
	}
}

func TestCaptureAndVoidRejectUnknownTransaction(t *testing.T) {
	f := newFixture(t)

	if _, err := f.svc.Capture(f.ctx, "txn_missing", nil); !errors.Is(err, ledger.ErrTransactionNotFound) {
		t.Errorf("Capture = %v, want ErrTransactionNotFound", err)
	}
	if _, err := f.svc.Void(f.ctx, "txn_missing"); !errors.Is(err, ledger.ErrTransactionNotFound) {
		t.Errorf("Void = %v, want ErrTransactionNotFound", err)
	}
	if _, err := f.svc.Reverse(f.ctx, "txn_missing", ""); !errors.Is(err, ledger.ErrTransactionNotFound) {
		t.Errorf("Reverse = %v, want ErrTransactionNotFound", err)
	}
}

func TestCaptureRejectsBadAmounts(t *testing.T) {
	f := newFixture(t)
	auth := f.authorize(20000, 10000)

	zero := usd(0)
	if _, err := f.svc.Capture(f.ctx, auth.ID, &zero); !errors.Is(err, ledger.ErrInvalidAmount) {
		t.Errorf("Capture(0) = %v, want ErrInvalidAmount", err)
	}

	eur := money.MustNew(5000, money.EUR)
	if _, err := f.svc.Capture(f.ctx, auth.ID, &eur); !errors.Is(err, ledger.ErrMultiCurrency) {
		t.Errorf("Capture in another currency = %v, want ErrMultiCurrency", err)
	}
}

func TestVoidRejectsAlreadyVoided(t *testing.T) {
	f := newFixture(t)
	auth := f.authorize(20000, 10000)

	if _, err := f.svc.Void(f.ctx, auth.ID); err != nil {
		t.Fatalf("Void = %v", err)
	}
	if _, err := f.svc.Void(f.ctx, auth.ID); !errors.Is(err, ledger.ErrNotPending) {
		t.Errorf("second Void = %v, want ErrNotPending", err)
	}
}

// A partial capture across several entries per side must still balance: each side is
// allocated independently, so both sum to the captured amount.
func TestPartialCaptureOfMultiEntryAuthorizationStaysBalanced(t *testing.T) {
	f := newFixture(t)

	f.post(ledger.TransactionSpec{
		MerchantID: merchant,
		Entries: []ledger.EntrySpec{
			entry(f.clearing, ledger.Debit, 30000),
			entry(f.customer, ledger.Credit, 30000),
		},
	})

	auth := f.post(ledger.TransactionSpec{
		MerchantID: merchant,
		Pending:    true,
		Entries: []ledger.EntrySpec{
			// Three debits against two credits, so each side has to be allocated
			// independently for the partial capture to come out balanced.
			entry(f.customer, ledger.Debit, 7000),
			entry(f.customer, ledger.Debit, 3000),
			entry(f.costs, ledger.Debit, 1),
			entry(f.holds, ledger.Credit, 6000),
			entry(f.holds, ledger.Credit, 4001),
		},
	})

	amount := usd(3333)
	captured, err := f.svc.Capture(f.ctx, auth.ID, &amount)
	if err != nil {
		t.Fatalf("Capture = %v", err)
	}

	var debits, credits int64
	for _, e := range captured.Entries {
		if e.Direction == ledger.Debit {
			debits += e.Amount
		} else {
			credits += e.Amount
		}
	}
	if debits != credits {
		t.Errorf("partial capture is unbalanced: debits=%d credits=%d", debits, credits)
	}
	if debits != 3333 {
		t.Errorf("captured %d, want 3333", debits)
	}
}
