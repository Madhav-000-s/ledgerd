//go:build integration

// Package integration exercises the ledger against a real PostgreSQL instance.
//
// The database is never mocked. The entire design rests on Postgres locking semantics,
// the deferred constraint trigger and the append-only rule; a mock would validate the
// mock and let every one of those bugs through. One container is started per package
// run and each test gets a freshly truncated schema.
package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/money"
	"github.com/Madhav-000-s/ledgerd/internal/platform/config"
	"github.com/Madhav-000-s/ledgerd/internal/postgres"
)

var (
	container *tcpostgres.PostgresContainer
	dsn       string
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	// CI runs the suite against both supported majors. The deferred constraint trigger
	// and SKIP LOCKED are the behaviours that would differ between them, and both are
	// load-bearing here.
	image := os.Getenv("LEDGERD_TEST_POSTGRES_IMAGE")
	if image == "" {
		image = "postgres:16-alpine"
	}

	var err error
	container, err = tcpostgres.Run(ctx,
		image,
		tcpostgres.WithDatabase("ledgerd"),
		tcpostgres.WithUsername("ledgerd"),
		tcpostgres.WithPassword("ledgerd"),
		// These match docker-compose so that a failure reproduces locally with the
		// same timeouts. A pathological lock wait must fail loudly rather than hang.
		testcontainers.WithEnv(map[string]string{"POSTGRES_HOST_AUTH_METHOD": "trust"}),
		testcontainers.WithCmd(
			"postgres",
			"-c", "lock_timeout=3s",
			"-c", "statement_timeout=10s",
			"-c", "max_connections=300",
			"-c", "log_lock_waits=on",
			"-c", "deadlock_timeout=200ms",
		),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting postgres container: %v\n", err)
		os.Exit(1)
	}

	dsn, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		_ = testcontainers.TerminateContainer(container)
		os.Exit(1)
	}

	code := m.Run()

	if err := testcontainers.TerminateContainer(container); err != nil {
		fmt.Fprintf(os.Stderr, "terminating container: %v\n", err)
	}
	os.Exit(code)
}

// env is one test's database handle plus the wiring above it.
type env struct {
	t    *testing.T
	ctx  context.Context
	db   *postgres.DB
	txm  *postgres.TxManager
	repo ledger.Repository
	svc  ledger.Service
}

// dbConfig returns a config pointed at the shared container.
func dbConfig() config.DBConfig {
	return config.DBConfig{
		URL:              dsn,
		MaxConns:         40,
		MinConns:         2,
		MaxConnLifetime:  time.Hour,
		MaxConnIdleTime:  30 * time.Minute,
		ConnectTimeout:   10 * time.Second,
		LockTimeout:      3 * time.Second,
		StatementTimeout: 10 * time.Second,
	}
}

// newEnv opens a pool, applies migrations and truncates every table, so each test sees
// an empty ledger without paying for a fresh container.
func newEnv(t *testing.T, isolation config.Isolation) *env {
	t.Helper()

	ctx := context.Background()

	db, err := postgres.Open(ctx, dbConfig())
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(db.Close)

	if err := postgres.Migrate(ctx, db); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	truncateAll(t, db.Pool())

	repo := postgres.NewLedgerRepo(db)
	return &env{
		t:    t,
		ctx:  ctx,
		db:   db,
		txm:  postgres.NewTxManager(db, isolation),
		repo: repo,
		svc:  ledger.NewService(repo),
	}
}

// inTx runs fn inside a database transaction. Every ledger write goes through it: the
// service composes several repository calls, and they are only atomic together.
func (e *env) inTx(fn func(ctx context.Context) error) error {
	return e.txm.InTx(e.ctx, fn)
}

// post writes a transaction and fails the test if it does not commit.
func (e *env) post(spec ledger.TransactionSpec) *ledger.Transaction {
	e.t.Helper()

	txn, err := e.tryPost(spec)
	if err != nil {
		e.t.Fatalf("Post = %v", err)
	}
	return txn
}

// tryPost writes a transaction and returns the error for the caller to assert on.
func (e *env) tryPost(spec ledger.TransactionSpec) (*ledger.Transaction, error) {
	var txn *ledger.Transaction
	err := e.inTx(func(ctx context.Context) error {
		var err error
		txn, err = e.svc.Post(ctx, spec)
		return err
	})
	if err != nil {
		return nil, err
	}
	return txn, nil
}

// account adds an account to the chart of accounts.
func (e *env) account(name string, typ ledger.AccountType, currency money.Currency, allowNegative bool) *ledger.Account {
	e.t.Helper()

	var a *ledger.Account
	err := e.inTx(func(ctx context.Context) error {
		var err error
		a, err = e.svc.CreateAccount(ctx, &ledger.Account{
			MerchantID:    testMerchant,
			Name:          name,
			Type:          typ,
			Currency:      currency,
			AllowNegative: allowNegative,
		})
		return err
	})
	if err != nil {
		e.t.Fatalf("CreateAccount(%s) = %v", name, err)
	}
	return a
}

// balance reads an account's materialized balance.
func (e *env) balance(accountID string) ledger.Balance {
	e.t.Helper()

	b, err := e.svc.Balance(e.ctx, accountID)
	if err != nil {
		e.t.Fatalf("Balance(%s) = %v", accountID, err)
	}
	return b
}

// derivedBalance recomputes an account's posted balance from the entry log, which is
// the source of truth. Comparing the two is invariant I7.
func (e *env) derivedBalance(accountID string) int64 {
	e.t.Helper()

	var derived int64
	err := e.db.Pool().QueryRow(e.ctx, `
        SELECT COALESCE(SUM(
                   CASE WHEN e.direction::text = a.normal_balance::text THEN e.amount_minor
                        ELSE -e.amount_minor
                   END
               ), 0)::bigint
          FROM entries e
          JOIN accounts a ON a.id = e.account_id
         WHERE e.account_id = $1
           AND e.status = 'posted'`, accountID).Scan(&derived)
	if err != nil {
		e.t.Fatalf("deriving balance for %s: %v", accountID, err)
	}
	return derived
}

const testMerchant = "mer_integration"

func usd(n int64) money.Money { return money.MustNew(n, money.USD) }

func entry(accountID string, d ledger.Direction, n int64) ledger.EntrySpec {
	return ledger.EntrySpec{AccountID: accountID, Direction: d, Amount: usd(n)}
}

// truncateAll empties every table between tests. RESTART IDENTITY resets the entry
// sequence so that assertions about id ordering start from a known point.
//
// Every table has to be listed. A table left out leaks state into the next test, and the
// symptom is a dispatcher claiming events the test never emitted — which looks like a
// bug in the code under test rather than in the harness.
func truncateAll(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	// entries is append-only for the application role, but TRUNCATE is DDL and is not
	// intercepted by the DO INSTEAD NOTHING rules, which is what makes a clean slate
	// possible without dropping the schema.
	_, err := pool.Exec(context.Background(), `
        TRUNCATE
            webhook_attempts,
            webhook_deliveries,
            webhook_secrets,
            webhook_endpoints,
            events,
            idempotency_keys,
            api_keys,
            entries,
            account_balances,
            transactions,
            accounts
        RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncating: %v", err)
	}
}
