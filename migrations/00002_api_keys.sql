-- +goose Up

CREATE TYPE api_key_role AS ENUM ('read', 'write', 'admin');

CREATE TABLE api_keys (
    id          TEXT PRIMARY KEY,               -- "ak_01HQ..."
    merchant_id TEXT NOT NULL,
    name        TEXT NOT NULL DEFAULT '',
    -- The first characters of the secret, stored in the clear. It is not secret on its
    -- own, and it turns verification into one index seek plus one hash rather than
    -- hashing every key in the table to find a match. Without it, argon2id — which is
    -- deliberately slow — would have to run once per stored key on every request.
    lookup      TEXT NOT NULL,
    -- argon2id of the full secret. The secret itself is shown exactly once, at creation,
    -- and is not recoverable from here.
    secret_hash BYTEA NOT NULL,
    role        api_key_role NOT NULL DEFAULT 'write',
    livemode    BOOLEAN NOT NULL DEFAULT FALSE,
    last_used_at TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (lookup)
);

CREATE INDEX api_keys_merchant_idx ON api_keys (merchant_id) WHERE revoked_at IS NULL;

-- +goose Down

DROP TABLE IF EXISTS api_keys;
DROP TYPE IF EXISTS api_key_role;
