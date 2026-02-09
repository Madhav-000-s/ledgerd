//go:build integration

package integration

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/money"
	"github.com/Madhav-000-s/ledgerd/internal/platform/config"
)

// The headline test.
//
//	go test -race -count=1 -tags=integration ./test/integration -run TestConcurrentTransfers
//
// 64 goroutines x 500 iterations of random transfers among 16 accounts, with roughly a
// fifth of attempts deliberately overdrawing. What matters is not that it passes but
// what it asserts afterwards: money is conserved, no account illegally went negative,
// every transaction balanced, the materialized balances still agree with the entry log,
// no request vanished or was applied twice, every entry's balance_after replays to the
// final balance in id order, and no deadlock occurred.
//
// Assertion F — the replay — is the one that earns its keep. A lost update can leave the
// endpoint balances looking plausible while the sequence that produced them is
// impossible, and only replaying the recorded running balance catches that.
//
// The same body runs under READ COMMITTED with ordered row locks and under SERIALIZABLE.
// If the two ever disagree about the final state, the locking strategy has a bug.
const (
	concurrentGoroutines = 64
	iterationsPerG       = 500
	transferAccounts     = 16
	openingBalance       = 100_000
	overdrawFraction     = 5 // one attempt in five deliberately overdraws
)

func TestConcurrentTransfers(t *testing.T) {
	for _, isolation := range []config.Isolation{
		config.IsolationReadCommitted,
		config.IsolationSerializable,
	} {
		t.Run(string(isolation), func(t *testing.T) {
			runConcurrentTransfers(t, isolation)
		})
	}
}

func runConcurrentTransfers(t *testing.T, isolation config.Isolation) {
	e := newEnv(t, isolation)

	goroutines, iterations := concurrentGoroutines, iterationsPerG
	if testing.Short() {
		goroutines, iterations = 8, 40
	}

	// The chart of accounts: sixteen liability accounts that may not go negative, plus
	// one equity account to fund them from. Refusing to overdraw is the point, so the
	// transfer accounts deliberately do not set allow_negative.
	funding := e.account("opening_equity", ledger.Equity, money.USD, true)

	accounts := make([]*ledger.Account, transferAccounts)
	for i := range accounts {
		accounts[i] = e.account(accountName(i), ledger.Liability, money.USD, false)
		e.post(ledger.TransactionSpec{
			MerchantID:  testMerchant,
			Description: "opening balance",
			Entries: []ledger.EntrySpec{
				entry(funding.ID, ledger.Debit, openingBalance),
				entry(accounts[i].ID, ledger.Credit, openingBalance),
			},
		})
	}

	openingTotal := int64(transferAccounts) * openingBalance

	var (
		succeeded             atomic.Int64
		refused               atomic.Int64
		deadlocks             atomic.Int64
		attempted             atomic.Int64
		serializationFailures atomic.Int64
		wg                    sync.WaitGroup
	)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()

			rng := rand.New(rand.NewSource(seed))

			for i := 0; i < iterations; i++ {
				from := rng.Intn(len(accounts))
				to := rng.Intn(len(accounts))
				if from == to {
					to = (to + 1) % len(accounts)
				}

				// Most transfers are small enough to succeed; one in five asks for
				// more than any account could hold, so the overdraft path is exercised
				// under contention rather than only in a quiet unit test.
				amount := int64(rng.Intn(500) + 1)
				if rng.Intn(overdrawFraction) == 0 {
					amount = openingTotal + int64(rng.Intn(1000)) + 1
				}

				attempted.Add(1)

				err := e.inTx(func(ctx context.Context) error {
					_, err := e.svc.Post(ctx, ledger.TransactionSpec{
						MerchantID:  testMerchant,
						Description: "concurrent transfer",
						Entries: []ledger.EntrySpec{
							{AccountID: accounts[from].ID, Direction: ledger.Debit, Amount: usd(amount)},
							{AccountID: accounts[to].ID, Direction: ledger.Credit, Amount: usd(amount)},
						},
					})
					return err
				})

				switch {
				case err == nil:
					succeeded.Add(1)
				case errors.Is(err, ledger.ErrInsufficientFunds):
					refused.Add(1)
				case isDeadlock(err):
					// Ordered lock acquisition is supposed to make this impossible.
					deadlocks.Add(1)
					t.Errorf("deadlock: %v", err)
				case isSerializationFailure(err):
					// Only reachable under SERIALIZABLE, where the database aborts a
					// transaction rather than making it wait. Counted separately: the
					// request did not happen, so it is neither a success nor a refusal,
					// and assertion E accounts for it explicitly.
					//
					// That this bucket is empty under READ COMMITTED is the point of
					// running both: ordered row locks make contenders queue, where
					// SERIALIZABLE makes them abort and retry.
					serializationFailures.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}
		}(int64(g)*7919 + 1)
	}
	wg.Wait()

	if deadlocks.Load() > 0 {
		t.Fatalf("G: %d deadlocks; ordered lock acquisition should make them impossible", deadlocks.Load())
	}

	// ── A. conservation of money ────────────────────────────────────────────
	//
	// Transfers only move value between the sixteen accounts, so their total must be
	// exactly what was paid in.
	var total int64
	for _, a := range accounts {
		total += e.balance(a.ID).Posted
	}
	if total != openingTotal {
		t.Errorf("A: balances total %d, want %d (%d was created or destroyed)",
			total, openingTotal, total-openingTotal)
	}

	// ── B. no illegal negative balance ──────────────────────────────────────
	for _, a := range accounts {
		bal := e.balance(a.ID)
		if bal.Posted < 0 {
			t.Errorf("B: account %s is %d, and it does not allow a negative balance", a.Name, bal.Posted)
		}
	}

	// ── C. every transaction balanced ───────────────────────────────────────
	var unbalanced int64
	if err := e.db.Pool().QueryRow(e.ctx, `
        SELECT count(*) FROM (
            SELECT transaction_id
              FROM entries
             GROUP BY transaction_id
            HAVING COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'debit'), 0)
                <> COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'credit'), 0)
        ) unbalanced`).Scan(&unbalanced); err != nil {
		t.Fatalf("checking balance per transaction: %v", err)
	}
	if unbalanced != 0 {
		t.Errorf("C: %d transactions are unbalanced", unbalanced)
	}

	// ── D. materialized balances match the entry log (I7) ───────────────────
	for _, a := range accounts {
		if got, want := e.balance(a.ID).Posted, e.derivedBalance(a.ID); got != want {
			t.Errorf("D: account %s materialized %d, derived %d", a.Name, got, want)
		}
	}

	// ── E. no request vanished, none applied twice ──────────────────────────
	//
	// Every attempt ended in exactly one of: committed, refused for insufficient funds,
	// or aborted by the serializable checker. And the number of committed transfers must
	// equal the number of transfer transactions on disk.
	accountedFor := succeeded.Load() + refused.Load() + serializationFailures.Load()
	if accountedFor != attempted.Load() {
		t.Errorf("E: %d attempts but %d accounted for (%d succeeded, %d refused, %d aborted)",
			attempted.Load(), accountedFor, succeeded.Load(), refused.Load(), serializationFailures.Load())
	}

	var transferTxns int64
	if err := e.db.Pool().QueryRow(e.ctx,
		`SELECT count(*) FROM transactions WHERE description = 'concurrent transfer'`).Scan(&transferTxns); err != nil {
		t.Fatalf("counting transfers: %v", err)
	}
	if transferTxns != succeeded.Load() {
		t.Errorf("E: %d transfers committed but %d rows on disk", succeeded.Load(), transferTxns)
	}

	// ── F. balance_after replays to the final balance, in id order ──────────
	//
	// The assertion that catches lost updates the endpoint checks cannot: it validates
	// the sequence, not just where it ended up.
	for _, a := range accounts {
		replayAccount(t, e, a)
	}

	// ── G. no deadlocks — asserted above, before the slower checks ──────────

	if succeeded.Load() == 0 {
		t.Fatal("no transfer succeeded; the test proved nothing")
	}
	if refused.Load() == 0 {
		t.Error("no transfer was refused; the overdraft path was never exercised")
	}

	t.Logf("%s: %d attempted, %d committed, %d refused, %d serialization aborts",
		isolation, attempted.Load(), succeeded.Load(), refused.Load(), serializationFailures.Load())
}

