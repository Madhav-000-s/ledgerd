// Package workers holds the background jobs: the reconciler that checks the books
// against themselves, and the reaper that expires idempotency keys.
package workers

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/platform/log"
	"github.com/Madhav-000-s/ledgerd/internal/platform/metrics"
)

// LedgerChecker exposes the two aggregate queries the reconciler needs.
type LedgerChecker interface {
	// TrialBalance restates every posted balance in debit-positive terms and sums it,
	// per currency. It must be zero.
	TrialBalance(ctx context.Context, currency string) (int64, error)
	// Currencies returns the currencies that have accounts.
	Currencies(ctx context.Context) ([]string, error)
	// DriftedAccounts returns accounts whose materialized balance disagrees with the
	// balance derived from the entry log.
	DriftedAccounts(ctx context.Context) ([]AccountDrift, error)
}

// AccountDrift is one account where the materialized and derived balances disagree.
type AccountDrift struct {
	AccountID    string
	Materialized int64
	Derived      int64
}

// Delta is how far off the account is.
func (d AccountDrift) Delta() int64 { return d.Materialized - d.Derived }

// Report is the outcome of one reconciliation pass.
type Report struct {
	TrialBalances map[string]int64
	Drifted       []AccountDrift
	CheckedAt     time.Time
}

// Clean reports whether the books add up.
func (r *Report) Clean() bool {
	if len(r.Drifted) > 0 {
		return false
	}
	for _, drift := range r.TrialBalances {
		if drift != 0 {
			return false
		}
	}
	return true
}

// Reconciler checks the ledger against itself.
//
// It is a pager, not a dashboard. I7 (the materialized balance equals the sum of the
// entries) and I8 (the trial balance is zero) are the two invariants nothing else can
// catch: the domain checks a transaction in isolation and the database checks it at
// commit, but neither can notice that a balance drifted three weeks ago. If either of
// these is non-zero, money has been created or destroyed somewhere and every downstream
// number is suspect.
type Reconciler struct {
	checker LedgerChecker
	metrics *metrics.Metrics
	now     func() time.Time
}

// NewReconciler builds a reconciler.
func NewReconciler(checker LedgerChecker, m *metrics.Metrics, now func() time.Time) *Reconciler {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Reconciler{checker: checker, metrics: m, now: now}
}

// Run performs one pass and reports what it found.
//
// It returns a Report rather than an error for a drift: a drift is not a failure of the
// reconciler, it is a finding, and the caller has to be able to tell "I could not check"
// from "I checked and the books are wrong".
func (r *Reconciler) Run(ctx context.Context) (*Report, error) {
	report := &Report{
		TrialBalances: map[string]int64{},
		CheckedAt:     r.now(),
	}

	currencies, err := r.checker.Currencies(ctx)
	if err != nil {
		r.record("error")
		return nil, fmt.Errorf("workers: listing currencies: %w", err)
	}

	for _, currency := range currencies {
		drift, err := r.checker.TrialBalance(ctx, currency)
		if err != nil {
			r.record("error")
			return nil, fmt.Errorf("workers: trial balance for %s: %w", currency, err)
		}
		report.TrialBalances[currency] = drift

		if r.metrics != nil {
			r.metrics.TrialBalanceDrift.WithLabelValues(currency).Set(float64(drift))
		}
	}

	drifted, err := r.checker.DriftedAccounts(ctx)
	if err != nil {
		r.record("error")
		return nil, fmt.Errorf("workers: checking for balance drift: %w", err)
	}
	report.Drifted = drifted

	if r.metrics != nil {
		r.metrics.BalanceDriftAccounts.Set(float64(len(drifted)))
	}

	logger := log.From(ctx)
	if report.Clean() {
		r.record("clean")
		logger.Debug("reconciler pass clean", slog.Int("currencies", len(currencies)))
		return report, nil
	}

	r.record("drift")

	// Logged at error, one line per problem, with the numbers needed to start
	// investigating. balance_after on each entry makes the offending write bisectable
	// from here.
	for currency, drift := range report.TrialBalances {
		if drift != 0 {
			logger.Error("TRIAL BALANCE DRIFT — the books do not sum to zero",
				slog.String("currency", currency),
				slog.Int64("drift_minor", drift),
			)
		}
	}
	for _, d := range report.Drifted {
		logger.Error("BALANCE DRIFT — materialized balance disagrees with the entry log",
			slog.String(log.KeyAccountID, d.AccountID),
			slog.Int64("materialized", d.Materialized),
			slog.Int64("derived", d.Derived),
			slog.Int64("delta", d.Delta()),
		)
	}

	return report, nil
}

func (r *Reconciler) record(result string) {
	if r.metrics != nil {
		r.metrics.ReconcilerRuns.WithLabelValues(result).Inc()
	}
}

// Reaper deletes expired idempotency keys.
type Reaper struct {
	store     ExpiryStore
	metrics   *metrics.Metrics
	batchSize int
	now       func() time.Time
}

// ExpiryStore is the slice of the idempotency store the reaper needs.
type ExpiryStore interface {
	DeleteExpired(ctx context.Context, before time.Time, limit int) (int64, error)
}

// NewReaper builds a reaper.
func NewReaper(store ExpiryStore, m *metrics.Metrics, batchSize int, now func() time.Time) *Reaper {
	if batchSize <= 0 {
		batchSize = 1000
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Reaper{store: store, metrics: m, batchSize: batchSize, now: now}
}

// Run deletes expired keys in bounded batches until there are none left.
//
// The batching is not an optimization. A single unbounded DELETE over a table that has
// accumulated a weekend of keys would hold locks long enough to be noticed by every
// request trying to claim one.
func (r *Reaper) Run(ctx context.Context) (int64, error) {
	var total int64

	for {
		deleted, err := r.store.DeleteExpired(ctx, r.now(), r.batchSize)
		if err != nil {
			return total, fmt.Errorf("workers: reaping expired keys: %w", err)
		}
		total += deleted

		if r.metrics != nil {
			r.metrics.ReaperDeleted.Add(float64(deleted))
		}

		// A short batch means the backlog is cleared.
		if deleted < int64(r.batchSize) {
			break
		}
		// Yield between batches so a large backlog does not monopolize a connection.
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}

	if total > 0 {
		log.From(ctx).Info("reaped expired idempotency keys", slog.Int64("deleted", total))
	}
	return total, nil
}
