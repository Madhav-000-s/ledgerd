-- name: CreateAccount :exec
INSERT INTO accounts (
    id, merchant_id, name, type, normal_balance, currency, allow_negative, metadata, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
);

-- name: GetAccount :one
SELECT id, merchant_id, name, type, normal_balance, currency, allow_negative, metadata, created_at
  FROM accounts
 WHERE id = $1;

-- name: ListAccounts :many
SELECT id, merchant_id, name, type, normal_balance, currency, allow_negative, metadata, created_at
  FROM accounts
 WHERE merchant_id = $1
 ORDER BY id;

-- LockAccounts is the first step of the critical section.
--
-- ORDER BY before FOR UPDATE is the whole trick: it acquires the row locks in a total
-- order, which makes deadlock structurally impossible rather than something to recover
-- from by retrying. The caller sorts the ids it passes in as well, so the ordering is
-- stated twice — once where a reader can see the intent, once where the database can
-- act on it.
--
-- FOR UPDATE OF b locks only the balance rows. Locking the account rows too would
-- serialize unrelated writers against metadata that does not change during a movement.
--
-- name: LockAccounts :many
SELECT a.id, a.merchant_id, a.name, a.type, a.normal_balance, a.currency,
       a.allow_negative, a.metadata, a.created_at,
       b.posted_minor, b.pending_minor, b.version, b.updated_at
  FROM account_balances b
  JOIN accounts a ON a.id = b.account_id
 WHERE b.account_id = ANY(@account_ids::text[])
 ORDER BY b.account_id
   FOR UPDATE OF b;

-- ApplyBalance writes a new materialized balance.
--
-- The version read under the lock goes into the WHERE clause. The row lock should
-- already make a lost update impossible; this turns "should" into zero rows affected if
-- it ever is not, which is far cheaper to diagnose than a reconciler alarm.
--
-- name: ApplyBalance :execrows
UPDATE account_balances
   SET posted_minor  = $2,
       pending_minor = $3,
       version       = version + 1,
       updated_at    = now()
 WHERE account_id = $1
   AND version    = $4;

-- name: GetBalance :one
SELECT b.account_id, a.currency, b.posted_minor, b.pending_minor, b.version, b.updated_at
  FROM account_balances b
  JOIN accounts a ON a.id = b.account_id
 WHERE b.account_id = $1;

-- TrialBalance restates every posted balance in debit-positive terms and sums it. The
-- reconciler asserts this is zero (I8); a non-zero result is a P0 page, not a metric to
-- be smoothed over.
--
-- name: TrialBalance :one
SELECT COALESCE(SUM(
           CASE WHEN a.type IN ('asset', 'expense') THEN b.posted_minor
                ELSE -b.posted_minor
           END
       ), 0)::bigint AS drift
  FROM account_balances b
  JOIN accounts a ON a.id = b.account_id
 WHERE a.currency = $1;

-- DerivedBalances recomputes every account's posted balance from the entry log, which
-- is the source of truth, and reports any account whose materialized balance disagrees
-- (I7).
--
-- name: DerivedBalances :many
SELECT a.id AS account_id,
       b.posted_minor AS materialized,
       COALESCE(SUM(
           CASE WHEN e.direction::text = a.normal_balance::text THEN e.amount_minor
                ELSE -e.amount_minor
           END
       ) FILTER (WHERE e.status = 'posted'), 0)::bigint AS derived
  FROM accounts a
  JOIN account_balances b ON b.account_id = a.id
  LEFT JOIN entries e ON e.account_id = a.id
 GROUP BY a.id, b.posted_minor;
