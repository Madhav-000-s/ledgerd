// Command workerd runs the background jobs: the outbox dispatcher, the webhook delivery
// workers, the lease sweeper, the reconciler and the reaper.
//
// It is a separate binary from the API so that a slow merchant endpoint cannot consume
// the connection pool the API depends on, and so the two can be scaled independently.
// Several replicas are safe: every job claims its work with SKIP LOCKED, so competing
// consumers need no coordination.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Madhav-000-s/ledgerd/internal/platform/config"
	"github.com/Madhav-000-s/ledgerd/internal/platform/log"
	"github.com/Madhav-000-s/ledgerd/internal/platform/metrics"
	"github.com/Madhav-000-s/ledgerd/internal/postgres"
	"github.com/Madhav-000-s/ledgerd/internal/webhooks"
	"github.com/Madhav-000-s/ledgerd/internal/workers"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "workerd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := log.New(os.Stdout, log.Options{
		Level:  cfg.Log.Level,
		Format: cfg.Log.Format,
		Source: !cfg.IsProd(),
	})
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = log.Into(ctx, logger)

	db, err := postgres.Open(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer db.Close()

	cipher, err := webhooks.NewCipher(cfg.Webhooks.SecretKey)
	if err != nil {
		return fmt.Errorf("workerd: %w", err)
	}

	m := metrics.New()
	txm := postgres.NewTxManager(db, cfg.Ledger.Isolation)

	eventRepo := postgres.NewEventRepo(db)
	webhookRepo := postgres.NewWebhookRepo(db, cipher)

	dispatcher := webhooks.NewDispatcher(eventRepo, webhookRepo, txm, cfg.Worker.DispatchBatchSize, nil)

	worker := webhooks.NewWorker(webhooks.WorkerOptions{
		Store: webhookRepo,
		Client: webhooks.NewClient(webhooks.ClientOptions{
			AllowInsecure:    cfg.Webhooks.AllowInsecure,
			AllowPrivateIPs:  cfg.Webhooks.AllowPrivateIPs,
			RequestTimeout:   cfg.Webhooks.RequestTimeout,
			ConnectTimeout:   cfg.Webhooks.ConnectTimeout,
			MaxResponseBytes: cfg.Webhooks.MaxRespBytes,
		}),
		Cipher: cipher,
		Backoff: webhooks.Backoff{
			Base: cfg.Webhooks.BackoffBase,
			Cap:  cfg.Webhooks.BackoffCap,
		},
		Tx:          txm,
		MaxAttempts: cfg.Webhooks.MaxAttempts,
		Lease:       cfg.Worker.DeliveryLease,
		BatchSize:   cfg.Worker.DeliveryBatchSize,
	})

	reconciler := workers.NewReconciler(postgres.NewLedgerChecker(db), m, nil)
	reaper := workers.NewReaper(postgres.NewIdempotencyRepo(db), m, cfg.Worker.ReapBatchSize, nil)

	// errgroup with a bounded set of long-lived jobs. Nothing spawns a goroutine per
	// delivery: an unbounded fan-out against a merchant endpoint that has gone slow is
	// how a worker runs itself out of memory.
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return loop(gctx, "dispatcher", cfg.Worker.DispatchPollPeriod, func(ctx context.Context) error {
			_, err := dispatcher.DispatchOnce(ctx)
			return err
		})
	})

	for i := 0; i < cfg.Worker.DeliveryWorkers; i++ {
		g.Go(func() error {
			return loop(gctx, "delivery", 250*time.Millisecond, func(ctx context.Context) error {
				_, err := worker.DeliverOnce(ctx)
				return err
			})
		})
	}

	// The sweeper is what makes a worker crash recoverable rather than a stuck delivery.
	g.Go(func() error {
		return loop(gctx, "lease-sweeper", cfg.Worker.DeliveryLease/2, func(ctx context.Context) error {
			n, err := worker.ReclaimOnce(ctx)
			if err == nil && n > 0 {
				log.From(ctx).Warn("reclaimed leases from crashed workers", slog.Int64("count", n))
			}
			return err
		})
	})

	g.Go(func() error {
		return loop(gctx, "reconciler", cfg.Worker.ReconcilePeriod, func(ctx context.Context) error {
			report, err := reconciler.Run(ctx)
			if err != nil {
				return err
			}
			// A drift is a finding rather than a job failure, so it is logged and
			// gauged — and paged on — without taking the worker down.
			if !report.Clean() {
				log.From(ctx).Error("reconciler found drift; this is a P0",
					slog.Int("drifted_accounts", len(report.Drifted)))
			}
			return nil
		})
	})

	g.Go(func() error {
		return loop(gctx, "reaper", cfg.Worker.ReapPeriod, func(ctx context.Context) error {
			_, err := reaper.Run(ctx)
			return err
		})
	})

	g.Go(func() error {
		return loop(gctx, "queue-metrics", 15*time.Second, func(ctx context.Context) error {
			depth, err := webhookRepo.QueueDepth(ctx)
			if err != nil {
				return err
			}
			for state, n := range depth {
				m.WebhookQueueDepth.WithLabelValues(string(state)).Set(float64(n))
			}

			oldest, err := webhookRepo.OldestPendingSeconds(ctx)
			if err != nil {
				return err
			}
			m.WebhookOldestPending.Set(float64(oldest))
			return nil
		})
	})

	// A metrics endpoint, so the worker is observable without an API in front of it.
	metricsServer := &http.Server{
		Addr:              ":9090",
		Handler:           metricsMux(m),
		ReadHeaderTimeout: 5 * time.Second,
	}
	g.Go(func() error {
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return metricsServer.Shutdown(shutdownCtx)
	})

	logger.Info("workerd started",
		slog.Int("delivery_workers", cfg.Worker.DeliveryWorkers),
		slog.Duration("reconcile_period", cfg.Worker.ReconcilePeriod),
	)

	if err := g.Wait(); err != nil && !isShutdown(err) {
		return err
	}
	logger.Info("workerd stopped")
	return nil
}

// loop runs fn on a period until the context is cancelled.
//
// An error from one pass is logged and the loop continues. A transient database blip
// must not take a worker down: the next tick will pick the work up, and exiting would
// mean the whole pool restarts together and hammers a database that is already unwell.
func loop(ctx context.Context, name string, every time.Duration, fn func(context.Context) error) error {
	if every <= 0 {
		every = time.Second
	}

	ticker := time.NewTicker(every)
	defer ticker.Stop()

	logger := log.From(ctx).With(slog.String("job", name))

	for {
		select {
		case <-ctx.Done():
			logger.Debug("job stopping")
			return nil
		case <-ticker.C:
			if err := fn(ctx); err != nil {
				if isShutdown(err) {
					return nil
				}
				logger.Error("job pass failed", log.Err(err))
			}
		}
	}
}

func isShutdown(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

func metricsMux(m *metrics.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}
