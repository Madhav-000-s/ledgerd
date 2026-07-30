# Ledgerd — Double-Entry Ledger & Payments API

**Status:** Design locked, implementation not started
**Language/runtime:** Go 1.23+
**Datastore:** PostgreSQL 16
**Deploy target:** single binary + worker binary, Docker Compose for local, anything for prod

---

## 1. Purpose & Scope

A minimal but *correct* payments core: an append-only double-entry ledger, an HTTP API
for moving money, idempotent request handling, and a webhook delivery subsystem.

The point of this project is not feature surface. It is to demonstrate that the
hard parts of payments infrastructure — **correctness under concurrency, exactly-once
semantics over an at-least-once world, and auditability** — have been understood and
implemented rather than described.

### In scope

| Area | Included |
|---|---|
| Ledger | Accounts, immutable entries, balanced transactions, invariant enforcement |
| Money | Integer minor units, per-currency exponents, no floats anywhere |
| Payments | Authorize → capture → refund, fee splitting |
| API | REST/JSON, cursor pagination, typed errors, API-key auth |
| Idempotency | Stripe-style keys with request fingerprinting and recovery points |
| Events | Transactional outbox |
| Webhooks | HMAC-signed, exponential backoff w/ full jitter, DLQ, manual replay |
| Concurrency | Deterministic row locking, verified under `-race` |
| Testing | Property tests, concurrency tests, integration tests on Dockerized Postgres |

### Explicitly out of scope

- Real card-network / processor integration. There is no PSP. Money is moved between
  internal accounts only. (The architecture leaves a seam for an external processor —
  see §7.4 recovery points — but nothing is wired to one.)
- Multi-currency FX conversion. A transaction is single-currency. Cross-currency is a
  documented future extension (§14).
- Payouts, disputes/chargebacks, KYC, PCI scope, tokenization.
- Horizontal write scaling, sharding, read replicas. Single-writer Postgres is assumed.
- A UI. `curl` and the integration suite are the interface.

### The one-line thesis

> Every cent that enters the system is accounted for twice, every request can be safely
> retried, and every state change is eventually delivered exactly once to the outside
> world — and all three properties are proven by tests, not asserted by comments.

---

## 2. Foundational Decisions

These are locked. Everything downstream depends on them.

### D1 — Money is `int64` minor units + ISO-4217 currency. No floats, ever.

```go
type Currency string // "USD", "JPY", "BHD"

type Money struct {
    amount   int64    // minor units: cents, yen, fils
    currency Currency
}
```

`float64` cannot represent `0.1` exactly and silently loses precision under
accumulation. Decimal string types are correct but invite accidental division.
`int64` in minor units is exact, comparison-cheap, and maps directly to a Postgres
`BIGINT`. Range is ±9.2×10¹⁸ minor units — ~$92 quadrillion. Sufficient.

Currency exponents come from a compiled-in table (`USD`→2, `JPY`→0, `BHD`→3).
`Money` has no exported field access. Arithmetic returns errors:

```go
func (m Money) Add(o Money) (Money, error)      // ErrCurrencyMismatch
func (m Money) Sub(o Money) (Money, error)
func (m Money) MulBps(bps int64) Money          // round-half-up, exact integer path
func (m Money) Allocate(ratios []int64) []Money // largest-remainder, sums exactly
```

**No `Div`.** Division is where money leaks. The only splitting primitive is
`Allocate`, which uses the largest-remainder method and guarantees
`sum(parts) == total` exactly:

```
Allocate(10000, [1,1,1]) → [3334, 3333, 3333]   sum = 10000 ✓
Allocate(-10000, [1,1,1]) → [-3333, -3333, -3334] sum = -10000 ✓
```

Fees use basis points with explicit round-half-up on the integer path:

```
fee = (amount*bps + 5000) / 10000 + fixed        // integer division
```

Verified cases (`bps=290, fixed=30`, i.e. 2.9% + 30¢):

| amount | fee | exact | net |
|---:|---:|---:|---:|
| 10000 | 320 | 320 | 9680 |
| 999 | 59 | 58.971 | 940 |
| 12345 | 388 | 388.005 | 11957 |
| 50 | 31 | 31.45 | 19 |
| **1** | **30** | **30.029** | **−29** ⚠ |

The last row is the edge case that must be rejected at the domain boundary:
**a fee schedule that produces `fee >= amount` is a validation error, not a negative
transfer.** This check lives in `payments`, not `ledger`.

### D2 — Entries are append-only and immutable. Corrections are compensating transactions.

No `UPDATE`, no `DELETE` on `entries` — enforced by a Postgres rule and by only ever
granting `INSERT, SELECT` to the application role. A mistake is corrected by posting a
reversing transaction that references the original. This is what makes the ledger
auditable: history is a fact, not a mutable projection.

### D3 — A transaction is atomic and must balance per currency.

`Σ debits == Σ credits` within a transaction, per currency. Enforced at three layers
(defence in depth, §4.4): domain constructor, database constraint trigger, and a
periodic trial-balance reconciler.

### D4 — Materialized balances are the read path; derived balances are the truth.

`account_balances` is updated inside the same DB transaction as the entry inserts and
is what authorization checks read. A background reconciler recomputes
`SUM(entries)` per account and alerts on any drift. Drift is a P0 alarm, not a metric
to be smoothed over.

### D5 — Pessimistic row locking with deterministic lock ordering, not `SERIALIZABLE`.

See §6. Chosen for predictable latency and deadlock-freedom by construction, rather
than retry loops under contention.

### D6 — Postgres is the queue. No Kafka, no Redis, no RabbitMQ.

`SELECT ... FOR UPDATE SKIP LOCKED` gives a correct work queue with the same
transactional guarantees as the ledger writes, which is precisely what the outbox
pattern needs. Introducing a broker would *weaken* the guarantee (dual-write problem)
while adding operational surface. This is a deliberate simplification, not an omission.

---

## 3. System Architecture

