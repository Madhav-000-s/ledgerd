//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/events"
	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/money"
	"github.com/Madhav-000-s/ledgerd/internal/platform/config"
	"github.com/Madhav-000-s/ledgerd/internal/platform/metrics"
	"github.com/Madhav-000-s/ledgerd/internal/postgres"
	"github.com/Madhav-000-s/ledgerd/internal/webhooks"
	"github.com/Madhav-000-s/ledgerd/internal/workers"
)

// The reconciler is the only thing that can catch a balance that drifted in the past:
// the domain validates one transaction at a time and the database validates it at
// commit, so neither can notice that a stored figure stopped matching its entries.
//
// These tests exercise the actual SQL. A reconciler that silently returns "clean"
// because its query is wrong is worse than no reconciler at all, since it converts an
// unnoticed problem into an actively misleading all-clear.
func TestReconcilerOnACleanLedger(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)

	cash := e.account("cash", ledger.Asset, money.USD, true)
	payable := e.account("payable", ledger.Liability, money.USD, true)

	for i := 0; i < 10; i++ {
		e.post(ledger.TransactionSpec{
			MerchantID: testMerchant,
			Entries: []ledger.EntrySpec{
				entry(cash.ID, ledger.Debit, int64(100+i)),
				entry(payable.ID, ledger.Credit, int64(100+i)),
			},
		})
	}

	rec := workers.NewReconciler(postgres.NewLedgerChecker(e.db), metrics.New(), nil)

	report, err := rec.Run(e.ctx)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if !report.Clean() {
		t.Errorf("a correct ledger did not reconcile clean: %+v", report)
	}
	if got := report.TrialBalances["USD"]; got != 0 {
		t.Errorf("trial balance = %d, want 0", got)
	}
	if len(report.Drifted) != 0 {
		t.Errorf("%d accounts reported drifted on a clean ledger", len(report.Drifted))
	}
}

// The test that matters: corrupt a materialized balance behind the ledger's back and
// confirm the reconciler notices. If this does not fail, I7 is not actually being checked.
func TestReconcilerDetectsInjectedBalanceDrift(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)

	cash := e.account("cash", ledger.Asset, money.USD, true)
	payable := e.account("payable", ledger.Liability, money.USD, true)

	e.post(ledger.TransactionSpec{
		MerchantID: testMerchant,
		Entries: []ledger.EntrySpec{
			entry(cash.ID, ledger.Debit, 5000),
			entry(payable.ID, ledger.Credit, 5000),
		},
	})

	checker := postgres.NewLedgerChecker(e.db)
	rec := workers.NewReconciler(checker, metrics.New(), nil)

	if report, err := rec.Run(e.ctx); err != nil || !report.Clean() {
		t.Fatalf("the ledger was not clean before the injection: %v, %+v", err, report)
	}

	// Corrupt the materialized balance. account_balances has no immutability rule — only
	// entries do — so this is exactly the shape of the bug the reconciler exists for: a
	// balance that no longer matches the log that produced it.
	if _, err := e.db.Pool().Exec(e.ctx,
		`UPDATE account_balances SET posted_minor = posted_minor + 100 WHERE account_id = $1`,
		cash.ID); err != nil {
		t.Fatalf("injecting drift: %v", err)
	}

	report, err := rec.Run(e.ctx)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if report.Clean() {
		t.Fatal("the reconciler reported clean with a corrupted balance; I7 is not being checked")
	}

	if len(report.Drifted) != 1 {
		t.Fatalf("%d accounts reported drifted, want 1", len(report.Drifted))
	}
	drift := report.Drifted[0]
	if drift.AccountID != cash.ID {
		t.Errorf("drifted account = %s, want %s", drift.AccountID, cash.ID)
	}
	if drift.Delta() != 100 {
		t.Errorf("delta = %d, want 100", drift.Delta())
	}
	if drift.Materialized != 5100 || drift.Derived != 5000 {
		t.Errorf("materialized/derived = %d/%d, want 5100/5000", drift.Materialized, drift.Derived)
	}

	// The trial balance moves too, because the corrupted figure no longer nets out.
	if got := report.TrialBalances["USD"]; got != 100 {
		t.Errorf("trial balance = %d, want 100", got)
	}
}

// Pending entries must not count towards the posted balance, or every outstanding
// authorization would look like drift.
func TestReconcilerIgnoresPendingEntries(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)

	clearing := e.account("clearing", ledger.Asset, money.USD, true)
	holds := e.account("holds", ledger.Liability, money.USD, true)

	e.post(ledger.TransactionSpec{
		MerchantID: testMerchant,
		Pending:    true,
		Entries: []ledger.EntrySpec{
			entry(clearing.ID, ledger.Debit, 10000),
			entry(holds.ID, ledger.Credit, 10000),
		},
	})

	report, err := workers.NewReconciler(postgres.NewLedgerChecker(e.db), metrics.New(), nil).Run(e.ctx)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if !report.Clean() {
		t.Errorf("an outstanding authorization was reported as drift: %+v", report)
	}
}

// Every currency is checked separately: a transaction is single-currency by invariant, so
// summing across currencies would compare yen against cents and produce nonsense.
func TestReconcilerChecksEachCurrencySeparately(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)

	usdCash := e.account("usd_cash", ledger.Asset, money.USD, true)
	usdPayable := e.account("usd_payable", ledger.Liability, money.USD, true)
	jpyCash := e.account("jpy_cash", ledger.Asset, money.JPY, true)
	jpyPayable := e.account("jpy_payable", ledger.Liability, money.JPY, true)

	e.post(ledger.TransactionSpec{
		MerchantID: testMerchant,
		Entries: []ledger.EntrySpec{
			entry(usdCash.ID, ledger.Debit, 10000),
			entry(usdPayable.ID, ledger.Credit, 10000),
		},
	})
	e.post(ledger.TransactionSpec{
		MerchantID: testMerchant,
		Entries: []ledger.EntrySpec{
			{AccountID: jpyCash.ID, Direction: ledger.Debit, Amount: money.MustNew(500, money.JPY)},
			{AccountID: jpyPayable.ID, Direction: ledger.Credit, Amount: money.MustNew(500, money.JPY)},
		},
	})

	report, err := workers.NewReconciler(postgres.NewLedgerChecker(e.db), metrics.New(), nil).Run(e.ctx)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if !report.Clean() {
		t.Errorf("a two-currency ledger did not reconcile clean: %+v", report)
	}
	if len(report.TrialBalances) != 2 {
		t.Errorf("checked %d currencies, want 2", len(report.TrialBalances))
	}
	for currency, drift := range report.TrialBalances {
		if drift != 0 {
			t.Errorf("%s trial balance = %d, want 0", currency, drift)
		}
	}
}

// The reaper against the real table, driven through the same interface workerd uses.
func TestReaperAgainstRealPostgres(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)
	store := postgres.NewIdempotencyRepo(e.db)

	for i := 0; i < 7; i++ {
		rec := seedRecord("reap-" + string(rune('a'+i)))
		rec.ExpiresAt = time.Now().UTC().Add(-time.Hour)
		if _, _, err := store.Acquire(e.ctx, rec); err != nil {
			t.Fatalf("Acquire = %v", err)
		}
	}
	live := seedRecord("reap-live")
	if _, _, err := store.Acquire(e.ctx, live); err != nil {
		t.Fatalf("Acquire = %v", err)
	}

	deleted, err := workers.NewReaper(store, metrics.New(), 3, nil).Run(e.ctx)
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if deleted != 7 {
		t.Errorf("reaped %d keys, want 7", deleted)
	}

	var remaining int64
	if err := e.db.Pool().QueryRow(e.ctx, `SELECT count(*) FROM idempotency_keys`).Scan(&remaining); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if remaining != 1 {
		t.Errorf("%d keys left, want 1 (the unexpired one)", remaining)
	}
}

// The read and administrative paths on the webhook store, and the two queue gauges that
// back the metrics workerd publishes.
func TestWebhookStoreReadPaths(t *testing.T) {
	w := newWebhookEnv(t)

	ep, _ := w.endpoint(t, "https://a.example.com/hook", "payment.*")
	w.emit(t, events.TypePaymentSucceeded, "pay_1")

	if _, err := w.dispatcher.DispatchOnce(w.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}

	listed, err := w.webhookRepo.ListEndpoints(w.ctx, testMerchant)
	if err != nil {
		t.Fatalf("ListEndpoints = %v", err)
	}
	if len(listed) != 1 || listed[0].ID != ep.ID {
		t.Errorf("ListEndpoints returned %+v", listed)
	}

	depth, err := w.webhookRepo.QueueDepth(w.ctx)
	if err != nil {
		t.Fatalf("QueueDepth = %v", err)
	}
	if depth[webhooks.StatePending] != 1 {
		t.Errorf("pending depth = %d, want 1", depth[webhooks.StatePending])
	}

	// The real service-level indicator: not how much work there is, but whether any of
	// it is stuck.
	if _, err := w.webhookRepo.OldestPendingSeconds(w.ctx); err != nil {
		t.Fatalf("OldestPendingSeconds = %v", err)
	}

	// Rotation: a second secret, then retiring the first.
	secrets, err := w.webhookRepo.ActiveSecrets(w.ctx, ep.ID)
	if err != nil {
		t.Fatalf("ActiveSecrets = %v", err)
	}
	if len(secrets) != 1 {
		t.Fatalf("%d active secrets, want 1", len(secrets))
	}
	if err := w.webhookRepo.RetireSecret(w.ctx, secrets[0].ID); err != nil {
		t.Fatalf("RetireSecret = %v", err)
	}
	if _, err := w.webhookRepo.ActiveSecrets(w.ctx, ep.ID); err == nil {
		t.Error("ActiveSecrets returned secrets after the only one was retired")
	}

	// Events are readable by the merchant that owns them, and only that merchant.
	stored, _, err := w.eventRepo.List(w.ctx, testMerchant, 10, "")
	if err != nil {
		t.Fatalf("List = %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("%d events listed, want 1", len(stored))
	}
	if _, err := w.eventRepo.Get(w.ctx, testMerchant, stored[0].ID); err != nil {
		t.Errorf("Get = %v", err)
	}
	if _, err := w.eventRepo.Get(w.ctx, "mer_other", stored[0].ID); err == nil {
		t.Error("another merchant could read the event")
	}
}

// The pool statistics that back the db_pool_* gauges, and the readiness ping.
func TestPoolStatsAndPing(t *testing.T) {
	e := newEnv(t, config.IsolationReadCommitted)

	if err := e.db.Ping(e.ctx); err != nil {
		t.Errorf("Ping = %v", err)
	}

	stats := e.db.Stats()
	if stats.Max <= 0 {
		t.Errorf("pool max = %d, want a positive limit", stats.Max)
	}
	if stats.Total < 0 || stats.Idle < 0 || stats.Acquired < 0 {
		t.Errorf("pool stats are negative: %+v", stats)
	}

	version, err := postgres.SchemaVersion(e.ctx, e.db)
	if err != nil {
		t.Fatalf("SchemaVersion = %v", err)
	}
	if version < 5 {
		t.Errorf("schema version = %d, want at least 5", version)
	}
}
