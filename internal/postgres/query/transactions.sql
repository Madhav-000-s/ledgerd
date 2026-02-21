-- name: InsertTransaction :exec
INSERT INTO transactions (
    id, merchant_id, status, currency, description, reverses_id, external_ref,
    metadata, created_at, posted_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
);

-- name: SetTransactionStatus :execrows
UPDATE transactions
   SET status    = $2,
       posted_at = COALESCE(sqlc.narg('posted_at'), posted_at)
 WHERE id = $1;

-- name: GetTransaction :one
SELECT id, merchant_id, status, currency, description, reverses_id, external_ref,
       metadata, created_at, posted_at
  FROM transactions
 WHERE id = $1;

-- FindReversal returns the mirror of a transaction, if one has been posted. The unique
-- partial index on reverses_id is what actually prevents a double refund; this lookup
-- turns the common case into a precise error rather than a constraint violation.
--
-- name: FindReversal :one
SELECT id, merchant_id, status, currency, description, reverses_id, external_ref,
       metadata, created_at, posted_at
  FROM transactions
 WHERE reverses_id = $1;

-- ListTransactions pages a merchant's transactions newest-first over the
-- (merchant_id, created_at DESC, id DESC) index.
--
-- name: ListTransactions :many
SELECT id, merchant_id, status, currency, description, reverses_id, external_ref,
       metadata, created_at, posted_at
  FROM transactions
 WHERE merchant_id = $1
   AND (sqlc.narg('external_ref')::text IS NULL OR external_ref = sqlc.narg('external_ref')::text)
   AND (sqlc.narg('created_before')::timestamptz IS NULL OR created_at < sqlc.narg('created_before')::timestamptz)
 ORDER BY created_at DESC, id DESC
 LIMIT $2;

-- ExpirePendingTransactions finds authorizations older than the TTL that were never
-- captured or voided, so the sweep can release their holds.
--
-- name: ExpirePendingTransactions :many
SELECT id, merchant_id, status, currency, description, reverses_id, external_ref,
       metadata, created_at, posted_at
  FROM transactions
 WHERE status = 'pending'
   AND created_at < $1
 ORDER BY created_at
 LIMIT $2;

-- TransactionsByRef returns every ledger transaction belonging to one external object —
-- a payment or a refund. A payment's public state is derived from these rather than
-- stored in a mutable column, so it cannot disagree with the books.
--
-- name: TransactionsByRef :many
SELECT id, merchant_id, status, currency, description, reverses_id, external_ref,
       metadata, created_at, posted_at
  FROM transactions
 WHERE merchant_id = $1
   AND external_ref = $2
 ORDER BY created_at, id;
