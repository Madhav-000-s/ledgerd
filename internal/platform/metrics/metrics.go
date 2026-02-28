// Package metrics defines the process's Prometheus instruments.
//
// The set is deliberately small. Two of these gauges are pager duty and the rest are
// dashboards, and mixing those up is how an alert that matters gets lost among forty
// that do not.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
)

// Metrics holds every instrument the service reports.
type Metrics struct {
	registry *prometheus.Registry

	HTTPRequests        *prometheus.CounterVec
	HTTPDuration        *prometheus.HistogramVec
	LedgerTransactions  *prometheus.CounterVec
	LedgerLockWait      prometheus.Histogram
	IdempotencyOutcomes *prometheus.CounterVec

	// TrialBalanceDrift and BalanceDriftAccounts must both be zero. They are the two
	// P0 alarms: a non-zero value means the books do not add up, which is a page rather
	// than a ticket and certainly not a number to smooth over on a dashboard.
	TrialBalanceDrift    *prometheus.GaugeVec
	BalanceDriftAccounts prometheus.Gauge

	WebhookAttempts       *prometheus.CounterVec
	WebhookLatency        prometheus.Histogram
	WebhookQueueDepth     *prometheus.GaugeVec
	WebhookOldestPending  prometheus.Gauge
	ReconcilerRuns        *prometheus.CounterVec
	ReaperDeleted         prometheus.Counter
	DBPoolAcquired        prometheus.Gauge
	DBPoolIdle            prometheus.Gauge
	DBPoolWaitingAcquires prometheus.Gauge
}

// New builds the instruments and registers them.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,

		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP requests by route, method and status.",
		}, []string{"route", "method", "status"}),

		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency by route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route"}),

		LedgerTransactions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ledger_transactions_total",
			Help: "Ledger transactions by result.",
		}, []string{"result"}),

		// Contention shows up here long before it shows up as a latency complaint, which
		// is the point: this is the early warning for a hot account.
		LedgerLockWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "ledger_lock_wait_seconds",
			Help:    "Time spent waiting to acquire account row locks.",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 3},
		}),

		IdempotencyOutcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "idempotency_outcomes_total",
			Help: "Idempotency outcomes: new, replay, conflict or mismatch.",
		}, []string{"outcome"}),

		TrialBalanceDrift: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ledger_trial_balance_drift_minor",
			Help: "Trial balance in minor units per currency. MUST be 0.",
		}, []string{"currency"}),

		BalanceDriftAccounts: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ledger_balance_drift_accounts",
			Help: "Accounts whose materialized balance disagrees with the entry log. MUST be 0.",
		}),

		WebhookAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "webhook_delivery_attempts_total",
			Help: "Webhook delivery attempts by outcome.",
		}, []string{"outcome"}),

		WebhookLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "webhook_delivery_latency_seconds",
			Help:    "Time taken by a webhook delivery attempt.",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2, 5},
		}),

		WebhookQueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "webhook_queue_depth",
			Help: "Webhook deliveries by state.",
		}, []string{"state"}),

		// The real service-level indicator. Queue depth says how much work there is;
		// this says whether any of it is stuck, which is the thing a merchant notices.
		WebhookOldestPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "webhook_oldest_pending_seconds",
			Help: "Age of the oldest undelivered webhook.",
		}),

		ReconcilerRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reconciler_runs_total",
			Help: "Reconciler runs by result.",
		}, []string{"result"}),

		ReaperDeleted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "reaper_expired_keys_deleted_total",
			Help: "Expired idempotency keys deleted.",
		}),

		DBPoolAcquired: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "db_pool_acquired", Help: "Connections currently checked out.",
		}),
		DBPoolIdle: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "db_pool_idle", Help: "Idle connections in the pool.",
		}),
		DBPoolWaitingAcquires: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "db_pool_waiting", Help: "Acquires that had to wait for a free connection.",
		}),
	}

	reg.MustRegister(
		m.HTTPRequests, m.HTTPDuration,
		m.LedgerTransactions, m.LedgerLockWait,
		m.IdempotencyOutcomes,
		m.TrialBalanceDrift, m.BalanceDriftAccounts,
		m.WebhookAttempts, m.WebhookLatency, m.WebhookQueueDepth, m.WebhookOldestPending,
		m.ReconcilerRuns, m.ReaperDeleted,
		m.DBPoolAcquired, m.DBPoolIdle, m.DBPoolWaitingAcquires,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

// Handler serves the metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Registry exposes the registry, for tests that want to gather directly.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }
