//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/idempotency"
	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/money"
	"github.com/Madhav-000-s/ledgerd/internal/platform/config"
	"github.com/Madhav-000-s/ledgerd/internal/platform/id"
	"github.com/Madhav-000-s/ledgerd/internal/postgres"
)

func newIdemStore(t *testing.T, e *env) *postgres.IdempotencyRepo {
	t.Helper()
	return postgres.NewIdempotencyRepo(e.db)
}

func seedRecord(key string) *idempotency.Record {
	now := time.Now().UTC()
	return &idempotency.Record{
		ID:            id.New(id.PrefixIdempotency),
		MerchantID:    testMerchant,
		Key:           key,
		Method:        "POST",
		Path:          "/v1/transfers",
		RequestHash:   []byte("0123456789abcdef0123456789abcdef"),
		State:         idempotency.StateInProgress,
		RecoveryPoint: idempotency.PointStarted,
		LockExpiresAt: ptr(now.Add(idempotency.LeaseDuration)),
		RequestID:     "req_seed",
		CreatedAt:     now,
		ExpiresAt:     now.Add(idempotency.TTL),
	}
}

func ptr[T any](v T) *T { return &v }

// The unique constraint is what makes the claim atomic. Of many concurrent retries
// exactly one may acquire the key; checking and then inserting would let several through.
func TestAcquireIsAtomicUnderConcurrency(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)
	store := newIdemStore(t, e)

	const attempts = 40

	var (
		acquired atomic.Int64
		wg       sync.WaitGroup
		gate     = make(chan struct{})
	)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate

			_, won, err := store.Acquire(context.Background(), seedRecord("concurrent-key"))
			if err != nil {
				t.Errorf("Acquire = %v", err)
				return
			}
			if won {
				acquired.Add(1)
			}
		}()
	}
	close(gate)
	wg.Wait()

	if got := acquired.Load(); got != 1 {
		t.Errorf("%d callers acquired the key, want exactly 1", got)
	}
}

