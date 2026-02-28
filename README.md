# ledgerd

A double-entry ledger and payments API in Go, backed by PostgreSQL.

The point of this project is not feature surface. It is that the hard parts of payments
infrastructure — **correctness under concurrency, exactly-once semantics over an
at-least-once world, and auditability** — are implemented and proven by tests rather
than described in comments.

## The invariants

Everything else in this repository exists to serve these. They are enforced in the
domain, in the database, and by a background reconciler that pages rather than charts.

| # | Invariant | Domain | DB | Reconciler |
|---|---|---|---|---|
| I1 | `Σ debits == Σ credits` per transaction | ✓ | ✓ trigger | ✓ |
| I2 | Single currency per transaction | ✓ | ✓ trigger | ✓ |
| I3 | `amount_minor > 0` | ✓ | ✓ CHECK | — |
| I4 | Entries are immutable | — | ✓ RULE | ✓ |
| I5 | `normal_balance` matches account `type` | ✓ | ✓ CHECK | — |
| I6 | Balance ≥ 0 unless `allow_negative` | ✓ in-lock | — | ✓ |
| I7 | `account_balances` == `SUM(entries)` | — | — | ✓ **P0** |
| I8 | Global trial balance sums to zero | — | — | ✓ **P0** |
| I9 | Accounting equation holds | — | — | ✓ |

> Every cent that enters the system is accounted for twice, every request can be safely
> retried, and every state change is eventually delivered exactly once to the outside
> world.

The three layers are not redundancy for its own sake. The domain gives a caller a precise
error. The database makes an unbalanced write impossible from *any* path — including a
migration script or a manual `psql` session, neither of which goes through the Go
validation. The reconciler catches what both somehow missed, because neither of them can
notice that a balance drifted three weeks ago.

## What the tests actually prove

### Money is conserved under concurrency

The headline test is 64 goroutines × 500 iterations of random transfers across 16
accounts, roughly a fifth of them deliberately overdrawing, run under `-race`. It asserts
seven things afterwards: money is conserved, no account illegally went negative, every
transaction balanced, the materialized balances still agree with the entry log, every
attempt is accounted for exactly once, **every entry's `balance_after` replays to the
final balance in id order**, and no deadlock occurred.

The replay is the assertion that earns its keep. A lost update can leave the endpoint
balances looking entirely plausible while the sequence that produced them is impossible.

### Ordered locking beats SERIALIZABLE, measured

The same body runs under both isolation modes. Over 32,000 attempts:

| Isolation | Committed | Refused | Serialization aborts |
|---|---:|---:|---:|
| READ COMMITTED + ordered row locks | 25,508 | 6,492 | **0** |
| SERIALIZABLE | 3,807 | 1,236 | **26,957** |

An 84% abort rate is what "the abort rate approaches 100% under contention" looks like
with real numbers attached. Sorting account ids before `SELECT ... FOR UPDATE` gives a
total order on lock acquisition, which makes deadlock **structurally impossible** rather
than something to recover from by retrying. Contenders queue and make progress instead of
aborting.

Both modes must still reach a consistent final state. If they ever disagree, the locking
strategy has a bug.

### The database enforces the invariants without the application

The integration suite goes *around* the application to test the layer that exists to
catch what the application got wrong. Raw SQL inserts an unbalanced transaction and a
mixed-currency one; both are refused at `COMMIT` by the deferred constraint trigger.
`UPDATE` and `DELETE` against `entries` affect zero rows.

### A retried request cannot move money twice

Fifty concurrent retries of the same request produce exactly one execution; every other
attempt is a replay or a conflict, with no third outcome. The proof that matters is
stronger than that: the response is recorded *in the same transaction as the ledger
entries*, so rolling that transaction back leaves neither the money nor the response.
Not "the ledger rolled back" and not "the response rolled back" — that they cannot
disagree.

### Delivery survives an outage without a thundering herd

Backoff is exponential with **full jitter** — a uniform draw across the whole interval,
not the interval plus a little noise. When an endpoint recovers, thousands of deliveries
come due together, and anything less lets them arrive as a herd and knock it back over.
The schedule produces a **35.4 hour** worst-case window and a **17.7 hour** expected one;
both are computed from the schedule and asserted, so the numbers cannot drift from the
code.

Four workers against one queue deliver 25 events exactly 25 times.

## Foundational decisions

- **Money is `int64` minor units + ISO-4217 currency. No floats, ever.** There is no
  `Div`; the only splitting primitive is `Allocate`, which uses the largest-remainder
  method and guarantees `sum(parts) == total` exactly.
- **Entries are append-only.** Corrections are compensating transactions, so history is
  a fact rather than a mutable projection.