```
                          ┌───────────────────────────────────┐
   client ──HTTP──────────▶│  ledgerd  (API binary)            │
     │                     │                                   │
     │  Idempotency-Key    │  chi router                       │
     │                     │   └ requestid → auth → idem → h.  │
     │                     │                                   │
     │                     │  httpapi ─▶ payments ─▶ ledger    │
     │                     │                  │        │       │
     │                     │                  └─▶ events(outbox)│
     └─────────────────────└──────────────┬────────────────────┘
                                          │  one DB txn
                                          ▼
                          ┌───────────────────────────────────┐
                          │            PostgreSQL             │
                          │  accounts  entries  transactions  │
                          │  account_balances                 │
                          │  idempotency_keys                 │
                          │  events (outbox)                  │
                          │  webhook_endpoints                │
                          │  webhook_deliveries (queue)       │
                          └───────────────┬───────────────────┘
                                          │ FOR UPDATE SKIP LOCKED
                                          ▼
                          ┌───────────────────────────────────┐
                          │  workerd  (worker binary)         │
                          │   ├ outbox dispatcher (fan-out)   │
                          │   ├ delivery workers (HTTP POST)  │
                          │   ├ reconciler (trial balance)    │
                          │   └ reaper (expired idem keys)    │
                          └───────────────┬───────────────────┘
                                          │ HMAC-signed POST
                                          ▼
                                   merchant endpoints
```

Two binaries, one codebase, one database. `workerd` can be scaled to N replicas
safely — `SKIP LOCKED` makes competing consumers correct without coordination.

### 3.1 Package layout

```
cmd/
  ledgerd/main.go            API server
  workerd/main.go            dispatcher + delivery + reconciler + reaper
internal/
  money/                     Money, Currency, exponents, Allocate, MulBps
  ledger/                    domain: Account, Transaction, Entry, invariants, Service
  payments/                  Authorize/Capture/Refund orchestration over ledger
  idempotency/               Store, middleware, recovery points
  events/                    event types, outbox writer
  webhooks/                  dispatcher, delivery worker, signer, backoff
  httpapi/                   router, handlers, error envelope, pagination, auth
  postgres/                  pgxpool, TxManager, sqlc-generated queries
  platform/
    config/  log/  metrics/  health/
migrations/                  goose .sql files, embedded via embed.FS
test/
  integration/               testcontainers-go suites
  property/                  rapid-based invariant tests
```

Dependency direction is strictly inward: `httpapi → payments → ledger → money`.
`ledger` imports nothing from `postgres`; it defines the repository interfaces it
needs and `postgres` implements them. This is what makes the domain testable without
Docker.

### 3.2 Library choices

| Concern | Choice | Why |
|---|---|---|
| Router | `chi` | stdlib-compatible `http.Handler`, clean middleware chain, route groups. `net/http` ServeMux is now viable but chi's middleware ergonomics matter for the idempotency layer. |
| DB driver | `pgx/v5` + `pgxpool` | Native protocol, real `int64`/`numeric` handling, `CopyFrom`, no `database/sql` impedance. |
| Queries | `sqlc` | Type-safe generated Go from hand-written SQL. Keeps SQL visible and reviewable — an ORM would hide exactly the locking semantics that matter here. |
| Migrations | `goose` | Embeddable via `embed.FS`, plain SQL, up/down. |
| Logging | `log/slog` | stdlib structured logging, JSON handler in prod. |
| Metrics | `prometheus/client_golang` | |
| Tracing | OpenTelemetry | Span per request, per DB txn, per delivery attempt. |
| Testing | stdlib + `testify/require` + `pgregory.net/rapid` + `testcontainers-go` | |
| Lint | `golangci-lint` (errcheck, staticcheck, gosec, bodyclose, sqlclosecheck) | |

### 3.3 Transaction management

The single most important internal API. Repositories must be composable into one
database transaction without threading `*pgx.Tx` through every signature.

```go
// internal/postgres
type TxManager struct{ pool *pgxpool.Pool }

// InTx runs fn inside a DB transaction. Repositories inside fn pick up the tx
// from ctx automatically. Nested InTx calls join the outer transaction.
func (m *TxManager) InTx(ctx context.Context, fn func(ctx context.Context) error) error
```

The `pgx.Tx` is carried in `context.Context` under an unexported key. Every repository
method starts with `q := r.queries(ctx)` which returns the tx-bound `*sqlc.Queries` if
present, else the pool-bound one. Rules:

- `InTx` is called **only** from the service layer (`payments`, `webhooks`), never
  from handlers or repositories.
- No external I/O (HTTP calls, sleeps) inside `InTx`. Ever. Holding a DB transaction
  open across a network call is how a ledger deadlocks under load.
- Nested `InTx` joins rather than opening a savepoint. Savepoints are available via an
  explicit `InNestedTx` if ever needed; currently unused.

---

## 4. The Ledger

### 4.1 Account model

Accounts are typed by their role in the accounting equation. `normal_balance` is
derived from `type` and stored denormalized for query convenience.

| type | normal balance | example |
|---|---|---|
| `asset` | debit | `platform_cash`, `clearing` |
| `liability` | credit | `merchant_payable:acct_123`, `customer_balance:cus_9` |
| `equity` | credit | `retained_earnings` |
| `revenue` | credit | `fee_revenue` |
| `expense` | debit | `processing_cost`, `chargeback_loss` |

Balance sign convention: a balance is stored as a signed `int64` in the account's
**normal** direction. An asset account with a debit balance of 500 stores `+500`;
a liability with a credit balance of 500 also stores `+500`. This means
"negative balance" always means "this account is inverted from what it should be",
which is what the `allow_negative` flag guards.

The accounting equation, checked by the reconciler:

```
Σ assets − Σ liabilities − Σ equity − Σ revenue + Σ expenses == 0
```

### 4.2 Schema

