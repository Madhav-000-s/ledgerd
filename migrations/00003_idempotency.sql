-- +goose Up

CREATE TYPE idem_state AS ENUM ('in_progress', 'succeeded', 'failed');

CREATE TABLE idempotency_keys (
    id              TEXT PRIMARY KEY,            -- "idem_01HQ..."
    merchant_id     TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_method  TEXT NOT NULL,
    request_path    TEXT NOT NULL,
    -- sha256(method || path || canonical body). Canonicalization is what stops a client
    -- that reserializes its map in a different key order from getting a spurious
    -- mismatch on a legitimate retry.
    request_hash    BYTEA NOT NULL,
    state           idem_state NOT NULL,
    -- Which atomic phase the operation last completed. The seam for external side
    -- effects: a crash resumes here rather than re-running work that already committed.
    recovery_point  TEXT NOT NULL DEFAULT 'started',
    -- NULL once the record reaches a terminal state. A lease that has passed means the
    -- holder crashed and the key may be taken over.
    lock_expires_at TIMESTAMPTZ,
    response_status INT,
    response_body   JSONB,
    resource_id     TEXT,
    request_id      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '24 hours',

    -- Scoping the key by merchant is what makes this safe multi-tenant: one merchant can
    -- neither collide with nor probe another's keys.
    UNIQUE (merchant_id, idempotency_key)
);

CREATE INDEX idempotency_keys_expiry_idx ON idempotency_keys (expires_at);

-- +goose Down

DROP TABLE IF EXISTS idempotency_keys;
DROP TYPE IF EXISTS idem_state;
