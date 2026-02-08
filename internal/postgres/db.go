// Package postgres implements the persistence interfaces the domain declares.
//
// The dependency points inward: ledger defines ledger.Repository and this package
// satisfies it, never the reverse. Nothing here leaks a pgx type past its own boundary.
//
// SQL is written by hand and kept visible in the file that uses it. The locking
// semantics are the whole point of this layer, and an ORM would hide exactly the part
// that has to be reviewable.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Madhav-000-s/ledgerd/internal/platform/config"
)

// DB owns the connection pool.
type DB struct {
	pool *pgxpool.Pool
	cfg  config.DBConfig
}

// Open creates a pool and verifies it can reach the database.
//
// lock_timeout and statement_timeout are set on every connection rather than per query,
// so that a pathological case fails loudly instead of holding a lock on a hot account
// until something else times out first.
func Open(ctx context.Context, cfg config.DBConfig) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parsing DATABASE_URL: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolCfg.ConnConfig.RuntimeParams["lock_timeout"] = millis(cfg.LockTimeout)
	poolCfg.ConnConfig.RuntimeParams["statement_timeout"] = millis(cfg.StatementTimeout)
	poolCfg.ConnConfig.RuntimeParams["application_name"] = "ledgerd"

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: creating pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	return &DB{pool: pool, cfg: cfg}, nil
}

// NewFromPool wraps an existing pool. The integration suite uses it to attach to a
// container it manages itself.
func NewFromPool(pool *pgxpool.Pool, cfg config.DBConfig) *DB {
	return &DB{pool: pool, cfg: cfg}
}

// Pool exposes the underlying pool for health checks and metrics.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Close releases every connection.
func (db *DB) Close() { db.pool.Close() }

// Ping reports whether the database is reachable. It backs the readiness probe.
func (db *DB) Ping(ctx context.Context) error {
	if err := db.pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: ping: %w", err)
	}
	return nil
}

// Stats reports pool utilization for the db_pool_* metrics.
func (db *DB) Stats() PoolStats {
	s := db.pool.Stat()
	return PoolStats{
		Acquired:        s.AcquiredConns(),
		Idle:            s.IdleConns(),
		Total:           s.TotalConns(),
		Max:             s.MaxConns(),
		AcquireWaits:    s.EmptyAcquireCount(),
		AcquireDuration: s.AcquireDuration(),
	}
}

// PoolStats is a snapshot of connection-pool utilization.
type PoolStats struct {
	Acquired        int32
	Idle            int32
	Total           int32
	Max             int32
	AcquireWaits    int64
	AcquireDuration time.Duration
}

func millis(d time.Duration) string {
	return fmt.Sprintf("%d", d.Milliseconds())
}

// sqlc.DBTX is the subset of pgx that both a pool and a transaction provide.
// Repositories resolve one from the context rather than taking a concrete type, which is
// what lets the same generated query code run inside or outside a transaction without
// threading a *pgx.Tx through every signature.