```sql
-- ── accounts ─────────────────────────────────────────────────────────────
CREATE TYPE account_type    AS ENUM ('asset','liability','equity','revenue','expense');
CREATE TYPE balance_side    AS ENUM ('debit','credit');
CREATE TYPE entry_status    AS ENUM ('pending','posted','voided');
CREATE TYPE txn_status      AS ENUM ('pending','posted','voided');

CREATE TABLE accounts (
    id             TEXT PRIMARY KEY,              -- "acct_01HQ..." ULID
    merchant_id    TEXT NOT NULL,
    name           TEXT NOT NULL,
    type           account_type NOT NULL,
    normal_balance balance_side NOT NULL,
    currency       CHAR(3) NOT NULL,
    allow_negative BOOLEAN NOT NULL DEFAULT FALSE,
    metadata       JSONB NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT normal_matches_type CHECK (
        (type IN ('asset','expense')                  AND normal_balance = 'debit') OR
        (type IN ('liability','equity','revenue')     AND normal_balance = 'credit')
    ),
    UNIQUE (merchant_id, name)
);

-- ── transactions (journal entries) ───────────────────────────────────────
CREATE TABLE transactions (
    id             TEXT PRIMARY KEY,              -- "txn_01HQ..."
    merchant_id    TEXT NOT NULL,
    status         txn_status NOT NULL,
    currency       CHAR(3) NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    reverses_id    TEXT REFERENCES transactions(id),
    external_ref   TEXT,                          -- payment/refund id
    metadata       JSONB NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_at      TIMESTAMPTZ
);
CREATE INDEX ON transactions (merchant_id, created_at DESC, id DESC);
CREATE UNIQUE INDEX ON transactions (reverses_id) WHERE reverses_id IS NOT NULL;

-- ── entries (postings) — APPEND ONLY ─────────────────────────────────────
CREATE TABLE entries (
    id             BIGSERIAL PRIMARY KEY,
    transaction_id TEXT NOT NULL REFERENCES transactions(id),
    account_id     TEXT NOT NULL REFERENCES accounts(id),
    direction      balance_side NOT NULL,
    amount_minor   BIGINT NOT NULL CHECK (amount_minor > 0),  -- sign lives in direction
    currency       CHAR(3) NOT NULL,
    status         entry_status NOT NULL,
    seq            SMALLINT NOT NULL,             -- ordinal within transaction
    balance_after  BIGINT NOT NULL,               -- account's posted balance after this entry
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (transaction_id, seq)
);
CREATE INDEX ON entries (account_id, id DESC);
CREATE INDEX ON entries (transaction_id);

CREATE RULE entries_no_update AS ON UPDATE TO entries DO INSTEAD NOTHING;
CREATE RULE entries_no_delete AS ON DELETE TO entries DO INSTEAD NOTHING;

-- ── materialized balances (the hot read path) ────────────────────────────
CREATE TABLE account_balances (
    account_id      TEXT PRIMARY KEY REFERENCES accounts(id),
    posted_minor    BIGINT NOT NULL DEFAULT 0,   -- signed, in normal direction
    pending_minor   BIGINT NOT NULL DEFAULT 0,   -- holds not yet captured
    version         BIGINT NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

> `available = posted − pending_debits` for debit-normal accounts. Exposed as a
> computed field in the API, never stored, to avoid a third thing that can drift.

`balance_after` on each entry is a cheap, enormously valuable audit artifact: it turns
"reconstruct the balance as of timestamp T" from a full scan into an index seek, and it
makes drift bisectable — the reconciler can binary-search to the exact entry where
derived and materialized diverged.

### 4.3 Constraint trigger for the balance invariant

A `CONSTRAINT TRIGGER ... DEFERRABLE INITIALLY DEFERRED` fires at commit, after all
entries for a transaction are inserted:

```sql
CREATE OR REPLACE FUNCTION assert_transaction_balanced() RETURNS trigger AS $$
DECLARE d BIGINT; c BIGINT; n INT;
BEGIN
    SELECT COALESCE(SUM(amount_minor) FILTER (WHERE direction='debit'),  0),
           COALESCE(SUM(amount_minor) FILTER (WHERE direction='credit'), 0),
           COUNT(DISTINCT currency)
      INTO d, c, n
      FROM entries WHERE transaction_id = NEW.transaction_id;

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

CREATE CONSTRAINT TRIGGER entries_balanced
    AFTER INSERT ON entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION assert_transaction_balanced();
```

This is the layer that catches bugs the Go code doesn't know it has. It costs one
aggregate query per transaction — acceptable, and it means *no code path anywhere,
including a future migration script or a manual `psql` session, can write an
unbalanced transaction.*

### 4.4 Invariants and where each is enforced

| # | Invariant | Domain | DB | Reconciler |
|---|---|---|---|---|
| I1 | `Σ debits == Σ credits` per transaction | ✓ constructor | ✓ trigger | ✓ |
| I2 | Single currency per transaction | ✓ | ✓ trigger | ✓ |
| I3 | `amount_minor > 0` | ✓ | ✓ CHECK | — |
| I4 | Entries immutable | — | ✓ RULE | ✓ (hash chain, future) |
| I5 | `normal_balance` matches `type` | ✓ | ✓ CHECK | — |
| I6 | Balance ≥ 0 unless `allow_negative` | ✓ (in lock) | — | ✓ |
| I7 | `account_balances` == `SUM(entries)` | — | — | ✓ **P0 alarm** |
| I8 | Global trial balance sums to zero | — | — | ✓ **P0 alarm** |
| I9 | Accounting equation holds | — | — | ✓ |

I6 cannot be a DB constraint because it's a *cross-row* condition evaluated against a
locked snapshot; it lives in the critical section (§6).

### 4.5 Ledger service API

```go
package ledger

type Direction string // "debit" | "credit"

type EntrySpec struct {
    AccountID string
    Direction Direction
    Amount    money.Money
}

type TransactionSpec struct {
    MerchantID  string
    Description string
    ExternalRef string
    Entries     []EntrySpec
    Pending     bool            // true → entries land as 'pending'
    Metadata    map[string]any
}

type Service interface {
    // Post validates, locks, writes entries, updates balances — all inside the
    // caller's ctx transaction. Fails with ErrUnbalanced, ErrInsufficientFunds,
    // ErrCurrencyMismatch, ErrAccountNotFound.
    Post(ctx context.Context, spec TransactionSpec) (*Transaction, error)

    // Capture converts pending entries of txn to posted (optionally for a lesser
    // amount, writing a compensating release for the remainder).
    Capture(ctx context.Context, txnID string, amount *money.Money) (*Transaction, error)

    // Void releases all pending entries of a transaction.
    Void(ctx context.Context, txnID string) (*Transaction, error)

    // Reverse posts a new transaction that is the mirror image of txnID.
    Reverse(ctx context.Context, txnID, reason string) (*Transaction, error)

    Balance(ctx context.Context, accountID string) (Balance, error)
    ListEntries(ctx context.Context, accountID string, p Page) ([]Entry, string, error)
}
```

`Post` is the only write primitive. `payments` composes it; it never touches
`entries` directly.

---

## 5. Payments Layer

Payments are *compositions* of ledger transactions. Nothing about a "payment" is
special to the ledger.

### 5.1 Charge with platform fee (2.9% + 30¢ on $100.00)

```
POST /v1/payments  { amount: 10000, currency: "USD", merchant_account: "acct_M" }

