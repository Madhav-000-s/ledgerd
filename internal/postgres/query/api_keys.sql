-- FindAPIKeyByLookup resolves the non-secret lookup prefix to a stored key.
--
-- The prefix exists so verification is one index seek plus one hash. Without it,
-- argon2id — which is deliberately slow — would have to run once per stored key on every
-- single request just to find which one matched.
--
-- name: FindAPIKeyByLookup :one
SELECT id, merchant_id, name, lookup, secret_hash, role, livemode,
       last_used_at, revoked_at, created_at
  FROM api_keys
 WHERE lookup = $1;

-- name: CreateAPIKey :exec
INSERT INTO api_keys (id, merchant_id, name, lookup, secret_hash, role, livemode, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, now());

-- name: RevokeAPIKey :execrows
UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: ListAPIKeys :many
SELECT id, merchant_id, name, lookup, secret_hash, role, livemode,
       last_used_at, revoked_at, created_at
  FROM api_keys
 WHERE merchant_id = $1
 ORDER BY created_at DESC;
