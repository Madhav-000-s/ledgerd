-- +goose Up

-- ── enums ────────────────────────────────────────────────────────────────────

CREATE TYPE account_type AS ENUM ('asset', 'liability', 'equity', 'revenue', 'expense');
CREATE TYPE balance_side AS ENUM ('debit', 'credit');
CREATE TYPE entry_status AS ENUM ('pending', 'posted', 'voided');
CREATE TYPE txn_status   AS ENUM ('pending', 'posted', 'voided');

-- ── accounts ─────────────────────────────────────────────────────────────────

CREATE TABLE accounts (
    id             TEXT PRIMARY KEY,              -- "acct_01HQ..." ULID
    merchant_id    TEXT NOT NULL,
    name           TEXT NOT NULL,
    type           account_type NOT NULL,
    normal_balance balance_side NOT NULL,
    currency       CHAR(3) NOT NULL,
    allow_negative BOOLEAN NOT NULL DEFAULT FALSE,
    -- Reserved for balance sharding: a hot logical account fans out to N physical
    -- sub-accounts. Unused in the MVP, but adding the column now means enabling it
    -- later is not a migration against a large table. See DESIGN.md Q3.
    shard_of       TEXT REFERENCES accounts(id),
    metadata       JSONB NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- I5. The normal balance is a function of the type, so a row where the two
    -- disagree must not be representable.
    CONSTRAINT normal_matches_type CHECK (
        (type IN ('asset', 'expense')              AND normal_balance = 'debit') OR
        (type IN ('liability', 'equity', 'revenue') AND normal_balance = 'credit')
    ),
    UNIQUE (merchant_id, name)
);

CREATE INDEX accounts_merchant_idx ON accounts (merchant_id, id);

-- ── transactions (journal entries) ───────────────────────────────────────────

CREATE TABLE transactions (
    id           TEXT PRIMARY KEY,                -- "txn_01HQ..."
    merchant_id  TEXT NOT NULL,
    status       txn_status NOT NULL,
    currency     CHAR(3) NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    reverses_id  TEXT REFERENCES transactions(id),
    external_ref TEXT,                            -- payment or refund id
    metadata     JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_at    TIMESTAMPTZ
);

CREATE INDEX transactions_merchant_idx ON transactions (merchant_id, created_at DESC, id DESC);
CREATE INDEX transactions_external_ref_idx ON transactions (external_ref) WHERE external_ref IS NOT NULL;

-- A transaction can be reversed at most once. Without this, two concurrent refunds
-- both pass their application-level check and both post.
CREATE UNIQUE INDEX transactions_reverses_idx ON transactions (reverses_id) WHERE reverses_id IS NOT NULL;

-- ── entries (postings) — APPEND ONLY ─────────────────────────────────────────

CREATE TABLE entries (
    id             BIGSERIAL PRIMARY KEY,
    transaction_id TEXT NOT NULL REFERENCES transactions(id),
    account_id     TEXT NOT NULL REFERENCES accounts(id),
    direction      balance_side NOT NULL,
    -- I3. The sign of a posting lives in its direction, never in its amount, so the
    -- same movement cannot be expressed two ways with only one of them balancing.
    amount_minor   BIGINT NOT NULL CHECK (amount_minor > 0),
    currency       CHAR(3) NOT NULL,
    status         entry_status NOT NULL,
    seq            SMALLINT NOT NULL,             -- ordinal within the transaction
    -- The account's posted balance immediately after this entry. Cheap to write and
    -- enormously valuable: it turns "reconstruct the balance as of T" from a full scan
    -- into an index seek, and it lets the reconciler binary-search to the exact entry
    -- where derived and materialized diverged.
    balance_after  BIGINT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (transaction_id, seq)
);

CREATE INDEX entries_account_idx ON entries (account_id, id DESC);
CREATE INDEX entries_transaction_idx ON entries (transaction_id);

-- I4. Entries are immutable. A mistake is corrected by posting a compensating
-- transaction, which is what keeps history a fact rather than a mutable projection.
--
-- DO INSTEAD NOTHING makes an UPDATE or DELETE a silent no-op rather than an error,
-- so the integration suite asserts on the affected row count being zero. The
-- application role is additionally granted only INSERT and SELECT on this table.
CREATE RULE entries_no_update AS ON UPDATE TO entries DO INSTEAD NOTHING;
CREATE RULE entries_no_delete AS ON DELETE TO entries DO INSTEAD NOTHING;

-- ── materialized balances (the hot read path) ────────────────────────────────

CREATE TABLE account_balances (
    account_id    TEXT PRIMARY KEY REFERENCES accounts(id),
    -- Both figures are signed and expressed in the account's normal direction, so a
    -- negative value always means "inverted from what it should be".
    posted_minor  BIGINT NOT NULL DEFAULT 0,
    pending_minor BIGINT NOT NULL DEFAULT 0,
    version       BIGINT NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- available = posted + least(pending, 0) is computed in the API rather than stored,
-- so that there is no third number that can drift away from the other two.

-- Every account gets a balance row the moment it exists, so that the locking path
-- never has to branch on whether one is present.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION create_account_balance() RETURNS trigger AS $$
BEGIN
    INSERT INTO account_balances (account_id) VALUES (NEW.id);
    RETURN NEW;
END; $$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER accounts_create_balance
    AFTER INSERT ON accounts
    FOR EACH ROW EXECUTE FUNCTION create_account_balance();

-- ── the balance invariant, enforced by the database itself ───────────────────

-- I1 and I2, checked at commit rather than per statement so that the entries of a
-- transaction can be inserted in any order.
--
-- This is the layer that catches bugs the Go code does not know it has. It costs one
-- aggregate query per transaction, and in exchange no code path anywhere — including a
-- future migration script or a manual psql session — can write an unbalanced
-- transaction.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION assert_transaction_balanced() RETURNS trigger AS $$
DECLARE
    d BIGINT;
    c BIGINT;
    n INT;
BEGIN
    SELECT COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'debit'),  0),
           COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'credit'), 0),
           COUNT(DISTINCT currency)
      INTO d, c, n
      FROM entries
     WHERE transaction_id = NEW.transaction_id;

    IF n <> 1 THEN
        RAISE EXCEPTION 'txn % spans % currencies', NEW.transaction_id, n
            USING ERRCODE = 'check_violation';
    END IF;

    IF d <> c THEN
        RAISE EXCEPTION 'txn % unbalanced: debits=% credits=%', NEW.transaction_id, d, c
            USING ERRCODE = 'check_violation';
    END IF;

    IF d = 0 THEN
        RAISE EXCEPTION 'txn % has no entries', NEW.transaction_id
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NULL;
END; $$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER entries_balanced
    AFTER INSERT ON entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION assert_transaction_balanced();

-- +goose Down

DROP TRIGGER IF EXISTS entries_balanced ON entries;
DROP FUNCTION IF EXISTS assert_transaction_balanced();
DROP TRIGGER IF EXISTS accounts_create_balance ON accounts;
DROP FUNCTION IF EXISTS create_account_balance();

DROP RULE IF EXISTS entries_no_delete ON entries;
DROP RULE IF EXISTS entries_no_update ON entries;

DROP TABLE IF EXISTS account_balances;
DROP TABLE IF EXISTS entries;
DROP TABLE IF EXISTS transactions;
DROP TABLE IF EXISTS accounts;

DROP TYPE IF EXISTS txn_status;
DROP TYPE IF EXISTS entry_status;
DROP TYPE IF EXISTS balance_side;
DROP TYPE IF EXISTS account_type;