Ledger transaction txn_A (posted):
  DR  platform_clearing            10000     (asset ↑)
  CR  merchant_payable:acct_M       9680     (liability ↑)
  CR  fee_revenue                    320     (revenue ↑)
                          debits = credits = 10000 ✓
```

### 5.2 Authorization → capture (the two-phase path)

**Authorize** posts a *pending* transaction. Pending entries move `pending_minor`, not
`posted_minor`, so `available` drops immediately while `posted` does not.

```
txn_B (pending):
  DR  customer_balance:cus_9        10000
  CR  authorization_holds           10000
```

**Capture** (full): voids the hold and posts the real movement in one DB transaction.
**Capture** (partial, $60): posts $60, releases $40.
**Void / expiry**: releases the hold, no posted entries ever exist. Expiry is a
7-day sweep in `workerd`.

### 5.3 Refund

Never an `UPDATE`. A refund is `ledger.Reverse` on the capture transaction (full) or a
new mirror transaction for a partial amount. Cumulative refunded amount is checked
against the original inside the same lock. Fee-refund policy is a config flag —
default: fixed component is retained, percentage component is returned.

### 5.4 Public payment states

`requires_capture → succeeded → refunded / partially_refunded`, plus `canceled`.
The state is *derived* from ledger facts, not stored as an independent mutable column,
so it cannot disagree with the books.

---

## 6. Concurrency & Correctness

### 6.1 The critical section

For each money movement:

```
BEGIN
  1. resolve all account_ids touched by the transaction
  2. sort account_ids ASCENDING
  3. SELECT ... FROM account_balances WHERE account_id = ANY($1) ORDER BY account_id
     FOR UPDATE                               ← locks acquired in a total order
  4. validate: currency match, sufficient funds (I6), refund caps
  5. INSERT transactions
  6. INSERT entries (computing balance_after from the locked values)
  7. UPDATE account_balances SET posted/pending, version = version + 1
  8. INSERT INTO events (outbox)              ← same txn, this is the whole point
  9. INSERT/UPDATE idempotency_keys with the serialized response  ← also same txn
COMMIT   ← deferred balance trigger fires here
```

### 6.2 Why pessimistic locking with ordered acquisition

| Option | Verdict |
|---|---|
| `SERIALIZABLE` + retry on `40001` | Correct, but under contention on a hot account (the platform fee account is touched by *every* charge) the abort rate approaches 100% and throughput collapses into a retry storm. Latency becomes unbounded and unpredictable. |
| Optimistic (`version` CAS + retry) | Same failure shape, plus we hand-roll what Postgres already does. |
| **`SELECT FOR UPDATE`, IDs sorted ascending** | **Chosen.** Under `READ COMMITTED`. Sorting gives a total order on lock acquisition, which makes deadlock **structurally impossible** — the classic dining-philosophers fix. Contenders queue instead of aborting; latency degrades linearly rather than falling off a cliff. |

Trade-off accepted: throughput on a single hot account is bounded by lock hold time
(~1–3ms). Mitigation, designed for but not built: **balance sharding** — a logical
account fans out to N physical sub-accounts, writes pick one at random, reads sum
across them. The schema supports this via a nullable `accounts.shard_of` column
reserved now, unused in MVP.

A `SERIALIZABLE` mode is available behind `LEDGER_ISOLATION=serializable` purely so the
concurrency test suite can assert *both* strategies produce identical final state. If
they ever disagree, the locking strategy has a bug.

`lock_timeout = 3s` and `statement_timeout = 10s` are set on the connection so a
pathological case fails loudly rather than hanging.

### 6.3 The headline test

```go
// test/integration/concurrency_test.go
// Run with: go test -race -count=1 ./test/integration -run TestConcurrentTransfers
//
//   64 goroutines × 500 iterations, random transfers among 16 accounts,
//   random amounts, ~20% of attempts intentionally overdraw.
//
// Assertions after the storm:
//   A. Σ all account balances == Σ initial balances          (conservation of money)
//   B. no account with allow_negative=false went below zero  (I6)
//   C. Σ debits == Σ credits over every transaction          (I1)
//   D. materialized balance == SUM(entries) for every account (I7)
//   E. count(successful posts) + count(ErrInsufficientFunds) == total attempts
//      (no request vanished, none double-applied)
//   F. every entry's balance_after replays to the final balance in id order
//   G. zero deadlock errors (40P01) observed
```

Assertion F is the one that catches subtle lost-update bugs that A–D can miss, because
it validates the *sequence*, not just the endpoint.

The same test runs twice in CI: once under `READ COMMITTED` + row locks, once under
`SERIALIZABLE`. Both must produce a consistent final state.

### 6.4 Go-level race safety

- `-race` on the full test suite in CI, non-negotiable.
- No shared mutable state in services; everything flows through `context` and the DB.
- Worker pools use `errgroup` with a bounded semaphore; no unbounded goroutine spawning
  per delivery.
- Graceful shutdown: `signal.NotifyContext` → stop accepting → drain in-flight with a
  30s deadline → close pool.

---

## 7. Idempotency

Modeled directly on Stripe's published approach: fingerprinted keys, persisted
responses, and phased execution with recovery points.

### 7.1 Schema

```sql
CREATE TYPE idem_state AS ENUM ('in_progress','succeeded','failed');

