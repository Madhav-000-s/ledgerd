package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesRegisteredMetrics(t *testing.T) {
	m := New()

	// Give the instruments a value each, so they appear in the exposition. A counter with
	// no observations is not exported, which would make this test pass vacuously.
	m.HTTPRequests.WithLabelValues("/v1/payments", "POST", "201").Inc()
	m.HTTPDuration.WithLabelValues("/v1/payments").Observe(0.05)
	m.LedgerTransactions.WithLabelValues("posted").Inc()
	m.LedgerLockWait.Observe(0.002)
	m.IdempotencyOutcomes.WithLabelValues("replay").Inc()
	m.TrialBalanceDrift.WithLabelValues("USD").Set(0)
	m.BalanceDriftAccounts.Set(0)
	m.WebhookAttempts.WithLabelValues("succeeded").Inc()
	m.WebhookLatency.Observe(0.12)
	m.WebhookQueueDepth.WithLabelValues("pending").Set(3)
	m.WebhookOldestPending.Set(12)
	m.ReconcilerRuns.WithLabelValues("clean").Inc()
	m.ReaperDeleted.Add(5)
	m.DBPoolAcquired.Set(2)
	m.DBPoolIdle.Set(8)
	m.DBPoolWaitingAcquires.Set(0)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()

	// The two P0 gauges have to be present even when they read zero: an alert on a
	// metric that is only exported once it goes wrong will never fire the first time.
	for _, name := range []string{
		"ledger_trial_balance_drift_minor",
		"ledger_balance_drift_accounts",
		"http_requests_total",
		"http_request_duration_seconds",
		"ledger_transactions_total",
		"ledger_lock_wait_seconds",
		"idempotency_outcomes_total",
		"webhook_delivery_attempts_total",
		"webhook_delivery_latency_seconds",
		"webhook_queue_depth",
		"webhook_oldest_pending_seconds",
		"reconciler_runs_total",
		"reaper_expired_keys_deleted_total",
		"db_pool_acquired",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("exposition does not contain %s", name)
		}
	}
}

func TestRegistryIsIsolatedPerInstance(t *testing.T) {
	// Two instances must not fight over the default registry; a duplicate registration
	// would panic, which is exactly what a shared default causes in tests.
	first := New()
	second := New()

	if first.Registry() == second.Registry() {
		t.Error("two Metrics instances share a registry")
	}

	first.ReaperDeleted.Add(1)

	families, err := second.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather = %v", err)
	}
	for _, f := range families {
		if f.GetName() == "reaper_expired_keys_deleted_total" {
			for _, metric := range f.GetMetric() {
				if got := metric.GetCounter().GetValue(); got != 0 {
					t.Errorf("the second registry saw the first's value: %v", got)
				}
			}
		}
	}
}
