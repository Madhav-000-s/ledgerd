package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/ledger/memory"
	"github.com/Madhav-000-s/ledgerd/internal/money"
	"pgregory.net/rapid"
)

// chart is a randomly generated set of accounts plus the service that writes to them.
type chart struct {
	store    *memory.Store
	svc      ledger.Service
	accounts []*ledger.Account
}

// newChart builds a random chart of accounts spanning all five account types, so that
// both normal balances are exercised.
func newChart(t *rapid.T) *chart {
	store := memory.New()
	svc := ledger.NewService(store)
	ctx := context.Background()

	types := []ledger.AccountType{ledger.Asset, ledger.Liability, ledger.Equity, ledger.Revenue, ledger.Expense}

	n := rapid.IntRange(3, 10).Draw(t, "accounts")
	c := &chart{store: store, svc: svc}

	for i := 0; i < n; i++ {
		typ := types[rapid.IntRange(0, len(types)-1).Draw(t, "type")]
		a, err := svc.CreateAccount(ctx, &ledger.Account{
			MerchantID: merchant,
			Name:       "acct" + string(rune('a'+i)),
			Type:       typ,
			Currency:   money.USD,
			// Most accounts permit a negative balance so that transactions mostly
			// succeed; the ones that do not exercise I6.
			AllowNegative: rapid.IntRange(0, 4).Draw(t, "allow_negative") > 0,
		})
		if err != nil {
			t.Fatalf("CreateAccount = %v", err)
		}
		c.accounts = append(c.accounts, a)
	}
	return c
}

// drawSpec generates a balanced transaction. Each side's amounts are produced by
// Allocate over random ratios, so the two sides sum to the same total by construction
// rather than by the generator getting the arithmetic right.
func (c *chart) drawSpec(t *rapid.T) ledger.TransactionSpec {
	total := rapid.Int64Range(1, 1_000_000).Draw(t, "total")
	pending := rapid.Bool().Draw(t, "pending")

	nDebit := rapid.IntRange(1, 3).Draw(t, "n_debit")
	nCredit := rapid.IntRange(1, 3).Draw(t, "n_credit")

	pick := func(n int, label string) ([]*ledger.Account, []int64) {
		accts := make([]*ledger.Account, n)
		ratios := make([]int64, n)
		for i := range accts {
			accts[i] = c.accounts[rapid.IntRange(0, len(c.accounts)-1).Draw(t, label)]
			ratios[i] = rapid.Int64Range(1, 100).Draw(t, label+"_ratio")
		}
		return accts, ratios
	}

	debitAccts, debitRatios := pick(nDebit, "debit_account")
	creditAccts, creditRatios := pick(nCredit, "credit_account")

	amount := money.MustNew(total, money.USD)
	debitParts := amount.Allocate(debitRatios)
	creditParts := amount.Allocate(creditRatios)

	entries := make([]ledger.EntrySpec, 0, nDebit+nCredit)
	for i, a := range debitAccts {
		if debitParts[i].Amount() == 0 {
			continue // I3 forbids zero-amount entries
		}
		entries = append(entries, ledger.EntrySpec{AccountID: a.ID, Direction: ledger.Debit, Amount: debitParts[i]})
	}
	for i, a := range creditAccts {
		if creditParts[i].Amount() == 0 {
			continue
		}
		entries = append(entries, ledger.EntrySpec{AccountID: a.ID, Direction: ledger.Credit, Amount: creditParts[i]})
	}

	return ledger.TransactionSpec{
		MerchantID: merchant,
		Entries:    entries,
		Pending:    pending,
	}
}

// trialBalance restates every materialized balance in debit-positive terms and sums
// them. Balances are stored in each account's own normal direction, so this is both the
// trial balance (P1) and the accounting equation (P2) — in this representation they are
// the same sum, which is a consequence of the storage convention rather than a
// coincidence.
func (c *chart) trialBalance(t *rapid.T) int64 {
	ctx := context.Background()

	var sum int64
	for _, a := range c.accounts {
		bal, err := c.svc.Balance(ctx, a.ID)
		if err != nil {
			t.Fatalf("Balance = %v", err)
		}
		sum += a.Type.EquationSign() * bal.Posted
	}
	return sum
}

