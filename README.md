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

## Foundational decisions

- **Money is `int64` minor units + ISO-4217 currency. No floats, ever.** There is no
  `Div`; the only splitting primitive is `Allocate`, which uses the largest-remainder
  method and guarantees `sum(parts) == total` exactly.
- **Entries are append-only.** Corrections are compensating transactions, so history is
  a fact rather than a mutable projection.
- **Pessimistic row locking with deterministic lock ordering**, not `SERIALIZABLE`.
  Sorting account IDs before `SELECT ... FOR UPDATE` makes deadlock structurally
  impossible; contenders queue instead of aborting.
- **Postgres is the queue.** `SELECT ... FOR UPDATE SKIP LOCKED` gives a correct work
  queue with the same transactional guarantees as the ledger writes — which is exactly
  what the transactional outbox needs. A broker would *weaken* the guarantee by
  reintroducing the dual-write problem.

See [DESIGN.md](DESIGN.md) for the full rationale, including what is deliberately out
of scope and why.

## Layout

```
cmd/ledgerd      API server
cmd/workerd      dispatcher + delivery + reconciler + reaper
internal/
  money          Money, Currency, exponents, Allocate, MulBps
  ledger         domain: Account, Transaction, Entry, invariants, Service
  payments       Authorize/Capture/Refund orchestration over ledger
  idempotency    Store, middleware, recovery points
  events         event types, outbox writer
  webhooks       dispatcher, delivery worker, signer, backoff
  httpapi        router, handlers, error envelope, pagination, auth
  postgres       pgxpool, TxManager, sqlc-generated queries
  platform       config, log, metrics, health
migrations       goose .sql files, embedded via embed.FS
test/            integration (testcontainers) and property (rapid) suites
```

Dependency direction is strictly inward: `httpapi → payments → ledger → money`.
`ledger` imports nothing from `postgres`; it declares the repository interfaces it
needs and `postgres` implements them. That is what makes the domain testable without
Docker.

## Requirements

- Go 1.23+
- PostgreSQL 16 (Docker is enough — `make db-up`)
- Docker, for the integration suite

## Getting started

```bash
make db-up        # start Postgres 16
make migrate      # apply migrations
make run          # start the API on :8080
```

## Testing

```bash
make test         # unit + property, no Docker, milliseconds
make test-race    # the same under -race
make test-integration   # testcontainers, real Postgres
make lint
```

The concurrency suite is the headline test: 64 goroutines × 500 iterations of random
transfers across 16 accounts, run under `-race` in both isolation modes, asserting that
money is conserved, that no account illegally went negative, and that every entry's
recorded `balance_after` replays to the final balance in order.

## Status

Under construction, built in phases. See §13 of [DESIGN.md](DESIGN.md).

## License

MIT
