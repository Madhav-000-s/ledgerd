package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Madhav-000-s/ledgerd/internal/httpapi"
	"github.com/Madhav-000-s/ledgerd/internal/postgres/sqlc"
)

// APIKeyRepo implements httpapi.APIKeyStore.
type APIKeyRepo struct {
	db *DB
}

var _ httpapi.APIKeyStore = (*APIKeyRepo)(nil)

// NewAPIKeyRepo returns a store backed by db.
func NewAPIKeyRepo(db *DB) *APIKeyRepo { return &APIKeyRepo{db: db} }

func (r *APIKeyRepo) q(ctx context.Context) *sqlc.Queries {
	return sqlc.New(r.db.q(ctx))
}

// FindByLookup resolves the non-secret lookup prefix to a stored key.
//
// A revoked key is returned rather than hidden, so the caller can apply the same
// constant-cost verification path to it. Skipping the hash for revoked keys would make
// revocation observable by timing.
func (r *APIKeyRepo) FindByLookup(ctx context.Context, lookup string) (*httpapi.APIKeyRecord, error) {
	row, err := r.q(ctx).FindAPIKeyByLookup(ctx, lookup)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpapi.ErrAPIKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: find api key: %w", err)
	}

	return &httpapi.APIKeyRecord{
		ID:         row.ID,
		MerchantID: row.MerchantID,
		Lookup:     row.Lookup,
		SecretHash: row.SecretHash,
		Role:       httpapi.Role(row.Role),
		Livemode:   row.Livemode,
		Revoked:    row.RevokedAt.Valid,
	}, nil
}

// Create stores a new key. The secret itself is never passed here — only its hash, which
// the caller produced with httpapi.GenerateAPIKey.
func (r *APIKeyRepo) Create(ctx context.Context, rec *httpapi.APIKeyRecord, name string) error {
	err := r.q(ctx).CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{
		ID:         rec.ID,
		MerchantID: rec.MerchantID,
		Name:       name,
		Lookup:     rec.Lookup,
		SecretHash: rec.SecretHash,
		Role:       sqlc.ApiKeyRole(rec.Role),
		Livemode:   rec.Livemode,
	})
	if err != nil {
		return fmt.Errorf("postgres: create api key: %w", err)
	}
	return nil
}

// Revoke marks a key unusable. Keys are never deleted: an audit trail that cannot say
// which key authorized a movement is not much of an audit trail.
func (r *APIKeyRepo) Revoke(ctx context.Context, keyID string) error {
	affected, err := r.q(ctx).RevokeAPIKey(ctx, keyID)
	if err != nil {
		return fmt.Errorf("postgres: revoke api key: %w", err)
	}
	if affected == 0 {
		return httpapi.ErrAPIKeyNotFound
	}
	return nil
}