CREATE TABLE idempotency_keys (
    id                TEXT PRIMARY KEY,
    merchant_id       TEXT NOT NULL,
    idempotency_key   TEXT NOT NULL,
    request_method    TEXT NOT NULL,
    request_path      TEXT NOT NULL,
    request_hash      BYTEA NOT NULL,        -- sha256(canonical body)
    state             idem_state NOT NULL,
    recovery_point    TEXT NOT NULL,         -- 'started'|'ledger_posted'|'finished'
    lock_expires_at   TIMESTAMPTZ,           -- NULL when terminal
    response_status   INT,
    response_body     JSONB,
    resource_id       TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '24 hours',
    UNIQUE (merchant_id, idempotency_key)
);
CREATE INDEX ON idempotency_keys (expires_at);
```

The unique constraint on `(merchant_id, idempotency_key)` scoped by merchant is what
makes this safe multi-tenant — one merchant cannot collide with or probe another's keys.

### 7.2 Middleware decision flow

```
Idempotency-Key header present?
├─ no  → money-mutating route? → 400 idempotency_key_required
│                              → else: pass through
└─ yes → validate: 1..255 chars, printable ASCII

  INSERT ... ON CONFLICT (merchant_id, idempotency_key) DO NOTHING RETURNING *
  ├─ inserted → we own it. lock_expires_at = now()+60s. execute handler.
  └─ conflict → SELECT existing row
       ├─ request_hash ≠ ours          → 422 idempotency_key_reuse
       ├─ state = succeeded|failed     → replay stored response
       │                                 + header  Idempotent-Replay: true
       ├─ state = in_progress
       │    ├─ lock_expires_at > now() → 409 idempotency_conflict  (Retry-After: 1)
       │    └─ lock expired (crash)    → take over the lock, resume at recovery_point
```

**Critical ordering property:** the idempotency row's transition to `succeeded` and the
serialized response body are written *in the same DB transaction as the ledger entries*
(§6.1 steps 5–9). There is therefore no window in which money moved but the response
was not recorded. A crash at any instant leaves the system in exactly one of two states:
nothing happened, or everything happened and the response is replayable.

This is the whole trick, and it is why the ledger write and the idempotency write must
not be separated by a network call.

### 7.3 Request fingerprinting

`request_hash = SHA-256(method || "\n" || path || "\n" || canonicalJSON(body))`.
Canonicalization: keys sorted, insignificant whitespace stripped, numbers normalized.
Without canonicalization, a client that reserializes its map in a different key order
gets a spurious 422 on a legitimate retry — a real and infuriating failure mode.

### 7.4 Recovery points

The seam for external side effects. An operation is a sequence of **atomic phases**;
each phase commits its work *and* advances `recovery_point` in one DB transaction.
Foreign calls happen strictly between phases.

```
POST /v1/payments
   started        ──[DB txn: post ledger, write outbox event]──▶ ledger_posted
   ledger_posted  ──[DB txn: persist response]────────────────▶ finished
```

In MVP the two phases collapse into one, so the machinery looks like overkill — and it
is, today. It exists because the moment a real PSP is introduced, the shape becomes:

```
   started       ──[DB: reserve funds, pending]──▶ funds_reserved
   funds_reserved──[HTTP: charge the PSP]───────▶ (no DB write; PSP is idempotent by our key)
                 ──[DB: capture, persist resp]──▶ finished
