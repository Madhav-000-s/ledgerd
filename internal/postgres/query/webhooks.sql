-- ── the outbox ───────────────────────────────────────────────────────────────

-- name: AppendEvent :exec
INSERT INTO events (id, merchant_id, type, resource_id, payload, created_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetEvent :one
SELECT id, merchant_id, type, resource_id, payload, dispatched_at, created_at
  FROM events
 WHERE id = $1;

-- name: GetEventForMerchant :one
SELECT id, merchant_id, type, resource_id, payload, dispatched_at, created_at
  FROM events
 WHERE merchant_id = $1 AND id = $2;

-- name: ListEvents :many
SELECT id, merchant_id, type, resource_id, payload, dispatched_at, created_at
  FROM events
 WHERE merchant_id = $1
   AND (sqlc.narg('before_id')::text IS NULL OR id < sqlc.narg('before_id')::text)
 ORDER BY id DESC
 LIMIT $2;

-- ClaimUndispatched takes a batch of events that have not been fanned out.
--
-- SKIP LOCKED is what lets several dispatchers run at once with no coordination: each
-- takes rows the others are not holding, so an event is never expanded twice
-- concurrently. The unique constraint on (event_id, endpoint_id) covers the case where
-- one of them crashed after inserting some deliveries.
--
-- name: ClaimUndispatchedEvents :many
SELECT id, merchant_id, type, resource_id, payload, dispatched_at, created_at
  FROM events
 WHERE dispatched_at IS NULL
 ORDER BY id
 LIMIT $1
   FOR UPDATE SKIP LOCKED;

-- name: MarkEventsDispatched :execrows
UPDATE events SET dispatched_at = now() WHERE id = ANY($1::text[]);

-- ── endpoints ────────────────────────────────────────────────────────────────

-- name: CreateWebhookEndpoint :exec
INSERT INTO webhook_endpoints (id, merchant_id, url, enabled_events, description, created_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetWebhookEndpoint :one
SELECT id, merchant_id, url, enabled_events, status, consecutive_failures,
       description, created_at, disabled_at
  FROM webhook_endpoints
 WHERE merchant_id = $1 AND id = $2;

-- name: GetWebhookEndpointByID :one
SELECT id, merchant_id, url, enabled_events, status, consecutive_failures,
       description, created_at, disabled_at
  FROM webhook_endpoints
 WHERE id = $1;

-- name: ListWebhookEndpoints :many
SELECT id, merchant_id, url, enabled_events, status, consecutive_failures,
       description, created_at, disabled_at
  FROM webhook_endpoints
 WHERE merchant_id = $1
 ORDER BY created_at DESC;

-- ActiveWebhookEndpoints returns the endpoints an event may fan out to. A disabled
-- endpoint is excluded here rather than filtered later, so a disabled endpoint never
-- gets a delivery row created for it at all.
--
-- name: ActiveWebhookEndpoints :many
SELECT id, merchant_id, url, enabled_events, status, consecutive_failures,
       description, created_at, disabled_at
  FROM webhook_endpoints
 WHERE merchant_id = $1 AND status <> 'disabled'
 ORDER BY id;

-- Both uses of the status parameter are cast to endpoint_status explicitly. Without the
-- casts Postgres deduces text in the CASE and the enum in the assignment, decides the
-- two are inconsistent, and refuses the whole statement — which showed up as an endpoint
-- that answered 410 and was never actually disabled.
--
-- name: SetWebhookEndpointStatus :execrows
UPDATE webhook_endpoints
   SET status = sqlc.arg('status')::endpoint_status,
       disabled_at = CASE
           WHEN sqlc.arg('status')::endpoint_status = 'disabled'::endpoint_status THEN now()
           ELSE disabled_at
       END
 WHERE id = sqlc.arg('id');

-- name: IncrementEndpointFailures :one
UPDATE webhook_endpoints
   SET consecutive_failures = consecutive_failures + 1
 WHERE id = $1
RETURNING consecutive_failures;

-- name: ResetEndpointFailures :execrows
UPDATE webhook_endpoints SET consecutive_failures = 0 WHERE id = $1;

-- ── secrets ──────────────────────────────────────────────────────────────────

-- name: CreateWebhookSecret :exec
INSERT INTO webhook_secrets (id, endpoint_id, secret_hash, secret_enc, active, created_at)
VALUES ($1, $2, $3, $4, TRUE, $5);

-- name: ActiveWebhookSecrets :many
SELECT id, endpoint_id, secret_hash, secret_enc, active, created_at, retired_at
  FROM webhook_secrets
 WHERE endpoint_id = $1 AND active
 ORDER BY created_at DESC;

-- name: RetireWebhookSecret :execrows
UPDATE webhook_secrets SET active = FALSE, retired_at = now() WHERE id = $1;

-- ── the delivery queue ───────────────────────────────────────────────────────

-- EnqueueDelivery is idempotent on (event_id, endpoint_id), so re-running a dispatch
-- that crashed part way through cannot produce a second delivery to the same endpoint.
--
-- The due time comes from now() rather than a parameter, because it is compared against
-- now() when the row is claimed. Passing the application's clock instead couples the two:
-- a deployment whose app clock runs ahead of the database's would delay every first
-- attempt by the skew, and the queue would look mysteriously slow.
--
-- name: EnqueueDelivery :exec
INSERT INTO webhook_deliveries (id, event_id, endpoint_id, merchant_id, state, next_attempt_at, created_at)
VALUES ($1, $2, $3, $4, 'pending', now(), now())
ON CONFLICT (event_id, endpoint_id) DO NOTHING;

-- ClaimDueDeliveries is the canonical Postgres work queue.
--
-- FOR UPDATE SKIP LOCKED makes competing consumers correct without any coordination:
-- each worker takes rows the others are not holding. Adding workers adds throughput
-- rather than contention, which is the entire reason Postgres is the queue here and no
-- broker is involved.
--
-- The lease is set in the same statement as the claim, so a worker that dies immediately
-- after claiming still leaves a row that the sweeper can reclaim.
--
-- name: ClaimDueDeliveries :many
UPDATE webhook_deliveries
   SET state = 'delivering',
       leased_until = $2
 WHERE id IN (
     SELECT id FROM webhook_deliveries
      WHERE state = 'pending'
        AND next_attempt_at <= now()
      ORDER BY next_attempt_at
      LIMIT $1
        FOR UPDATE SKIP LOCKED
 )
RETURNING id, event_id, endpoint_id, merchant_id, state, attempt, next_attempt_at,
          leased_until, last_status, last_error, last_duration_ms, created_at, completed_at;

-- name: CompleteDelivery :execrows
UPDATE webhook_deliveries
   SET state = sqlc.arg('state'),
       attempt = attempt + 1,
       leased_until = NULL,
       last_status = sqlc.narg('last_status'),
       last_error = sqlc.narg('last_error'),
       last_duration_ms = sqlc.narg('last_duration_ms'),
       completed_at = now()
 WHERE id = sqlc.arg('id');

-- The backoff arrives as a delay rather than an absolute instant, for the same reason:
-- the schedule is relative to the database's clock, which is the one the claim reads.
--
-- name: RescheduleDelivery :execrows
UPDATE webhook_deliveries
   SET state = 'pending',
       attempt = attempt + 1,
       next_attempt_at = now() + make_interval(secs => sqlc.arg('delay_seconds')::float8),
       leased_until = NULL,
       last_status = sqlc.narg('last_status'),
       last_error = sqlc.narg('last_error'),
       last_duration_ms = sqlc.narg('last_duration_ms')
 WHERE id = sqlc.arg('id');

-- ReclaimExpiredLeases is the crash-recovery path: a worker that died mid-POST left a
-- row marked delivering with a lease that has now passed.
--
-- This is precisely why delivery is at-least-once. The worker may have died after the
-- endpoint accepted the request but before recording it, so the event will be sent
-- again — which is why consumers are told to dedupe on the event id.
--
-- name: ReclaimExpiredLeases :execrows
UPDATE webhook_deliveries
   SET state = 'pending',
       leased_until = NULL
 WHERE state = 'delivering'
   AND leased_until IS NOT NULL
   AND leased_until < $1;

-- name: RecordDeliveryAttempt :exec
INSERT INTO webhook_attempts (delivery_id, attempt, status_code, error, duration_ms, attempted_at)
VALUES (sqlc.arg('delivery_id'), sqlc.arg('attempt'), sqlc.narg('status_code'),
        sqlc.narg('error'), sqlc.narg('duration_ms'), sqlc.arg('attempted_at'));

-- name: ListDeliveries :many
SELECT id, event_id, endpoint_id, merchant_id, state, attempt, next_attempt_at,
       leased_until, last_status, last_error, last_duration_ms, created_at, completed_at
  FROM webhook_deliveries
 WHERE endpoint_id = $1
   AND (sqlc.narg('state')::delivery_state IS NULL OR state = sqlc.narg('state')::delivery_state)
 ORDER BY created_at DESC
 LIMIT $2;

-- ReplayDelivery moves an exhausted delivery back into the queue.
--
-- The state check is in the WHERE clause rather than in the caller, so replaying a
-- delivery that is already pending cannot reset its attempt counter and hand it a fresh
-- retry budget it has not earned.
--
-- name: ReplayDelivery :execrows
UPDATE webhook_deliveries
   SET state = 'pending',
       attempt = 0,
       next_attempt_at = now(),
       leased_until = NULL,
       completed_at = NULL
 WHERE id = $1
   AND merchant_id = $2
   AND state = 'exhausted';

-- name: CountDeliveriesByState :many
SELECT state, count(*)::bigint AS total
  FROM webhook_deliveries
 GROUP BY state;

-- OldestPendingDelivery backs the real service-level indicator: not how many deliveries
-- are queued, but how long the oldest one has been waiting.
--
-- name: OldestPendingDelivery :one
SELECT COALESCE(EXTRACT(EPOCH FROM (now() - MIN(next_attempt_at))), 0)::bigint AS oldest_seconds
  FROM webhook_deliveries
 WHERE state IN ('pending', 'delivering');