func replayAccount(t *testing.T, e *env, a *ledger.Account) {
	t.Helper()

	rows, err := e.db.Pool().Query(e.ctx, `
        SELECT e.direction::text, e.amount_minor, e.balance_after
          FROM entries e
         WHERE e.account_id = $1 AND e.status = 'posted'
         ORDER BY e.id`, a.ID)
	if err != nil {
		t.Fatalf("F: querying entries for %s: %v", a.Name, err)
	}
	defer rows.Close()

	var running int64
	for rows.Next() {
		var (
			direction string
			amount    int64
			after     int64
		)
		if err := rows.Scan(&direction, &amount, &after); err != nil {
			t.Fatalf("F: scan: %v", err)
		}

		// Every transfer account here is credit-normal, so a credit raises it.
		if direction == string(ledger.Credit) {
			running += amount
		} else {
			running -= amount
		}

		if after != running {
			t.Fatalf("F: account %s recorded balance_after=%d where the replay gives %d; "+
				"a write was lost or applied out of order", a.Name, after, running)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("F: iterating: %v", err)
	}

	if got := e.balance(a.ID).Posted; got != running {
		t.Errorf("F: account %s final balance %d, replay %d", a.Name, got, running)
	}
}

func accountName(i int) string {
	return "transfer_" + string(rune('a'+i))
}

// isDeadlock reports whether err is Postgres error 40P01. Ordered lock acquisition is
// meant to make it unreachable, so seeing one is a design failure rather than something
// to retry.
func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40P01"
	}
	return strings.Contains(err.Error(), "deadlock detected")
}

// isSerializationFailure reports whether err is Postgres error 40001, which only occurs
// under SERIALIZABLE.
func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40001"
	}
	return strings.Contains(err.Error(), "could not serialize")
}

// Lock contention must degrade into waiting, not into errors. A transfer that touches
// the same hot account from many goroutines should still commit, just more slowly.
func TestHotAccountContentionDoesNotFail(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)

	funding := e.account("opening_equity", ledger.Equity, money.USD, true)
	hot := e.account("fee_revenue", ledger.Revenue, money.USD, true)

	const (
		writers = 32
		each    = 25
	)

	var wg sync.WaitGroup
	var failures atomic.Int64

	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				err := e.inTx(func(ctx context.Context) error {
					_, err := e.svc.Post(ctx, ledger.TransactionSpec{
						MerchantID:  testMerchant,
						Description: "fee",
						Entries: []ledger.EntrySpec{
							entry(funding.ID, ledger.Debit, 1),
							entry(hot.ID, ledger.Credit, 1),
						},
					})
					return err
				})
				if err != nil {
					failures.Add(1)
					t.Errorf("contended write failed: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	if failures.Load() > 0 {
		t.Fatalf("%d contended writes failed; contention should queue, not error", failures.Load())
	}

	want := int64(writers * each)
	if got := e.balance(hot.ID).Posted; got != want {
		t.Errorf("hot account balance = %d, want %d", got, want)
	}
	if got, derived := e.balance(hot.ID).Posted, e.derivedBalance(hot.ID); got != derived {
		t.Errorf("hot account materialized %d, derived %d", got, derived)
	}
}
