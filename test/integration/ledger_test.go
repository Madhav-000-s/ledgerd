//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/money"
	"github.com/Madhav-000-s/ledgerd/internal/platform/config"
)

// The full charge → authorize → capture → refund path against a real database.
func TestChargeAuthorizeCaptureRefund(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)

	clearing := e.account("platform_clearing", ledger.Asset, money.USD, true)
	payable := e.account("merchant_payable", ledger.Liability, money.USD, true)
	fees := e.account("fee_revenue", ledger.Revenue, money.USD, true)
	customer := e.account("customer_balance", ledger.Liability, money.USD, false)
	holds := e.account("authorization_holds", ledger.Liability, money.USD, true)

	// A $100.00 charge with a 2.9% + 30c fee.
	charge := e.post(ledger.TransactionSpec{
		MerchantID:  testMerchant,
		Description: "charge",
		ExternalRef: "pay_1",
		Entries: []ledger.EntrySpec{
			entry(clearing.ID, ledger.Debit, 10000),
			entry(payable.ID, ledger.Credit, 9680),
			entry(fees.ID, ledger.Credit, 320),
		},
	})

	if got := e.balance(clearing.ID).Posted; got != 10000 {
		t.Errorf("clearing = %d, want 10000", got)
	}
	if got := e.balance(fees.ID).Posted; got != 320 {
		t.Errorf("fees = %d, want 320", got)
	}

	// Fund the customer, then authorize against them.
	e.post(ledger.TransactionSpec{
		MerchantID: testMerchant,
		Entries: []ledger.EntrySpec{
			entry(clearing.ID, ledger.Debit, 20000),
			entry(customer.ID, ledger.Credit, 20000),
		},
	})

	auth := e.post(ledger.TransactionSpec{
		MerchantID: testMerchant,
		Pending:    true,
		Entries: []ledger.EntrySpec{
			entry(customer.ID, ledger.Debit, 10000),
			entry(holds.ID, ledger.Credit, 10000),
		},
	})

	bal := e.balance(customer.ID)
	if bal.Posted != 20000 {
		t.Errorf("a hold moved the posted balance to %d", bal.Posted)
	}
	if bal.Available() != 10000 {
		t.Errorf("available = %d, want 10000", bal.Available())
	}

	// Capture $60 of the $100 hold.
	amount := usd(6000)
	var captured *ledger.Transaction
	if err := e.inTx(func(ctx context.Context) error {
		var err error
		captured, err = e.svc.Capture(ctx, auth.ID, &amount)
		return err
	}); err != nil {
		t.Fatalf("Capture = %v", err)
	}

	bal = e.balance(customer.ID)
	if bal.Posted != 14000 {
		t.Errorf("after partial capture posted = %d, want 14000", bal.Posted)
	}
	if bal.Pending != 0 {
		t.Errorf("after capture pending = %d, want 0", bal.Pending)
	}

	// Refund the original charge by reversing it.
	var reversal *ledger.Transaction
	if err := e.inTx(func(ctx context.Context) error {
		var err error
		reversal, err = e.svc.Reverse(ctx, charge.ID, "customer disputed")
		return err
	}); err != nil {
		t.Fatalf("Reverse = %v", err)
	}
	if reversal.ReversesID != charge.ID {
		t.Errorf("ReversesID = %q, want %q", reversal.ReversesID, charge.ID)
	}
	if got := e.balance(fees.ID).Posted; got != 0 {
		t.Errorf("fees after reversal = %d, want 0", got)
	}

	// Every posted balance must still equal what the entry log derives (I7).
	for _, a := range []*ledger.Account{clearing, payable, fees, customer, holds} {
		if got, want := e.balance(a.ID).Posted, e.derivedBalance(a.ID); got != want {
			t.Errorf("account %s: materialized %d, derived %d", a.Name, got, want)
		}
	}
	if _, err := e.svc.GetTransaction(e.ctx, captured.ID); err != nil {
		t.Errorf("GetTransaction(capture) = %v", err)
	}
}