// The ordering property from DESIGN.md §7.2: the response and the ledger entries commit
// together, so a crash cannot leave money moved with no replayable answer.
func TestResponseCommitsWithTheLedgerWrite(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)
	store := newIdemStore(t, e)

	cash := e.account("cash", ledger.Asset, money.USD, true)
	payable := e.account("payable", ledger.Liability, money.USD, true)

	rec, won, err := store.Acquire(context.Background(), seedRecord("atomic-key"))
	if err != nil || !won {
		t.Fatalf("Acquire = %v, won = %v", err, won)
	}

	sentinel := errors.New("crash after posting, before commit")

	// Post the ledger entries and record the response in one transaction, then fail it.
	err = e.inTx(func(ctx context.Context) error {
		if _, err := e.svc.Post(ctx, ledger.TransactionSpec{
			MerchantID: testMerchant,
			Entries: []ledger.EntrySpec{
				entry(cash.ID, ledger.Debit, 1000),
				entry(payable.ID, ledger.Credit, 1000),
			},
		}); err != nil {
			return err
		}
		if err := store.Complete(ctx, rec.ID, idempotency.StateSucceeded, 201, []byte(`{"ok":true}`), "txn_x"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx = %v, want the sentinel", err)
	}

	// Neither half survived. This is the guarantee: not "the ledger rolled back" and not
	// "the response rolled back", but that they cannot disagree.
	if got := e.balance(cash.ID).Posted; got != 0 {
		t.Errorf("ledger balance = %d after rollback, want 0", got)
	}

	after, err := store.Get(context.Background(), testMerchant, "atomic-key")
	if err != nil {
		t.Fatalf("Get = %v", err)
	}
	if after.State != idempotency.StateInProgress {
		t.Errorf("idempotency state = %q after rollback, want in_progress", after.State)
	}
	if after.ResponseStatus != 0 {
		t.Errorf("a response was recorded for a transaction that rolled back: %d", after.ResponseStatus)
	}

	// Now do it for real.
	var txnID string
	if err := e.inTx(func(ctx context.Context) error {
		txn, err := e.svc.Post(ctx, ledger.TransactionSpec{
			MerchantID: testMerchant,
			Entries: []ledger.EntrySpec{
				entry(cash.ID, ledger.Debit, 1000),
				entry(payable.ID, ledger.Credit, 1000),
			},
		})
		if err != nil {
			return err
		}
		txnID = txn.ID
		return store.Complete(ctx, rec.ID, idempotency.StateSucceeded, 201, []byte(`{"ok":true}`), txn.ID)
	}); err != nil {
		t.Fatalf("InTx = %v", err)
	}

	if got := e.balance(cash.ID).Posted; got != 1000 {
		t.Errorf("ledger balance = %d, want 1000", got)
	}

	committed, err := store.Get(context.Background(), testMerchant, "atomic-key")
	if err != nil {
		t.Fatalf("Get = %v", err)
	}
	if committed.State != idempotency.StateSucceeded {
		t.Errorf("state = %q, want succeeded", committed.State)
	}
	if committed.ResourceID != txnID {
		t.Errorf("resource_id = %q, want %q", committed.ResourceID, txnID)
	}
	if committed.LockExpiresAt != nil {
		t.Error("a terminal record still holds a lock")
	}
}

// Taking over an expired lease is a compare-and-set, so several retries arriving after a
// crash do not all decide they own the key.
func TestTakeOverIsExclusive(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)
	store := newIdemStore(t, e)
	ctx := context.Background()

	rec, won, err := store.Acquire(ctx, seedRecord("stale-key"))
	if err != nil || !won {
		t.Fatalf("Acquire = %v, won = %v", err, won)
	}

	// A live lease cannot be taken.
	taken, err := store.TakeOver(ctx, rec.ID, time.Now().UTC().Add(idempotency.LeaseDuration))
	if err != nil {
		t.Fatalf("TakeOver = %v", err)
	}
	if taken {
		t.Fatal("a live lease was taken over")
	}

	// Expire it, as a crashed holder would leave it.
	if _, err := e.db.Pool().Exec(ctx,
		`UPDATE idempotency_keys SET lock_expires_at = now() - interval '1 hour' WHERE id = $1`, rec.ID); err != nil {
		t.Fatalf("expiring the lease: %v", err)
	}

	const racers = 20
	var (
		winners atomic.Int64
		wg      sync.WaitGroup
		gate    = make(chan struct{})
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate

			ok, err := store.TakeOver(context.Background(), rec.ID, time.Now().UTC().Add(idempotency.LeaseDuration))
			if err != nil {
				t.Errorf("TakeOver = %v", err)
				return
			}
			if ok {
				winners.Add(1)
			}
		}()
	}
	close(gate)
	wg.Wait()

	if got := winners.Load(); got != 1 {
		t.Errorf("%d callers took over the expired lease, want exactly 1", got)
	}
}

// A 5xx releases the key so a retry genuinely re-executes rather than replaying a 500.
func TestReleaseAllowsReexecution(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)
	store := newIdemStore(t, e)
	ctx := context.Background()

	rec, _, err := store.Acquire(ctx, seedRecord("released-key"))
	if err != nil {
		t.Fatalf("Acquire = %v", err)
	}

	if err := store.Release(ctx, rec.ID); err != nil {
		t.Fatalf("Release = %v", err)
	}

	if _, err := store.Get(ctx, testMerchant, "released-key"); !errors.Is(err, idempotency.ErrKeyNotFound) {
		t.Errorf("Get after Release = %v, want ErrKeyNotFound", err)
	}

	// The key is free again.
	_, won, err := store.Acquire(ctx, seedRecord("released-key"))
	if err != nil {
		t.Fatalf("Acquire = %v", err)
	}
	if !won {
		t.Error("a released key could not be re-acquired")
	}
}

