-- AcquireIdempotencyKey claims a key, or returns the row that already holds it.
--
-- The insert-or-read has to be one statement. Checking and then inserting has a race in
-- which two concurrent retries both believe they own the key, which is precisely the
-- double-charge this table exists to prevent. ON CONFLICT DO NOTHING gives the loser an
-- empty result, and the caller then reads the winner's row.
--
-- name: AcquireIdempotencyKey :one
INSERT INTO idempotency_keys (
    id, merchant_id, idempotency_key, request_method, request_path, request_hash,
    state, recovery_point, lock_expires_at, request_id, created_at, expires_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
)
ON CONFLICT (merchant_id, idempotency_key) DO NOTHING
RETURNING id, merchant_id, idempotency_key, request_method, request_path, request_hash,
          state, recovery_point, lock_expires_at, response_status, response_body,
          resource_id, request_id, created_at, expires_at;

-- name: GetIdempotencyKey :one
SELECT id, merchant_id, idempotency_key, request_method, request_path, request_hash,
       state, recovery_point, lock_expires_at, response_status, response_body,
       resource_id, request_id, created_at, expires_at
  FROM idempotency_keys
 WHERE merchant_id = $1 AND idempotency_key = $2;

-- CompleteIdempotencyKey records the terminal outcome and the response to replay.
--
-- Called inside the same transaction as the ledger write, so the entries and the
-- response commit together. The lock is dropped in the same statement: a terminal record
-- has no holder.
--
-- name: CompleteIdempotencyKey :execrows
UPDATE idempotency_keys
   SET state           = $2,
       recovery_point  = 'finished',
       response_status = $3,
       response_body   = $4,
       resource_id     = COALESCE(sqlc.narg('resource_id'), resource_id),
       lock_expires_at = NULL
 WHERE id = $1;

-- name: AdvanceIdempotencyKey :execrows
UPDATE idempotency_keys
   SET recovery_point  = $2,
       lock_expires_at = $3
 WHERE id = $1
   AND state = 'in_progress';

-- TakeOverIdempotencyKey claims a record whose holder crashed.
--
-- The lease comparison is in the WHERE clause, so this is a compare-and-set: of several
-- retries arriving at once after a crash, exactly one wins and the rest are told to try
-- again shortly.
--
-- name: TakeOverIdempotencyKey :execrows
UPDATE idempotency_keys
   SET lock_expires_at = $2
 WHERE id = $1
   AND state = 'in_progress'
   AND (lock_expires_at IS NULL OR lock_expires_at <= now());

-- ReleaseIdempotencyKey drops the lock without recording a response, so a retry after a
-- server error genuinely re-executes rather than replaying a 500 forever.
--
-- name: ReleaseIdempotencyKey :execrows
DELETE FROM idempotency_keys
 WHERE id = $1
   AND state = 'in_progress';

-- DeleteExpiredIdempotencyKeys is the reaper's batch. Bounded so a single sweep cannot
-- lock the table for an unbounded time.
--
-- name: DeleteExpiredIdempotencyKeys :execrows
DELETE FROM idempotency_keys k
 WHERE k.id IN (
     SELECT doomed.id FROM idempotency_keys doomed
      WHERE doomed.expires_at < $1
      ORDER BY doomed.expires_at
      LIMIT $2
 );