- **Pessimistic row locking with deterministic lock ordering**, not `SERIALIZABLE`.
- **Postgres is the queue.** `SELECT ... FOR UPDATE SKIP LOCKED` gives a correct work
  queue with the same transactional guarantees as the ledger writes — which is exactly
  what the transactional outbox needs. A broker would *weaken* the guarantee by
  reintroducing the dual-write problem it was meant to solve.
- **A payment's public state is derived from the ledger**, never stored in a mutable
  column, so it cannot disagree with the books.

See [DESIGN.md](DESIGN.md) for the full rationale, including what is deliberately out
of scope and why.

## Layout

```
cmd/ledgerd      API server
cmd/workerd      dispatcher + delivery + lease sweeper + reconciler + reaper
internal/
  money          Money, Currency, exponents, Allocate, MulBps
  ledger         domain: Account, Transaction, Entry, invariants, Service
  payments       Authorize/Capture/Refund orchestration over ledger
  idempotency    Store, middleware, recovery points
  events         event types, transactional outbox
  webhooks       dispatcher, delivery worker, signer, backoff, SSRF guard
  httpapi        router, handlers, error envelope, pagination, auth
  postgres       pgxpool, TxManager, sqlc-generated queries
  workers        reconciler, reaper
  platform       config, log, id, metrics
pkg/webhookverify  reference signature verifier, published for consumers
migrations       goose .sql files, embedded via embed.FS
test/integration testcontainers suites, incl. the concurrency proof
```

Dependency direction is strictly inward: `httpapi → payments → ledger → money`.
`ledger` imports nothing from `postgres`; it declares the repository interfaces it
needs and `postgres` implements them. That is what makes the domain testable without
Docker, and why the property suite runs in milliseconds.

## Requirements

- Go 1.25.7+ — see the note below
- PostgreSQL 16 (Docker is enough — `make db-up`)
- Docker, for the integration suite

> **On the Go version:** the design doc pins Go 1.23. That is no longer reachable —
> current `pgx` requires 1.25.0 and `goose` requires 1.25.7. Pinning year-old
> dependencies to preserve the older floor is the worse trade with `govulncheck` in CI,
> so the floor moved and the CI matrix moved with it.

## Getting started

```bash
make db-up
```

```bash
make run
```

Migrations are applied at startup, so there is no separate step for local work.

## Testing

```bash
make test-race
```

```bash
make test-integration
```

The full suite is unit + property tests (no Docker, milliseconds), integration tests
against real Postgres via testcontainers, and fuzz targets over the two parsers that sit
on an untrusted boundary. The database is never mocked: the entire design rests on
Postgres locking semantics, and a mock would only validate the mock.

```bash
make cover
```

CI runs `-race` on every job, checks that the committed `sqlc` output still matches its
SQL, runs the integration suite against both Postgres 15 and 16, and gates coverage at
85%.

The gate spans **both** suites, merged. Large parts of `internal/postgres` are only
reachable with a real database, so a unit-only number understates the project by roughly
twenty points — and would create pressure to mock the database to make the number go up,
which is precisely what this codebase refuses to do. Generated `sqlc` output is excluded:
covering it would measure the generator. Combined coverage currently sits at **86.4%**.

## Delivery semantics

**At-least-once, unordered.** Stated as a contract rather than implied:

- Consumers **must** dedupe on `event.id`.
- Consumers **must not** assume ordering. Order by `event.created_at` and treat handlers
  as commutative.
- Exactly-once delivery is not achievable across a network partition. Claiming it would
  push the problem onto consumers silently instead of loudly.

Signatures are Stripe-compatible, so verification code already written against Stripe
works unmodified. [`pkg/webhookverify`](pkg/webhookverify) is the reference
implementation — getting verification subtly wrong is easy and every mistake is silent.

## Known limitations

Written down rather than left to be discovered:

- **Rate limiting is in-process**, so it is correct only for a single replica. With N
  replicas a caller gets N times the intended allowance. Fixing it means moving the
  counter into Postgres or Redis.
- **Webhook secrets are encrypted with a key from the environment.** A KMS with envelope
  encryption is the right answer anywhere real.
- **Migrations run at API startup.** Fine for single-writer; a multi-replica rollout
  should move them to a job so replicas do not race.
- **Balance sharding for hot accounts is designed but not built.** The schema reserves
  `accounts.shard_of`. The trigger to build it is `ledger_lock_wait_seconds` p99 > 50ms.
- **No FX.** A transaction is single-currency by invariant. Cross-currency would be two
  transactions linked by an `fx_conversions` row, never one mixed transaction.

## License

MIT