// A completed record must not be releasable, or a retry after the response was recorded
// would re-execute and charge twice.
func TestReleaseDoesNotTouchTerminalRecords(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)
	store := newIdemStore(t, e)
	ctx := context.Background()

	rec, _, err := store.Acquire(ctx, seedRecord("done-key"))
	if err != nil {
		t.Fatalf("Acquire = %v", err)
	}
	if err := store.Complete(ctx, rec.ID, idempotency.StateSucceeded, 201, []byte(`{"ok":true}`), "txn_1"); err != nil {
		t.Fatalf("Complete = %v", err)
	}

	if err := store.Release(ctx, rec.ID); err != nil {
		t.Fatalf("Release = %v", err)
	}

	after, err := store.Get(ctx, testMerchant, "done-key")
	if err != nil {
		t.Fatalf("a completed record was deleted by Release: %v", err)
	}
	if after.State != idempotency.StateSucceeded {
		t.Errorf("state = %q, want succeeded", after.State)
	}
}

func TestAdvanceRecoveryPoint(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)
	store := newIdemStore(t, e)
	ctx := context.Background()

	rec, _, err := store.Acquire(ctx, seedRecord("phased-key"))
	if err != nil {
		t.Fatalf("Acquire = %v", err)
	}

	lease := time.Now().UTC().Add(2 * idempotency.LeaseDuration)
	if err := store.Advance(ctx, rec.ID, idempotency.PointLedgerPosted, lease); err != nil {
		t.Fatalf("Advance = %v", err)
	}

	after, err := store.Get(ctx, testMerchant, "phased-key")
	if err != nil {
		t.Fatalf("Get = %v", err)
	}
	if after.RecoveryPoint != idempotency.PointLedgerPosted {
		t.Errorf("recovery_point = %q, want %q", after.RecoveryPoint, idempotency.PointLedgerPosted)
	}
	if after.LockExpiresAt == nil || !after.LockExpiresAt.After(time.Now().UTC()) {
		t.Error("Advance did not extend the lease")
	}
}

// Keys are scoped by merchant, so one tenant can neither collide with nor probe
// another's.
func TestKeysAreIsolatedByMerchant(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)
	store := newIdemStore(t, e)
	ctx := context.Background()

	a := seedRecord("shared-key")
	if _, won, err := store.Acquire(ctx, a); err != nil || !won {
		t.Fatalf("merchant A Acquire = %v, won = %v", err, won)
	}

	b := seedRecord("shared-key")
	b.MerchantID = "mer_other"
	if _, won, err := store.Acquire(ctx, b); err != nil || !won {
		t.Fatalf("merchant B was blocked by merchant A's key: %v, won = %v", err, won)
	}

	if _, err := store.Get(ctx, "mer_third", "shared-key"); !errors.Is(err, idempotency.ErrKeyNotFound) {
		t.Errorf("a third merchant could see the key: %v", err)
	}
}

func TestReaperDeletesExpiredKeys(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)
	store := newIdemStore(t, e)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		rec := seedRecord("expired-" + string(rune('a'+i)))
		rec.ExpiresAt = time.Now().UTC().Add(-time.Hour)
		if _, _, err := store.Acquire(ctx, rec); err != nil {
			t.Fatalf("Acquire = %v", err)
		}
	}
	live := seedRecord("still-live")
	if _, _, err := store.Acquire(ctx, live); err != nil {
		t.Fatalf("Acquire = %v", err)
	}

	n, err := store.DeleteExpired(ctx, time.Now().UTC(), 1000)
	if err != nil {
		t.Fatalf("DeleteExpired = %v", err)
	}
	if n != 5 {
		t.Errorf("deleted %d keys, want 5", n)
	}

	if _, err := store.Get(ctx, testMerchant, "still-live"); err != nil {
		t.Errorf("an unexpired key was reaped: %v", err)
	}
}

func TestReaperRespectsBatchLimit(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)
	store := newIdemStore(t, e)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		rec := seedRecord("old-" + string(rune('a'+i)))
		rec.ExpiresAt = time.Now().UTC().Add(-time.Hour)
		if _, _, err := store.Acquire(ctx, rec); err != nil {
			t.Fatalf("Acquire = %v", err)
		}
	}

	n, err := store.DeleteExpired(ctx, time.Now().UTC(), 3)
	if err != nil {
		t.Fatalf("DeleteExpired = %v", err)
	}
	if n != 3 {
		t.Errorf("deleted %d keys, want the batch limit of 3", n)
	}
}
