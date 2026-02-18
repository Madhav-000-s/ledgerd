package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Madhav-000-s/ledgerd/internal/idempotency"
	"github.com/Madhav-000-s/ledgerd/internal/postgres/sqlc"
)

// IdempotencyRepo implements idempotency.Store.
type IdempotencyRepo struct {
	db *DB
}

var _ idempotency.Store = (*IdempotencyRepo)(nil)

// NewIdempotencyRepo returns a store backed by db.
func NewIdempotencyRepo(db *DB) *IdempotencyRepo { return &IdempotencyRepo{db: db} }

func (r *IdempotencyRepo) q(ctx context.Context) *sqlc.Queries {
	return sqlc.New(r.db.q(ctx))
}

// Acquire claims the key, or returns the record that already holds it.
//
// The insert is the claim. ON CONFLICT DO NOTHING returns no row to the loser of a race,
// which then reads the winner's record — so of two concurrent retries exactly one runs
// the handler, and there is no window between checking and inserting for both to slip
// through.
func (r *IdempotencyRepo) Acquire(ctx context.Context, rec *idempotency.Record) (*idempotency.Record, bool, error) {
	q := r.q(ctx)

	row, err := q.AcquireIdempotencyKey(ctx, sqlc.AcquireIdempotencyKeyParams{
		ID:             rec.ID,
		MerchantID:     rec.MerchantID,
		IdempotencyKey: rec.Key,
		RequestMethod:  rec.Method,
		RequestPath:    rec.Path,
		RequestHash:    rec.RequestHash,
		State:          sqlc.IdemState(rec.State),
		RecoveryPoint:  string(rec.RecoveryPoint),
		LockExpiresAt:  timestamptzPtr(rec.LockExpiresAt),
		RequestID:      nullString(rec.RequestID),
		CreatedAt:      timestamptz(rec.CreatedAt),
		ExpiresAt:      timestamptz(rec.ExpiresAt),
	})
	if err == nil {
		return recordFromRow(row), true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("postgres: acquire idempotency key: %w", err)
	}

	// Someone else holds it. Read their record so the caller can compare fingerprints
	// and decide between replay, conflict and takeover.
	existing, err := r.Get(ctx, rec.MerchantID, rec.Key)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

func (r *IdempotencyRepo) Get(ctx context.Context, merchantID, key string) (*idempotency.Record, error) {
	row, err := r.q(ctx).GetIdempotencyKey(ctx, sqlc.GetIdempotencyKeyParams{
		MerchantID:     merchantID,
		IdempotencyKey: key,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, idempotency.ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get idempotency key: %w", err)
	}
	return recordFromRow(sqlc.IdempotencyKey(row)), nil
}

// Complete records the terminal outcome and the response to replay.
//
// Called from inside the caller's transaction, so it commits with the ledger entries.
// That is the ordering property in DESIGN.md §7.2: there is no instant at which money
// has moved but the response was not recorded.
func (r *IdempotencyRepo) Complete(
	ctx context.Context,
	recordID string,
	state idempotency.State,
	status int,
	body []byte,
	resourceID string,
) error {
	affected, err := r.q(ctx).CompleteIdempotencyKey(ctx, sqlc.CompleteIdempotencyKeyParams{
		ID:             recordID,
		State:          sqlc.IdemState(state),
		ResponseStatus: int32Ptr(status),
		ResponseBody:   body,
		ResourceID:     nullString(resourceID),
	})
	if err != nil {
		return fmt.Errorf("postgres: complete idempotency key: %w", err)
	}
	if affected == 0 {
		return idempotency.ErrKeyNotFound
	}
	return nil
}

func (r *IdempotencyRepo) Advance(ctx context.Context, recordID string, point idempotency.RecoveryPoint, leaseUntil time.Time) error {
	affected, err := r.q(ctx).AdvanceIdempotencyKey(ctx, sqlc.AdvanceIdempotencyKeyParams{
		ID:            recordID,
		RecoveryPoint: string(point),
		LockExpiresAt: timestamptz(leaseUntil),
	})
	if err != nil {
		return fmt.Errorf("postgres: advance idempotency key: %w", err)
	}
	if affected == 0 {
		return idempotency.ErrKeyNotFound
	}
	return nil
}

// TakeOver claims a record whose holder crashed. The lease comparison is in the WHERE
// clause, so of several retries arriving together exactly one wins.
func (r *IdempotencyRepo) TakeOver(ctx context.Context, recordID string, leaseUntil time.Time) (bool, error) {
	affected, err := r.q(ctx).TakeOverIdempotencyKey(ctx, sqlc.TakeOverIdempotencyKeyParams{
		ID:            recordID,
		LockExpiresAt: timestamptz(leaseUntil),
	})
	if err != nil {
		return false, fmt.Errorf("postgres: take over idempotency key: %w", err)
	}
	return affected > 0, nil
}

// Release drops an in-progress record so a retry after a server error re-executes.
func (r *IdempotencyRepo) Release(ctx context.Context, recordID string) error {
	if _, err := r.q(ctx).ReleaseIdempotencyKey(ctx, recordID); err != nil {
		return fmt.Errorf("postgres: release idempotency key: %w", err)
	}
	return nil
}

func (r *IdempotencyRepo) DeleteExpired(ctx context.Context, before time.Time, limit int) (int64, error) {
	n, err := r.q(ctx).DeleteExpiredIdempotencyKeys(ctx, sqlc.DeleteExpiredIdempotencyKeysParams{
		ExpiresAt: timestamptz(before),
		Limit:     int32(limit),
	})
	if err != nil {
		return 0, fmt.Errorf("postgres: delete expired idempotency keys: %w", err)
	}
	return n, nil
}

func recordFromRow(row sqlc.IdempotencyKey) *idempotency.Record {
	rec := &idempotency.Record{
		ID:            row.ID,
		MerchantID:    row.MerchantID,
		Key:           row.IdempotencyKey,
		Method:        row.RequestMethod,
		Path:          row.RequestPath,
		RequestHash:   row.RequestHash,
		State:         idempotency.State(row.State),
		RecoveryPoint: idempotency.RecoveryPoint(row.RecoveryPoint),
		ResponseBody:  row.ResponseBody,
		ResourceID:    derefString(row.ResourceID),
		RequestID:     derefString(row.RequestID),
		CreatedAt:     row.CreatedAt.Time,
		ExpiresAt:     row.ExpiresAt.Time,
	}
	if row.LockExpiresAt.Valid {
		at := row.LockExpiresAt.Time
		rec.LockExpiresAt = &at
	}
	if row.ResponseStatus != nil {
		rec.ResponseStatus = int(*row.ResponseStatus)
	}
	return rec
}

func int32Ptr(v int) *int32 {
	n := int32(v)
	return &n
}
