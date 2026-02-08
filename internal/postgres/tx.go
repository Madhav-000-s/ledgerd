package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Madhav-000-s/ledgerd/internal/platform/config"
	"github.com/Madhav-000-s/ledgerd/internal/postgres/sqlc"
)

// TxManager composes repository calls into a single database transaction.
//
// This is the most important internal API in the codebase. The ledger write, the outbox
// insert and the idempotency record must commit together or the system's central
// guarantees do not hold: an event for money that did not move, or money that moved
// without a replayable response, are both reconciliation incidents.
//
// The transaction travels in the context under an unexported key, so repositories pick
// it up automatically and no signature has to carry a *pgx.Tx.
type TxManager struct {
	db        *DB
	isolation config.Isolation
}

// NewTxManager returns a TxManager over db.
func NewTxManager(db *DB, isolation config.Isolation) *TxManager {
	if isolation == "" {
		isolation = config.IsolationReadCommitted
	}
	return &TxManager{db: db, isolation: isolation}
}

type txKey struct{}

// InTx runs fn inside a database transaction. Repositories called within fn join it
// automatically.
//
// A nested InTx joins the outer transaction rather than opening a savepoint. Savepoints
// would let an inner failure be swallowed while the outer transaction commits, which for
// a ledger write is precisely the wrong default: a failed step must take the whole
// movement with it. InNestedTx exists for the rare case that genuinely wants the other
// behavior.
//
// Two rules apply to fn, and breaking either is how a ledger deadlocks under load:
//
//   - No external I/O. No HTTP calls, no sleeps, no waiting on another goroutine.
//     Holding a transaction open across a network call holds its row locks too.
//   - Only the service layer calls InTx. Handlers and repositories never do.
func (m *TxManager) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := txFrom(ctx); ok {
		// Already inside a transaction: join it. The outer InTx owns commit and
		// rollback.
		return fn(ctx)
	}

	opts := pgx.TxOptions{AccessMode: pgx.ReadWrite}
	if m.isolation == config.IsolationSerializable {
		opts.IsoLevel = pgx.Serializable
	} else {
		opts.IsoLevel = pgx.ReadCommitted
	}

	tx, err := m.db.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}

	// Roll back on any non-nil error and on panic. Rollback after a successful commit
	// is a documented no-op in pgx, so the deferred call is safe either way.
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
	}()

	if err := fn(txInto(ctx, tx)); err != nil {
		// Use a context detached from cancellation: if the request was cancelled, the
		// rollback still has to reach the server, or the transaction lingers holding
		// locks until the server times it out.
		if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return errors.Join(err, fmt.Errorf("postgres: rollback: %w", rbErr))
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

// InNestedTx runs fn inside a savepoint when already in a transaction, so that fn can
// fail without aborting the enclosing work. It is deliberately not used by any current
// code path; the ledger wants an inner failure to take the whole movement with it.
func (m *TxManager) InNestedTx(ctx context.Context, fn func(ctx context.Context) error) error {
	outer, ok := txFrom(ctx)
	if !ok {
		return m.InTx(ctx, fn)
	}

	sp, err := outer.Begin(ctx) // pgx turns a nested Begin into a savepoint
	if err != nil {
		return fmt.Errorf("postgres: savepoint: %w", err)
	}

	if err := fn(txInto(ctx, sp)); err != nil {
		if rbErr := sp.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return errors.Join(err, fmt.Errorf("postgres: savepoint rollback: %w", rbErr))
		}
		return err
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: savepoint commit: %w", err)
	}
	return nil
}

// InTx reports whether ctx is already inside a transaction. Services assert on it
// before doing work that must be atomic.
func InTx(ctx context.Context) bool {
	_, ok := txFrom(ctx)
	return ok
}

func txInto(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

func txFrom(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok && tx != nil
}

// q resolves the querier for this call: the context's transaction if there is one, the
// pool otherwise. Every repository method starts with it.
func (db *DB) q(ctx context.Context) sqlc.DBTX {
	if tx, ok := txFrom(ctx); ok {
		return tx
	}
	return db.pool
}