```

and a crash after the PSP call but before the capture commit is recoverable: the retry
resumes at `funds_reserved`, re-issues the PSP call with the same idempotency key, gets
the cached PSP response, and completes. Designing the state machine now costs nothing
and is the difference between a demo and infrastructure.

### 7.5 Lifecycle

- Lock lease 60s; `lock_expires_at` refreshed on phase advance for long operations.
- Keys expire after 24h; the reaper deletes in batches of 1000 every 5 minutes.
- Replays return the original status code and body byte-for-byte, plus
  `Idempotent-Replay: true` and the original `Request-Id` in `Original-Request-Id`.
- 5xx responses are recorded as `failed` but **do not** lock the key — a retry after a
  server error must be allowed to genuinely re-execute.

---

## 8. Events & Webhooks

### 8.1 Transactional outbox

```sql
CREATE TABLE events (
    id            TEXT PRIMARY KEY,          -- "evt_01HQ..."
    merchant_id   TEXT NOT NULL,
    type          TEXT NOT NULL,             -- 'payment.succeeded'
    resource_id   TEXT NOT NULL,
    payload       JSONB NOT NULL,
    dispatched_at TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON events (dispatched_at, id) WHERE dispatched_at IS NULL;
```

The event row is inserted **in the same DB transaction as the ledger entries**. This
eliminates the dual-write problem outright: it is impossible to move money without
producing an event, and impossible to produce an event for money that didn't move. Any
design that publishes to a broker from application code after committing has a window
where those two facts disagree — and in payments that window is a reconciliation
incident.

`pg_notify('events', id)` on commit gives the dispatcher sub-second latency; a 1s
polling fallback covers missed notifications (NOTIFY is not durable).

### 8.2 Fan-out and the delivery queue

```sql
CREATE TYPE delivery_state AS ENUM ('pending','delivering','succeeded','exhausted','disabled');

CREATE TABLE webhook_endpoints (
    id             TEXT PRIMARY KEY,
    merchant_id    TEXT NOT NULL,
    url            TEXT NOT NULL,
    enabled_events TEXT[] NOT NULL,          -- ['payment.*'] glob supported
    status         TEXT NOT NULL DEFAULT 'active',  -- active|degraded|disabled
    consecutive_failures INT NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE webhook_secrets (         -- multiple active secrets → rotation
    id          TEXT PRIMARY KEY,
    endpoint_id TEXT NOT NULL REFERENCES webhook_endpoints(id),
    secret_hash BYTEA NOT NULL,        -- the secret itself is shown once at creation
    secret_enc  BYTEA NOT NULL,        -- AES-GCM, key from env/KMS
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE webhook_deliveries (
    id             TEXT PRIMARY KEY,
    event_id       TEXT NOT NULL REFERENCES events(id),
    endpoint_id    TEXT NOT NULL REFERENCES webhook_endpoints(id),
    state          delivery_state NOT NULL DEFAULT 'pending',
    attempt        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_status    INT,
    last_error     TEXT,
    last_duration_ms INT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (event_id, endpoint_id)        -- fan-out is idempotent
);
CREATE INDEX ON webhook_deliveries (next_attempt_at)
    WHERE state IN ('pending','delivering');

CREATE TABLE webhook_attempts (           -- append-only audit of every try
    id BIGSERIAL PRIMARY KEY,
    delivery_id TEXT NOT NULL REFERENCES webhook_deliveries(id),
    attempt INT NOT NULL,
    status_code INT, error TEXT, duration_ms INT,
    attempted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

The dispatcher claims undispatched events, expands them against matching endpoints,
inserts delivery rows, and marks `dispatched_at` — all in one transaction. The
`UNIQUE (event_id, endpoint_id)` makes a re-run harmless.

Delivery workers claim work with the canonical Postgres queue pattern:

```sql
UPDATE webhook_deliveries SET state = 'delivering'
WHERE id IN (
    SELECT id FROM webhook_deliveries
    WHERE state = 'pending' AND next_attempt_at <= now()
    ORDER BY next_attempt_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED           -- competing consumers, no coordination needed
)
RETURNING *;
```

A `delivering` row whose lease exceeds 60s is reset to `pending` by a sweeper —
this is the crash-recovery path for a worker that dies mid-POST.

### 8.3 Signing

Stripe-compatible scheme, chosen deliberately so any consumer already written against
Stripe's verification works unmodified.

```
Webhook-Signature: t=1753776000,v1=5257a86...,v1=9f2b1c...
signed_payload    = "{t}.{raw_request_body}"
v1                = hex(HMAC_SHA256(endpoint_secret, signed_payload))
```

- One `v1` per active secret → zero-downtime rotation.
- Consumers must reject `|now − t| > 300s` (replay window) and compare with a
  constant-time equality (`hmac.Equal`). Both are stated in the docs and enforced in
  the reference verifier shipped in `pkg/webhookverify`.
- The signature covers the **raw bytes** sent, never a re-serialization. The delivery
  worker marshals once and signs and sends the exact same `[]byte`.

### 8.4 Retry policy: exponential backoff with full jitter

```go
// delay = rand(0, min(cap, base * 2^attempt))
base := 10 * time.Second
cap  := 6 * time.Hour
maxAttempts := 16
```

**Full jitter, not "exponential + a bit of noise".** After a merchant endpoint recovers
from an outage, thousands of deliveries become due simultaneously; without full jitter
they arrive as a synchronized thundering herd and knock the endpoint back over.
Randomizing across the *entire* interval decorrelates them.

Per-attempt caps (seconds), 16 attempts:

```
10, 20, 40, 80, 160, 320, 640, 1280, 2560, 5120, 10240, 20480, 21600, 21600, 21600, 21600
```

- Worst-case retry window: **~35.4 hours**
- Expected window (uniform jitter): **~17.7 hours**

Explicitly *not* claiming a 3-day window — the numbers above are what the schedule
actually produces, and the doc says so.

Retry classification:

| Response | Action |
|---|---|
| `2xx` | `succeeded`, reset endpoint `consecutive_failures` |
| `410 Gone` | `disabled` immediately — the endpoint is telling us to stop |
| `429`, `503` w/ `Retry-After` | honor the header, capped at `cap` |
| other `4xx` | retry (a 404 may be a deploy in progress) |
| `5xx`, timeout, DNS, TLS, conn refused | retry |
| exhausted attempts | `exhausted` → DLQ |

Per-attempt HTTP client: 5s total timeout, 2s connect, redirects **not** followed
(a redirect to an internal address is an SSRF vector), response body read to a 64 KiB
cap then discarded.

### 8.5 SSRF hardening

Webhooks POST to a merchant-controlled URL, which makes the delivery worker a
confused deputy by default.

- Scheme must be `https` (`http` allowed only when `ALLOW_INSECURE_WEBHOOKS=true`, for
  local tests).
- Custom `DialContext` resolves DNS itself and rejects any resolved IP in
  loopback / link-local / RFC1918 / RFC4193 / `169.254.0.0/16` / CGNAT ranges —
  **checked at dial time on the actual connection IP**, which closes the DNS-rebinding
  hole that validating the URL at registration time leaves open.
- No redirects. No proxy env inheritance.
- URL is validated at endpoint creation *and* at every dial.

### 8.6 Dead-letter queue

`state = 'exhausted'` is the DLQ. Exhausted deliveries are retained 30 days and:

- increment `webhook_endpoints.consecutive_failures`
- at 20 consecutive → endpoint `degraded`, an internal alert fires, and an
  `endpoint.degraded` event is emitted to the merchant's *other* endpoints
- at 100 consecutive → endpoint `disabled`, requires manual re-enable

Operator/merchant recovery:

```
GET  /v1/webhook_endpoints/:id/deliveries?state=exhausted
POST /v1/webhook_endpoints/:id/deliveries/:did/replay   → resets attempt=0, state=pending
POST /v1/webhook_endpoints/:id/replay_all?since=...     → bulk, rate-limited
```

### 8.7 Delivery semantics — stated, not implied

**At-least-once, unordered.** Documented as a first-class contract:

- Consumers **must** dedupe on `event.id`.
- Consumers **must not** assume ordering; `payment.refunded` can arrive before
  `payment.succeeded`. Order by `event.created_at` and treat handlers as commutative.
- Exactly-once delivery is not achievable across a network partition; pretending
  otherwise pushes the problem to consumers silently instead of loudly.

Per-endpoint ordered delivery via `pg_advisory_xact_lock(hash(endpoint_id))` is
possible and deliberately not implemented — it converts the endpoint into a serial
bottleneck and makes head-of-line blocking a merchant-visible outage.

---

## 9. HTTP API

### 9.1 Surface

```
POST   /v1/accounts                                create account
GET    /v1/accounts/:id
GET    /v1/accounts/:id/balance                    {posted, pending, available}
GET    /v1/accounts/:id/entries                    cursor-paginated ledger view

POST   /v1/transfers                    [idem]     raw balanced ledger movement
POST   /v1/payments                     [idem]     charge (optionally capture=false)
POST   /v1/payments/:id/capture         [idem]
POST   /v1/payments/:id/cancel          [idem]
POST   /v1/refunds                      [idem]

GET    /v1/transactions/:id                        with entries expanded
GET    /v1/transactions                            filter by merchant/date/ref

POST   /v1/webhook_endpoints                       returns secret ONCE
GET    /v1/webhook_endpoints/:id
POST   /v1/webhook_endpoints/:id/secrets/rotate
GET    /v1/webhook_endpoints/:id/deliveries
POST   /v1/webhook_endpoints/:id/deliveries/:did/replay

GET    /v1/events                                  event log
GET    /v1/events/:id

GET    /healthz   liveness (no deps)
GET    /readyz    readiness (pings DB)
GET    /metrics   Prometheus
```

`[idem]` = `Idempotency-Key` header required; 400 without it.

### 9.2 Middleware chain (order matters)

```
RequestID → Recoverer → StructuredLogger → Metrics → Timeout(30s)
  → Auth(API key) → RateLimit(per-key token bucket) → BodyLimit(1MiB)
  → Idempotency → handler
```

RealIP was in this chain and has been dropped. It rewrote `RemoteAddr` from
`X-Forwarded-For`/`True-Client-IP`/`X-Real-IP` unconditionally, which lets any client
pick the address the service believes it came from. Nothing here reads `RemoteAddr` —
rate limiting is keyed on the API key — so it was a spoofing vector buying no behaviour.
Reintroducing a client address requires an explicit trusted-proxy hop count.

Idempotency sits **after** auth (the key is merchant-scoped, so we need the merchant
first) and **after** body limiting (we hash the body, so it must be bounded).
It sits **before** the handler and buffers the response through a
`httptest.ResponseRecorder`-style capturing writer so the exact bytes can be persisted.

### 9.3 Error envelope

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "insufficient_funds",
    "message": "Account acct_M has available balance 4200, requested 10000.",
    "param": "amount",
    "request_id": "req_01HQ...",
    "doc_url": "https://.../errors#insufficient_funds"
  }
}
```

`type` ∈ `invalid_request_error | idempotency_error | ledger_error | rate_limit_error |
authentication_error | api_error`. Domain sentinel errors map to
`(type, code, http_status)` in exactly one table in `httpapi/errors.go` — the mapping
is data, not scattered `if errors.Is(...)` in handlers.

### 9.4 Pagination

Keyset, never `OFFSET`:

```
GET /v1/accounts/acct_M/entries?limit=100&starting_after=en_01HQ...
→ { "data": [...], "has_more": true, "next_cursor": "en_01HQ..." }
```

Cursor is an opaque base64 of `(created_at, id)`. `OFFSET` degrades linearly and can
skip or duplicate rows when concurrent inserts land — unacceptable for a ledger view.

### 9.5 Auth

API keys, `sk_live_` / `sk_test_` prefixed. Stored as `argon2id` hashes plus a
non-secret 8-char lookup prefix so verification is one index seek and one hash rather
than a table scan. Keys carry a merchant scope and a role (`read` / `write` / `admin`).
Constant-time comparison. Keys are shown exactly once.

---

## 10. Observability

**Logs** (`slog`, JSON): every line carries `request_id`, `merchant_id`, `route`.
Money amounts are logged as integers with currency, never formatted strings. No PII,
no full request bodies, no secrets — a `slog.LogValuer` on `APIKey` and
`WebhookSecret` renders them as `[REDACTED]` so they cannot leak by accident.

**Metrics:**

```
http_requests_total{route,method,status}
http_request_duration_seconds{route}          histogram
ledger_transactions_total{result}
ledger_lock_wait_seconds                      histogram   ← contention early-warning
ledger_trial_balance_drift_minor              gauge, MUST be 0
ledger_balance_drift_accounts                 gauge, MUST be 0
idempotency_outcomes_total{outcome}           new|replay|conflict|mismatch
webhook_delivery_attempts_total{outcome}
webhook_delivery_latency_seconds              histogram
webhook_queue_depth{state}                    gauge
webhook_oldest_pending_seconds                gauge       ← the real SLI
db_pool_{acquired,idle,waiting}
```

**Traces:** OTel span per request; child spans for `InTx`, each lock acquisition, and
each delivery attempt. `traceparent` is propagated into webhook POSTs so a merchant
running OTel can correlate.

**Alerts:** `trial_balance_drift ≠ 0` and `balance_drift_accounts > 0` are **P0, page
immediately**. Everything else is a ticket.

---

## 11. Testing Strategy

Coverage target ≥85% on `internal/`, with the explicit caveat that coverage is a floor,
not the goal. The invariant and concurrency suites are the real signal.

### 11.1 Unit (no Docker, milliseconds)

- `money`: table-driven arithmetic, overflow behavior, currency mismatch, exponent
  handling for 0/2/3-decimal currencies.
- `ledger`: transaction construction, invariant rejection, reversal shape.
- `webhooks/backoff`: delay is always within `[0, min(cap, base·2ⁿ)]`.
- `webhooks/sign`: known-answer vectors; the reference verifier round-trips.

### 11.2 Property-based (`pgregory.net/rapid`)

```go
// For any sequence of randomly generated *balanced* transactions over a random
// chart of accounts:
//   P1  trial balance sums to zero
//   P2  Σ assets − Σ liabilities − Σ equity − Σ revenue + Σ expenses == 0
//   P3  Allocate(n, ratios) sums exactly to n for all n (incl. negative), all ratios
//   P4  MulBps never overflows for |amount| < 2^53
//   P5  Reverse(Reverse(t)) leaves every balance identical to before t
```

P5 is worth more than a hundred hand-written cases.

### 11.3 Integration (`testcontainers-go`, real Postgres 16)

One container per package, migrations applied on start, each test in a rolled-back
transaction or a truncated schema. Never mock the database — the entire design rests on
Postgres locking semantics, and a mock would validate the mock.

- Full flows: create accounts → authorize → capture → refund → assert entries.
- Trigger enforcement: attempt an unbalanced insert via raw SQL, expect
  `check_violation`. Attempt `UPDATE entries`, expect zero rows affected.
- Overdraw rejection and `allow_negative` accounts.

### 11.4 Idempotency suite

- Same key, same body, 50 concurrent → exactly **1** ledger transaction; the other 49
  are `409` or replays; no third outcome.
- Same key, different body → `422`.
- Kill the process (`ctx` cancel mid-handler) after ledger commit, retry → replay of
  the persisted response, **no second transaction**.
- Stale lock takeover: manually expire `lock_expires_at`, retry, assert resume.
- 5xx does not lock the key.

### 11.5 Webhook suite

- `httptest.Server` that fails N times then succeeds → assert attempt count, assert
  every observed delay falls within its jitter bound, assert `succeeded`.
- Always-failing endpoint with a compressed schedule → `exhausted`, appears in DLQ,
  endpoint transitions to `degraded`.
- Signature verifies against the reference verifier; a tampered body fails; a
  6-minute-old timestamp fails.
- Secret rotation: both old and new secrets validate during the overlap window.
- SSRF: endpoints resolving to `127.0.0.1` / `169.254.169.254` / `10.0.0.0/8` are
  refused at dial time, including via a DNS-rebinding stub resolver.
- Two worker replicas against one queue → no delivery processed twice.

### 11.6 Concurrency — see §6.3.

### 11.7 Fuzz

`FuzzCanonicalJSON`, `FuzzParseSignatureHeader`, `FuzzMoneyParse`. Seed corpus
committed; 30s per target in CI, longer nightly.

### 11.8 CI (GitHub Actions)

```yaml
- go vet ./...
- golangci-lint run
- govulncheck ./...
- go test -race -count=1 -coverprofile=cover.out ./...
- go test -race -tags=integration ./test/integration/...
- go test -run=Fuzz -fuzz=Fuzz -fuzztime=30s ./internal/...   # per target
- coverage gate: fail under 85% on ./internal/...
```

Matrix on Go 1.23 and 1.24. Postgres 15 and 16 for the integration job.

---

## 12. Failure Modes

| Failure | Behavior | Guarantee preserved |
|---|---|---|
| Crash mid-request, before commit | Nothing written | No partial ledger state |
| Crash after commit, before response | Client retries with same key → replay | Exactly-once effect |
| Crash after ledger, before webhook send | Outbox row committed; dispatcher picks it up | No lost events |
| DB connection lost mid-txn | `pgx` returns error, txn aborts | Atomicity |
| Two requests, same idempotency key | Unique constraint → one wins, other 409/replays | No double-charge |
| Concurrent transfers, same account | Ordered `FOR UPDATE` serializes them | No lost update, no deadlock |
| Merchant endpoint down 12h | Backoff retries, ~17.7h expected window | Delivery survives outage |
| Merchant endpoint permanently gone | 16 attempts → DLQ → degraded → disabled | Bounded resource use |
| Delivery worker dies mid-POST | Lease expires at 60s, row reset to `pending` | At-least-once |
| Materialized balance drifts | Reconciler detects, P0 page, `balance_after` bisects to the offending entry | Detectability |
| Clock skew on webhook consumer | 5-minute tolerance window | Verification still works |
| Hot account contention | Lock queue grows, `ledger_lock_wait_seconds` rises | Degrades linearly, alerts before failure |

---

## 13. Build Phases

Foundations are proven before anything is layered on them.

**Phase 0 — Domain, no I/O.** `internal/money` and `internal/ledger` as pure Go, with
in-memory repositories. Property tests P1–P5 pass. No database, no HTTP.
*Exit criterion: the invariants are proven before a single row exists.*

**Phase 1 — Persistence & concurrency.** Migrations, `sqlc`, `TxManager`, the balance
trigger, `SELECT FOR UPDATE` ordering. The §6.3 concurrency test passes under `-race`
in both isolation modes.
*Exit criterion: money is provably conserved under 32,000 concurrent operations.*

**Phase 2 — HTTP API & idempotency.** Router, auth, error envelope, pagination,
idempotency middleware with recovery points. Full §11.4 suite green.

**Phase 2.5 — Authorize/capture.** Pending entries, partial capture, void, expiry sweep.

**Phase 3 — Events & webhooks.** Outbox, dispatcher, delivery workers, signing,
backoff, DLQ, replay, SSRF hardening. Reference verifier published in `pkg/`.

**Phase 4 — Operational polish.** Reconciler, reaper, metrics, tracing, graceful
shutdown, OpenAPI spec, `docker-compose.yml`, a `k6` load profile, and a README that
opens with the invariants rather than the endpoint list.

---

## 14. Open Questions / Deferred

| # | Question | Current lean |
|---|---|---|
| Q1 | Multi-currency / FX | Defer. When needed: two single-currency transactions linked by an `fx_conversions` row, with an `fx_gain_loss` account absorbing rounding. Never a mixed-currency transaction. |
| Q2 | Hash-chain the entry log (`prev_hash`) for tamper evidence | Attractive and cheap (`sha256(prev_hash‖entry)`), but serializes all inserts globally. Defer; revisit as a per-account chain, which keeps the ordering local. |
| Q3 | Balance sharding for hot accounts | Schema reserved (`accounts.shard_of`), unimplemented. Trigger: `ledger_lock_wait_seconds` p99 > 50ms. |
| Q4 | Retention / partitioning of `entries` | Monthly `RANGE` partition on `created_at` once row count justifies it. Decide before the ledger passes ~50M rows. |
| Q5 | Ordered per-endpoint delivery | Explicitly rejected for MVP (§8.7). Would be opt-in per endpoint if a merchant demands it. |
| Q6 | Webhook secret storage | AES-GCM with a key from env in MVP; KMS/envelope encryption if this ever runs anywhere real. |
| Q7 | Rate limiting | Per-API-key token bucket in-process for MVP. Correct only for a single replica — needs Postgres or Redis backing when scaled out. Documented as a known limitation rather than quietly wrong. |

---

## 15. Why These Choices Read Well to a Payments Team

Stated plainly, because the project has an audience:

1. **The dual-write problem is solved, not sidestepped.** Outbox in the same
   transaction is the answer, and the doc says why publishing after commit is wrong.
2. **Idempotency has recovery points**, not just a key/response cache. That distinction
   is the difference between having read the blog post and having understood it.
3. **Deadlock is prevented structurally** (lock ordering), not handled by retry.
4. **Full jitter is justified by the thundering-herd failure**, not cargo-culted.
5. **Delivery semantics are stated as a contract** — at-least-once, unordered — instead
   of an exactly-once claim that cannot be honored.
6. **The invariants are enforced three times** and the reconciler is a pager, not a
   dashboard.
7. **The numbers in this document were computed, not estimated** — the fee table, the
   allocation results, and the 35.4h / 17.7h retry windows are all outputs of executed
   code, including the `amount=1` case where the fee exceeds the charge.
8. **What is out of scope is written down** with reasons, which is a stronger signal
   than pretending the scope was unlimited.
