// Command ledgerd is the API server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/httpapi"
	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/payments"
	"github.com/Madhav-000-s/ledgerd/internal/platform/config"
	"github.com/Madhav-000-s/ledgerd/internal/platform/log"
	"github.com/Madhav-000-s/ledgerd/internal/platform/metrics"
	"github.com/Madhav-000-s/ledgerd/internal/postgres"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet if configuration failed, so this one goes to
		// stderr directly.
		fmt.Fprintf(os.Stderr, "ledgerd: %v\n", err)
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

	// NotifyContext cancels on SIGINT or SIGTERM, which is what starts the drain.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = log.Into(ctx, logger)

	db, err := postgres.Open(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer db.Close()

	// Migrations run at startup. For a single-writer deployment this is the simplest
	// thing that works; a multi-replica rollout would move it to a job so that N
	// replicas do not race to apply the same migration.
	if err := postgres.Migrate(ctx, db); err != nil {
		return err
	}
	version, err := postgres.SchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	logger.Info("schema ready", slog.Int64("version", version))

	m := metrics.New()

	ledgerRepo := postgres.NewLedgerRepo(db)
	ledgerSvc := ledger.NewService(ledgerRepo)

	paymentSvc, err := payments.NewService(ledgerSvc, payments.Options{
		Fees:                payments.FeeSchedule{Bps: 290, Fixed: 30},
		RefundPercentageFee: cfg.Ledger.RefundPercentageFee,
	})
	if err != nil {
		return err
	}

	api := httpapi.NewServer(httpapi.Options{
		Config:      cfg.HTTP,
		Ledger:      ledgerSvc,
		Tx:          postgres.NewTxManager(db, cfg.Ledger.Isolation),
		Keys:        postgres.NewAPIKeyRepo(db),
		Health:      db,
		Payments:    paymentSvc,
		Idempotency: postgres.NewIdempotencyRepo(db),
	})

	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.Handle("/", api)

	server := &http.Server{
		Addr:         cfg.HTTP.Addr,
		Handler:      mux,
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
		IdleTimeout:  cfg.HTTP.IdleTimeout,
		BaseContext:  func(net.Listener) context.Context { return ctx },
	}

	// Report pool utilization in the background, so contention is visible before it
	// becomes a latency complaint.
	go pollPoolStats(ctx, db, m, 10*time.Second)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening",
			slog.String("addr", cfg.HTTP.Addr),
			slog.String("env", cfg.Env),
			slog.String("isolation", string(cfg.Ledger.Isolation)),
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Graceful shutdown: stop accepting, then let in-flight requests finish. A request
	// that is mid-transaction has to be allowed to commit or roll back, or the drain
	// leaves locks held until the server times them out.
	logger.Info("shutting down", slog.Duration("grace", cfg.HTTP.ShutdownTimeout))

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.HTTP.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("ledgerd: draining: %w", err)
	}
	logger.Info("stopped")
	return nil
}

func pollPoolStats(ctx context.Context, db *postgres.DB, m *metrics.Metrics, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s := db.Stats()
			m.DBPoolAcquired.Set(float64(s.Acquired))
			m.DBPoolIdle.Set(float64(s.Idle))
			m.DBPoolWaitingAcquires.Set(float64(s.AcquireWaits))
		}
	}
}
