package workers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/platform/metrics"
)

// fakeChecker stands in for the aggregate queries.
type fakeChecker struct {
	currencies []string
	trial      map[string]int64
	drifted    []AccountDrift
	err        error
}

func (f *fakeChecker) Currencies(context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.currencies, nil
}

func (f *fakeChecker) TrialBalance(_ context.Context, currency string) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.trial[currency], nil
}

func (f *fakeChecker) DriftedAccounts(context.Context) ([]AccountDrift, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.drifted, nil
}

func TestReconcilerCleanLedger(t *testing.T) {
	checker := &fakeChecker{
		currencies: []string{"USD", "JPY"},
		trial:      map[string]int64{"USD": 0, "JPY": 0},
	}

	report, err := NewReconciler(checker, metrics.New(), nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if !report.Clean() {
		t.Errorf("a balanced ledger did not report clean: %+v", report)
	}
	if len(report.TrialBalances) != 2 {
		t.Errorf("checked %d currencies, want 2", len(report.TrialBalances))
	}
}

// A non-zero trial balance means money was created or destroyed. It is a finding rather
// than a job failure, so Run reports it without erroring — the caller has to be able to
// tell "I could not check" from "I checked and the books are wrong".
func TestReconcilerReportsTrialBalanceDrift(t *testing.T) {
	checker := &fakeChecker{
		currencies: []string{"USD"},
		trial:      map[string]int64{"USD": -250},
	}

	report, err := NewReconciler(checker, metrics.New(), nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run = %v, want nil; a drift is a finding, not a failure", err)
	}
	if report.Clean() {
		t.Error("a drifted ledger reported clean")
	}
	if report.TrialBalances["USD"] != -250 {
		t.Errorf("drift = %d, want -250", report.TrialBalances["USD"])
	}
}

// I7: the materialized balance disagreeing with the entry log is the drift nothing else
// can catch, because the domain and the trigger only ever see one transaction.
func TestReconcilerReportsAccountDrift(t *testing.T) {
	checker := &fakeChecker{
		currencies: []string{"USD"},
		trial:      map[string]int64{"USD": 0},
		drifted: []AccountDrift{
			{AccountID: "acct_1", Materialized: 1000, Derived: 900},
		},
	}

	report, err := NewReconciler(checker, metrics.New(), nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if report.Clean() {
		t.Error("an account drift reported clean")
	}
	if len(report.Drifted) != 1 {
		t.Fatalf("got %d drifted accounts, want 1", len(report.Drifted))
	}
	if got := report.Drifted[0].Delta(); got != 100 {
		t.Errorf("delta = %d, want 100", got)
	}
}

// A query failure is a real error: the reconciler could not check, which is different
// from having checked and found nothing.
func TestReconcilerPropagatesQueryFailure(t *testing.T) {
	sentinel := errors.New("connection reset")
	checker := &fakeChecker{err: sentinel}

	if _, err := NewReconciler(checker, metrics.New(), nil).Run(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("Run = %v, want the underlying error", err)
	}
}

func TestReportCleanRequiresBoth(t *testing.T) {
	tests := []struct {
		name  string
		trial map[string]int64
		drift []AccountDrift
		clean bool
	}{
		{"nothing wrong", map[string]int64{"USD": 0}, nil, true},
		{"trial drift", map[string]int64{"USD": 1}, nil, false},
		{"account drift", map[string]int64{"USD": 0}, []AccountDrift{{AccountID: "a"}}, false},
		{"both", map[string]int64{"USD": 1}, []AccountDrift{{AccountID: "a"}}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &Report{TrialBalances: tc.trial, Drifted: tc.drift}
			if r.Clean() != tc.clean {
				t.Errorf("Clean() = %v, want %v", r.Clean(), tc.clean)
			}
		})
	}
}

// fakeExpiry deletes in batches, so the reaper's looping can be observed.
type fakeExpiry struct {
	remaining int64
	calls     int
	err       error
}

func (f *fakeExpiry) DeleteExpired(_ context.Context, _ time.Time, limit int) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.calls++

	deleted := int64(limit)
	if f.remaining < deleted {
		deleted = f.remaining
	}
	f.remaining -= deleted
	return deleted, nil
}

// The reaper keeps going until a short batch tells it the backlog is clear. Bounded
// batches are not an optimization: one unbounded DELETE over a weekend's worth of keys
// would hold locks long enough for every request trying to claim one to notice.
func TestReaperDrainsInBatches(t *testing.T) {
	store := &fakeExpiry{remaining: 2500}

	total, err := NewReaper(store, metrics.New(), 1000, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if total != 2500 {
		t.Errorf("deleted %d, want 2500", total)
	}
	// 1000, 1000, 500 — the short third batch ends the loop.
	if store.calls != 3 {
		t.Errorf("made %d batches, want 3", store.calls)
	}
}

func TestReaperStopsOnAnEmptyFirstBatch(t *testing.T) {
	store := &fakeExpiry{remaining: 0}

	total, err := NewReaper(store, metrics.New(), 1000, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if total != 0 {
		t.Errorf("deleted %d, want 0", total)
	}
	if store.calls != 1 {
		t.Errorf("made %d batches, want 1", store.calls)
	}
}

func TestReaperPropagatesFailure(t *testing.T) {
	sentinel := errors.New("deadlock detected")
	store := &fakeExpiry{err: sentinel}

	if _, err := NewReaper(store, metrics.New(), 100, nil).Run(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("Run = %v, want the underlying error", err)
	}
}

// A cancelled context must stop the loop rather than keep hammering a database that is
// shutting down underneath it.
func TestReaperHonoursCancellation(t *testing.T) {
	store := &fakeExpiry{remaining: 1_000_000}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewReaper(store, metrics.New(), 10, nil).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want context.Canceled", err)
	}
}