// The constraint trigger must reject an unbalanced transaction written by raw SQL, not
// merely one that went through the Go validation. This is the layer that catches what
// the application does not know it got wrong.
func TestTriggerRejectsUnbalancedRawInsert(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)

	a := e.account("cash", ledger.Asset, money.USD, true)
	b := e.account("payable", ledger.Liability, money.USD, true)

	ctx := context.Background()
	tx, err := e.db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
        INSERT INTO transactions (id, merchant_id, status, currency, created_at)
        VALUES ('txn_raw', $1, 'posted', 'USD', now())`, testMerchant); err != nil {
		t.Fatalf("inserting transaction: %v", err)
	}

	// Debits 100, credits 99 — deliberately unbalanced.
	if _, err := tx.Exec(ctx, `
        INSERT INTO entries (transaction_id, account_id, direction, amount_minor, currency, status, seq, balance_after, created_at)
        VALUES ('txn_raw', $1, 'debit', 100, 'USD', 'posted', 0, 100, now()),
               ('txn_raw', $2, 'credit', 99, 'USD', 'posted', 1, 99, now())`,
		a.ID, b.ID); err != nil {
		t.Fatalf("inserting entries: %v", err)
	}

	// The trigger is deferred, so it fires here rather than at the INSERT.
	err = tx.Commit(ctx)
	if err == nil {
		t.Fatal("commit succeeded with an unbalanced transaction")
	}
	if !strings.Contains(err.Error(), "unbalanced") {
		t.Errorf("commit error = %v, want an unbalanced complaint", err)
	}
}

func TestTriggerRejectsMultiCurrencyRawInsert(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)

	usdAcct := e.account("usd_cash", ledger.Asset, money.USD, true)
	eurAcct := e.account("eur_payable", ledger.Liability, money.EUR, true)

	ctx := context.Background()
	tx, err := e.db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
        INSERT INTO transactions (id, merchant_id, status, currency, created_at)
        VALUES ('txn_mixed', $1, 'posted', 'USD', now())`, testMerchant); err != nil {
		t.Fatalf("inserting transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO entries (transaction_id, account_id, direction, amount_minor, currency, status, seq, balance_after, created_at)
        VALUES ('txn_mixed', $1, 'debit', 100, 'USD', 'posted', 0, 100, now()),
               ('txn_mixed', $2, 'credit', 100, 'EUR', 'posted', 1, 100, now())`,
		usdAcct.ID, eurAcct.ID); err != nil {
		t.Fatalf("inserting entries: %v", err)
	}

	if err := tx.Commit(ctx); err == nil {
		t.Fatal("commit succeeded with a mixed-currency transaction")
	} else if !strings.Contains(err.Error(), "currencies") {
		t.Errorf("commit error = %v, want a currency complaint", err)
	}
}

// I3 is a CHECK constraint, so it rejects at the statement rather than at commit.
func TestCheckRejectsNonPositiveAmount(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)
	a := e.account("cash", ledger.Asset, money.USD, true)

	ctx := context.Background()
	if _, err := e.db.Pool().Exec(ctx, `
        INSERT INTO transactions (id, merchant_id, status, currency, created_at)
        VALUES ('txn_zero', $1, 'posted', 'USD', now())`, testMerchant); err != nil {
		t.Fatalf("inserting transaction: %v", err)
	}

	_, err := e.db.Pool().Exec(ctx, `
        INSERT INTO entries (transaction_id, account_id, direction, amount_minor, currency, status, seq, balance_after, created_at)
        VALUES ('txn_zero', $1, 'debit', 0, 'USD', 'posted', 0, 0, now())`, a.ID)
	if err == nil {
		t.Fatal("a zero-amount entry was accepted")
	}
}

// I4. Entries are append-only. The rules make an UPDATE or DELETE a silent no-op, so
// the assertion is on the affected row count rather than on an error.
func TestEntriesAreImmutableInTheDatabase(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)

	a := e.account("cash", ledger.Asset, money.USD, true)
	b := e.account("payable", ledger.Liability, money.USD, true)

	e.post(ledger.TransactionSpec{
		MerchantID: testMerchant,
		Entries: []ledger.EntrySpec{
			entry(a.ID, ledger.Debit, 500),
			entry(b.ID, ledger.Credit, 500),
		},
	})

	ctx := context.Background()

	tag, err := e.db.Pool().Exec(ctx, `UPDATE entries SET amount_minor = 999999`)
	if err != nil {
		t.Fatalf("UPDATE errored rather than being ignored: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Errorf("UPDATE affected %d entries, want 0", tag.RowsAffected())
	}

	tag, err = e.db.Pool().Exec(ctx, `DELETE FROM entries`)
	if err != nil {
		t.Fatalf("DELETE errored rather than being ignored: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Errorf("DELETE affected %d entries, want 0", tag.RowsAffected())
	}

	// The entries are still there, unchanged.
	var count, total int64
	if err := e.db.Pool().QueryRow(ctx,
		`SELECT count(*), COALESCE(sum(amount_minor), 0) FROM entries`).Scan(&count, &total); err != nil {
		t.Fatalf("counting entries: %v", err)
	}
	if count != 2 || total != 1000 {
		t.Errorf("entries: count=%d total=%d, want count=2 total=1000", count, total)
	}
}

// I6 across a real transaction boundary: an overdraft must roll back completely, taking
// the transaction row and the entries with it.
func TestOverdraftRollsBackEverything(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)

	clearing := e.account("clearing", ledger.Asset, money.USD, true)
	customer := e.account("customer", ledger.Liability, money.USD, false)

	e.post(ledger.TransactionSpec{
		MerchantID: testMerchant,
		Entries: []ledger.EntrySpec{
			entry(clearing.ID, ledger.Debit, 5000),
			entry(customer.ID, ledger.Credit, 5000),
		},
	})

	_, err := e.tryPost(ledger.TransactionSpec{
		MerchantID: testMerchant,
		Entries: []ledger.EntrySpec{
			entry(customer.ID, ledger.Debit, 5001),
			entry(clearing.ID, ledger.Credit, 5001),
		},
	})
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("Post = %v, want ErrInsufficientFunds", err)
	}

	if got := e.balance(customer.ID).Posted; got != 5000 {
		t.Errorf("customer balance = %d after a refused overdraft, want 5000", got)
	}

	var txnCount, entryCount int64
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT count(*) FROM transactions`).Scan(&txnCount); err != nil {
		t.Fatalf("counting transactions: %v", err)
	}
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT count(*) FROM entries`).Scan(&entryCount); err != nil {
		t.Fatalf("counting entries: %v", err)
	}
	if txnCount != 1 || entryCount != 2 {
		t.Errorf("after a refused overdraft: %d transactions, %d entries; want 1 and 2", txnCount, entryCount)
	}
}

// A transaction can be reversed at most once. The unique partial index is what makes
// that true under concurrency, not the application-level check.
func TestDoubleReversalIsRejectedByTheDatabase(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)

	a := e.account("cash", ledger.Asset, money.USD, true)
	b := e.account("payable", ledger.Liability, money.USD, true)

	txn := e.post(ledger.TransactionSpec{
		MerchantID: testMerchant,
		Entries: []ledger.EntrySpec{
			entry(a.ID, ledger.Debit, 100),
			entry(b.ID, ledger.Credit, 100),
		},
	})

	if err := e.inTx(func(ctx context.Context) error {
		_, err := e.svc.Reverse(ctx, txn.ID, "first")
		return err
	}); err != nil {
		t.Fatalf("first Reverse = %v", err)
	}

	err := e.inTx(func(ctx context.Context) error {
		_, err := e.svc.Reverse(ctx, txn.ID, "second")
		return err
	})
	if !errors.Is(err, ledger.ErrAlreadyReversed) {
		t.Errorf("second Reverse = %v, want ErrAlreadyReversed", err)
	}

	// Bypassing the service entirely must still fail, on the index.
	_, err = e.db.Pool().Exec(e.ctx, `
        INSERT INTO transactions (id, merchant_id, status, currency, reverses_id, created_at)
        VALUES ('txn_sneaky', $1, 'posted', 'USD', $2, now())`, testMerchant, txn.ID)
	if err == nil {
		t.Error("a second reversal row was accepted by raw SQL")
	}
}

// A failure anywhere inside InTx must take the whole unit with it, including work that
// had already succeeded.
func TestInTxRollsBackPartialWork(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)

	a := e.account("cash", ledger.Asset, money.USD, true)
	b := e.account("payable", ledger.Liability, money.USD, true)

	sentinel := errors.New("caller changed its mind")

	err := e.inTx(func(ctx context.Context) error {
		if _, err := e.svc.Post(ctx, ledger.TransactionSpec{
			MerchantID: testMerchant,
			Entries: []ledger.EntrySpec{
				entry(a.ID, ledger.Debit, 100),
				entry(b.ID, ledger.Credit, 100),
			},
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx = %v, want the sentinel", err)
	}

	var entryCount int64
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT count(*) FROM entries`).Scan(&entryCount); err != nil {
		t.Fatalf("counting entries: %v", err)
	}
	if entryCount != 0 {
		t.Errorf("%d entries survived a rolled-back transaction, want 0", entryCount)
	}
	if got := e.balance(a.ID).Posted; got != 0 {
		t.Errorf("balance = %d after rollback, want 0", got)
	}
}

