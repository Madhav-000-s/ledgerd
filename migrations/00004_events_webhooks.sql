-- +goose Up

-- ── the transactional outbox ─────────────────────────────────────────────────

-- An event row is inserted in the same database transaction as the ledger entries that
-- produced it. This eliminates the dual-write problem outright: it is impossible to move
-- money without producing an event, and impossible to produce an event for money that
-- did not move.
--
-- Any design that publishes to a broker from application code after committing has a
-- window in which those two facts disagree, and in payments that window is a
-- reconciliation incident rather than a missed notification.
CREATE TABLE events (
    id            TEXT PRIMARY KEY,              -- "evt_01HQ..."
    merchant_id   TEXT NOT NULL,
    type          TEXT NOT NULL,                 -- 'payment.succeeded'
    resource_id   TEXT NOT NULL,
    payload       JSONB NOT NULL,
    dispatched_at TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Partial index: the dispatcher only ever looks for undispatched rows, and this keeps
-- that scan proportional to the backlog rather than to the history.
CREATE INDEX events_undispatched_idx ON events (dispatched_at, id) WHERE dispatched_at IS NULL;
CREATE INDEX events_merchant_idx ON events (merchant_id, created_at DESC, id DESC);

-- Wake the dispatcher on commit so latency is sub-second rather than one poll interval.
-- NOTIFY is not durable, so the dispatcher also polls; this is an optimization, never the
-- mechanism.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION notify_event() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('ledgerd_events', NEW.id);
    RETURN NULL;
END; $$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER events_notify
    AFTER INSERT ON events
    FOR EACH ROW EXECUTE FUNCTION notify_event();

-- ── endpoints ────────────────────────────────────────────────────────────────

CREATE TYPE delivery_state AS ENUM ('pending', 'delivering', 'succeeded', 'exhausted', 'disabled');
CREATE TYPE endpoint_status AS ENUM ('active', 'degraded', 'disabled');

CREATE TABLE webhook_endpoints (
    id                   TEXT PRIMARY KEY,       -- "we_01HQ..."
    merchant_id          TEXT NOT NULL,
    url                  TEXT NOT NULL,
    enabled_events       TEXT[] NOT NULL,        -- ['payment.*'], globs supported
    status               endpoint_status NOT NULL DEFAULT 'active',
    consecutive_failures INT NOT NULL DEFAULT 0,
    description          TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at          TIMESTAMPTZ
);

CREATE INDEX webhook_endpoints_merchant_idx ON webhook_endpoints (merchant_id)
    WHERE status <> 'disabled';

-- Several secrets may be active at once, which is what makes rotation zero-downtime: a
-- payload is signed with every active secret, so a consumer can move across at its own
-- pace instead of during a synchronized cutover.
CREATE TABLE webhook_secrets (
    id          TEXT PRIMARY KEY,                -- "whsec_01HQ..."
    endpoint_id TEXT NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,
    -- The secret is shown exactly once, at creation.
    secret_hash BYTEA NOT NULL,
    -- AES-GCM ciphertext, so the worker can sign with it. The key comes from the
    -- environment in the MVP and from a KMS if this ever runs anywhere real.
    secret_enc  BYTEA NOT NULL,
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at  TIMESTAMPTZ
);

CREATE INDEX webhook_secrets_endpoint_idx ON webhook_secrets (endpoint_id) WHERE active;

-- ── the delivery queue ───────────────────────────────────────────────────────

CREATE TABLE webhook_deliveries (
    id               TEXT PRIMARY KEY,           -- "whd_01HQ..."
    event_id         TEXT NOT NULL REFERENCES events(id),
    endpoint_id      TEXT NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,
    merchant_id      TEXT NOT NULL,
    state            delivery_state NOT NULL DEFAULT 'pending',
    attempt          INT NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Set while a worker holds the row. A lease older than the limit means the worker
    -- died mid-POST and the row may be reclaimed.
    leased_until     TIMESTAMPTZ,
    last_status      INT,
    last_error       TEXT,
    last_duration_ms INT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ,

    -- Fan-out is idempotent: re-running the dispatcher for an event that was partially
    -- expanded cannot produce a second delivery to the same endpoint.
    UNIQUE (event_id, endpoint_id)
);

-- The claim query's index. Partial, because a worker only ever looks for work that is
-- due, and the succeeded rows vastly outnumber it over time.
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at)
    WHERE state IN ('pending', 'delivering');

CREATE INDEX webhook_deliveries_endpoint_idx ON webhook_deliveries (endpoint_id, created_at DESC);
CREATE INDEX webhook_deliveries_dlq_idx ON webhook_deliveries (merchant_id, completed_at DESC)
    WHERE state = 'exhausted';

-- Append-only audit of every attempt, so "why did this endpoint stop receiving events"
-- has an answer that does not depend on log retention.
CREATE TABLE webhook_attempts (
    id           BIGSERIAL PRIMARY KEY,
    delivery_id  TEXT NOT NULL REFERENCES webhook_deliveries(id) ON DELETE CASCADE,
    attempt      INT NOT NULL,
    status_code  INT,
    error        TEXT,
    duration_ms  INT,
    attempted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX webhook_attempts_delivery_idx ON webhook_attempts (delivery_id, attempt);

-- +goose Down

DROP TABLE IF EXISTS webhook_attempts;
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS webhook_secrets;
DROP TABLE IF EXISTS webhook_endpoints;

DROP TYPE IF EXISTS endpoint_status;
DROP TYPE IF EXISTS delivery_state;

DROP TRIGGER IF EXISTS events_notify ON events;
DROP FUNCTION IF EXISTS notify_event();
DROP TABLE IF EXISTS events;
