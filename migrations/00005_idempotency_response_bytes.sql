-- +goose Up

-- The idempotency response is stored as raw bytes, not JSONB.
--
-- DESIGN.md §7.5 requires that a replay return the original body "byte-for-byte", and
-- §7.1 specifies the column as JSONB. Those two cannot both hold: JSONB is a parsed
-- representation, so Postgres normalizes it on the way in — object keys are reordered,
-- insignificant whitespace is dropped and numbers are rewritten. A body stored as JSONB
-- and read back is equivalent JSON but different bytes.
--
-- That matters beyond tidiness. A client that verifies a response signature, compares
-- against a cached copy, or hashes the body to detect changes would see a replay differ
-- from the original for no reason it can act on. The byte-for-byte guarantee is the more
-- important half of the contradiction, so the column becomes BYTEA.
--
-- Nothing queries inside this column — it is opaque to the service, written once and
-- returned verbatim — so JSONB was buying nothing that is given up here.
ALTER TABLE idempotency_keys
    ALTER COLUMN response_body TYPE BYTEA
    USING convert_to(response_body::text, 'UTF8');

-- +goose Down

ALTER TABLE idempotency_keys
    ALTER COLUMN response_body TYPE JSONB
    USING convert_from(response_body, 'UTF8')::jsonb;