// Keyset pagination must return every row exactly once, in order, over a real index.
func TestListEntriesPaginationOverRealIndex(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)

	a := e.account("cash", ledger.Asset, money.USD, true)
	b := e.account("payable", ledger.Liability, money.USD, true)

	const n = 47
	for i := 0; i < n; i++ {
		e.post(ledger.TransactionSpec{
			MerchantID: testMerchant,
			Entries: []ledger.EntrySpec{
				entry(a.ID, ledger.Debit, int64(i+1)),
				entry(b.ID, ledger.Credit, int64(i+1)),
			},
		})
	}

	seen := map[int64]bool{}
	var (
		cursor string
		order  []int64
	)
	for page := 0; ; page++ {
		if page > n {
			t.Fatal("pagination did not terminate")
		}
		batch, next, err := e.svc.ListEntries(e.ctx, a.ID, ledger.Page{Limit: 10, StartingAfter: cursor})
		if err != nil {
			t.Fatalf("ListEntries = %v", err)
		}
		for _, entry := range batch {
			if seen[entry.ID] {
				t.Fatalf("entry %d returned twice", entry.ID)
			}
			seen[entry.ID] = true
			order = append(order, entry.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}

	if len(order) != n {
		t.Fatalf("saw %d entries, want %d", len(order), n)
	}
	for i := 1; i < len(order); i++ {
		if order[i] >= order[i-1] {
			t.Fatalf("entries are not strictly newest-first at %d", i)
		}
	}
}

// balance_after must replay to the final balance in id order. This is what makes drift
// bisectable, and it catches lost-update bugs that a balance comparison alone would miss.
func TestBalanceAfterReplaysInIDOrder(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)

	a := e.account("cash", ledger.Asset, money.USD, true)
	b := e.account("payable", ledger.Liability, money.USD, true)

	for i := 0; i < 30; i++ {
		e.post(ledger.TransactionSpec{
			MerchantID: testMerchant,
			Entries: []ledger.EntrySpec{
				entry(a.ID, ledger.Debit, int64(i+1)),
				entry(b.ID, ledger.Credit, int64(i+1)),
			},
		})
	}

	rows, err := e.db.Pool().Query(e.ctx, `
        SELECT amount_minor, balance_after FROM entries
         WHERE account_id = $1 AND status = 'posted' ORDER BY id`, a.ID)
	if err != nil {
		t.Fatalf("querying entries: %v", err)
	}
	defer rows.Close()

	var running int64
	for rows.Next() {
		var amount, after int64
		if err := rows.Scan(&amount, &after); err != nil {
			t.Fatalf("scan: %v", err)
		}
		running += amount // a debit on a debit-normal account
		if after != running {
			t.Fatalf("balance_after = %d, replayed value = %d", after, running)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating: %v", err)
	}

	if got := e.balance(a.ID).Posted; got != running {
		t.Errorf("final balance %d does not match the replay %d", got, running)
	}
}