// P1 / P2 — for any sequence of randomly generated balanced transactions over a random
// chart of accounts, the trial balance sums to zero and the accounting equation holds.
//
// Rejected transactions matter as much as accepted ones: an overdraft that is refused
// must leave nothing behind, and a partial write would show up here as a non-zero sum.
func TestPropertyTrialBalanceIsAlwaysZero(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := newChart(t)
		ctx := context.Background()

		n := rapid.IntRange(1, 40).Draw(t, "transactions")
		for i := 0; i < n; i++ {
			spec := c.drawSpec(t)
			if len(spec.Entries) == 0 {
				continue
			}

			_, err := c.svc.Post(ctx, spec)
			if err != nil && !errors.Is(err, ledger.ErrInsufficientFunds) {
				t.Fatalf("Post = %v", err)
			}

			// The invariant must hold after every single transaction, accepted or not,
			// rather than only at the end.
			if got := c.trialBalance(t); got != 0 {
				t.Fatalf("after transaction %d the trial balance is %d, want 0", i, got)
			}
		}
	})
}

// I1 restated over the entry log rather than the balances: every transaction's debits
// equal its credits, and so do the totals across the whole ledger.
func TestPropertyEntryLogIsBalanced(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := newChart(t)
		ctx := context.Background()

		n := rapid.IntRange(1, 30).Draw(t, "transactions")
		for i := 0; i < n; i++ {
			spec := c.drawSpec(t)
			if len(spec.Entries) == 0 {
				continue
			}
			if _, err := c.svc.Post(ctx, spec); err != nil && !errors.Is(err, ledger.ErrInsufficientFunds) {
				t.Fatalf("Post = %v", err)
			}
		}

		perTxn := map[string]int64{}
		var debits, credits int64

		for _, e := range c.store.AllEntries() {
			if e.Amount <= 0 {
				t.Fatalf("entry %d has amount %d, want a positive amount", e.ID, e.Amount)
			}
			switch e.Direction {
			case ledger.Debit:
				perTxn[e.TransactionID] += e.Amount
				debits += e.Amount
			case ledger.Credit:
				perTxn[e.TransactionID] -= e.Amount
				credits += e.Amount
			default:
				t.Fatalf("entry %d has direction %q", e.ID, e.Direction)
			}
		}

		for txnID, delta := range perTxn {
			if delta != 0 {
				t.Fatalf("transaction %s is unbalanced by %d", txnID, delta)
			}
		}
		if debits != credits {
			t.Fatalf("ledger totals: debits=%d credits=%d", debits, credits)
		}
	})
}

// I7 — the materialized balance must equal the balance derived by replaying the entry
// log. Drift here is the P0 alarm; a property test is where it should first show up.
func TestPropertyMaterializedBalanceMatchesDerived(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := newChart(t)
		ctx := context.Background()

		n := rapid.IntRange(1, 30).Draw(t, "transactions")
		for i := 0; i < n; i++ {
			spec := c.drawSpec(t)
			if len(spec.Entries) == 0 {
				continue
			}
			if _, err := c.svc.Post(ctx, spec); err != nil && !errors.Is(err, ledger.ErrInsufficientFunds) {
				t.Fatalf("Post = %v", err)
			}
		}

		for _, a := range c.accounts {
			bal, err := c.svc.Balance(ctx, a.ID)
			if err != nil {
				t.Fatalf("Balance = %v", err)
			}
			if got, want := bal.Posted, c.store.DerivedBalance(a.ID); got != want {
				t.Fatalf("account %s: materialized posted %d, derived %d", a.ID, got, want)
			}
			if got, want := bal.Pending, c.store.DerivedPending(a.ID); got != want {
				t.Fatalf("account %s: materialized pending %d, derived %d", a.ID, got, want)
			}
		}
	})
}

