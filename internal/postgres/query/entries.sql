-- AppendEntry inserts one posting.
--
-- Declared as a batch so that every entry of a transaction goes out in a single
-- round trip via pgx.SendBatch, rather than one network hop per side of the double
-- entry.
--
-- The balance trigger is FOR EACH ROW and DEFERRABLE INITIALLY DEFERRED, so it queues a
-- check per row and evaluates them all at COMMIT regardless of how the rows arrive. The
-- batching is a latency decision, not a correctness one.
--
-- seq comes back with the generated id because the caller has to match each id to the
-- entry it belongs to.
--
-- There is deliberately no UpdateEntry or DeleteEntry anywhere in this file. The
-- database enforces that as well, with a rule, so the guarantee survives a manual psql
-- session as readily as a future contributor.
--
-- name: AppendEntry :batchone
INSERT INTO entries (
    transaction_id, account_id, direction, amount_minor, currency, status, seq,
    balance_after, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
RETURNING id, seq, created_at;

-- ListEntries pages an account's ledger view newest-first over the
-- (account_id, id DESC) index.
--
-- Entry ids are a BIGSERIAL, so descending id is descending time and the keyset is one
-- comparison. OFFSET is never used: it degrades linearly with depth and, worse, silently
-- skips or duplicates rows when concurrent inserts land between pages.
--
-- name: ListEntries :many
SELECT id, transaction_id, account_id, direction, amount_minor, currency, status, seq,
       balance_after, created_at
  FROM entries
 WHERE account_id = $1
   AND (sqlc.narg('after_id')::bigint IS NULL OR id < sqlc.narg('after_id')::bigint)
 ORDER BY id DESC
 LIMIT $2;

-- name: EntriesByTransaction :many
SELECT id, transaction_id, account_id, direction, amount_minor, currency, status, seq,
       balance_after, created_at
  FROM entries
 WHERE transaction_id = $1
 ORDER BY seq;

-- EntriesForAccountAsc replays an account's entries in write order. The reconciler uses
-- it to bisect to the exact entry where the derived and materialized balances first
-- disagreed, which is what balance_after is for.
--
-- name: EntriesForAccountAsc :many
SELECT id, transaction_id, account_id, direction, amount_minor, currency, status, seq,
       balance_after, created_at
  FROM entries
 WHERE account_id = $1
   AND id > $2
 ORDER BY id
 LIMIT $3;