// P5 — reversal is exact.
//
// Posting a transaction and reversing it must leave every balance exactly as it was;
// reversing that reversal must return every balance to the post-transaction state. This
// is worth more than a hundred hand-written cases because it holds the sign convention,
// the balance convention and the mirroring logic against each other at once.
func TestPropertyReversalRestoresBalances(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := newChart(t)
		ctx := context.Background()

		snapshot := func() map[string]ledger.Balance {
			out := make(map[string]ledger.Balance, len(c.accounts))
			for _, a := range c.accounts {
				bal, err := c.svc.Balance(ctx, a.ID)
				if err != nil {
					t.Fatalf("Balance = %v", err)
				}
				out[a.ID] = bal
			}
			return out
		}

		same := func(what string, want, got map[string]ledger.Balance) {
			for accountID, w := range want {
				g := got[accountID]
				if g.Posted != w.Posted || g.Pending != w.Pending {
					t.Fatalf("%s: account %s is posted=%d pending=%d, want posted=%d pending=%d",
						what, accountID, g.Posted, g.Pending, w.Posted, w.Pending)
				}
			}
		}

		// Build up some history first, so the reversal is not happening against a
		// pristine ledger.
		for i := 0; i < rapid.IntRange(0, 10).Draw(t, "history"); i++ {
			spec := c.drawSpec(t)
			if len(spec.Entries) == 0 {
				continue
			}
			if _, err := c.svc.Post(ctx, spec); err != nil && !errors.Is(err, ledger.ErrInsufficientFunds) {
				t.Fatalf("Post = %v", err)
			}
		}

		before := snapshot()

		spec := c.drawSpec(t)
		spec.Pending = false // only a posted transaction can be reversed
		if len(spec.Entries) == 0 {
			t.Skip("empty spec")
		}
		txn, err := c.svc.Post(ctx, spec)
		if errors.Is(err, ledger.ErrInsufficientFunds) {
			t.Skip("transaction refused")
		}
		if err != nil {
			t.Fatalf("Post = %v", err)
		}

		afterPost := snapshot()

		r1, err := c.svc.Reverse(ctx, txn.ID, "property test")
		if errors.Is(err, ledger.ErrInsufficientFunds) {
			t.Skip("reversal refused by a no-overdraft account")
		}
		if err != nil {
			t.Fatalf("Reverse = %v", err)
		}

		// t followed by Reverse(t) leaves every balance exactly as it was.
		same("after reversal", before, snapshot())

		r2, err := c.svc.Reverse(ctx, r1.ID, "property test")
		if errors.Is(err, ledger.ErrInsufficientFunds) {
			t.Skip("second reversal refused")
		}
		if err != nil {
			t.Fatalf("Reverse of the reversal = %v", err)
		}

		// Reverse(Reverse(t)) returns to the state t left behind.
		same("after double reversal", afterPost, snapshot())

		// The double reversal must have the same shape as the original, not merely the
		// same net effect.
		if got, want := r2.Total(), txn.Total(); got != want {
			t.Fatalf("double reversal total = %d, want %d", got, want)
		}
		if len(r2.Entries) != len(txn.Entries) {
			t.Fatalf("double reversal has %d entries, want %d", len(r2.Entries), len(txn.Entries))
		}
		for i, e := range r2.Entries {
			orig := txn.Entries[i]
			if e.AccountID != orig.AccountID || e.Direction != orig.Direction || e.Amount != orig.Amount {
				t.Fatalf("double reversal entry %d is %s %s %d, want %s %s %d",
					i, e.AccountID, e.Direction, e.Amount, orig.AccountID, orig.Direction, orig.Amount)
			}
		}
	})
}

// Nothing a rejected transaction touched may change. A partial write would be the worst
// possible failure here: the trial balance would still be checked, but money would have
// moved without a balanced counterpart.
func TestPropertyRejectedTransactionsWriteNothing(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := newChart(t)
		ctx := context.Background()

		for i := 0; i < 20; i++ {
			spec := c.drawSpec(t)
			if len(spec.Entries) == 0 {
				continue
			}

			entriesBefore := len(c.store.AllEntries())
			balancesBefore := make(map[string]ledger.Balance, len(c.accounts))
			for _, a := range c.accounts {
				bal, err := c.svc.Balance(ctx, a.ID)
				if err != nil {
					t.Fatalf("Balance = %v", err)
				}
				balancesBefore[a.ID] = bal
			}

			if _, err := c.svc.Post(ctx, spec); err == nil {
				continue // accepted; nothing to assert here
			} else if !errors.Is(err, ledger.ErrInsufficientFunds) {
				t.Fatalf("Post = %v", err)
			}

			if got := len(c.store.AllEntries()); got != entriesBefore {
				t.Fatalf("a rejected transaction wrote %d entries", got-entriesBefore)
			}
			for _, a := range c.accounts {
				bal, err := c.svc.Balance(ctx, a.ID)
				if err != nil {
					t.Fatalf("Balance = %v", err)
				}
				if bal.Posted != balancesBefore[a.ID].Posted || bal.Pending != balancesBefore[a.ID].Pending {
					t.Fatalf("a rejected transaction moved account %s", a.ID)
				}
			}
		}
	})
}
