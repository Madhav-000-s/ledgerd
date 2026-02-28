package postgres

import (
	"context"
	"fmt"

	"github.com/Madhav-000-s/ledgerd/internal/postgres/sqlc"
	"github.com/Madhav-000-s/ledgerd/internal/workers"
)

// LedgerChecker implements workers.LedgerChecker over the aggregate queries.
type LedgerChecker struct{ db *DB }

var _ workers.LedgerChecker = (*LedgerChecker)(nil)

// NewLedgerChecker returns the reconciler's view of the ledger.
func NewLedgerChecker(db *DB) *LedgerChecker { return &LedgerChecker{db: db} }

func (c *LedgerChecker) q(ctx context.Context) *sqlc.Queries { return sqlc.New(c.db.q(ctx)) }

// TrialBalance restates every posted balance in debit-positive terms and sums it.
//
// Because balances are stored in each account's own normal direction, this single sum is
// both the trial balance and the accounting equation. It must be zero; anything else
// means money was created or destroyed.
func (c *LedgerChecker) TrialBalance(ctx context.Context, currency string) (int64, error) {
	drift, err := c.q(ctx).TrialBalance(ctx, currency)
	if err != nil {
		return 0, fmt.Errorf("postgres: trial balance: %w", err)
	}
	return drift, nil
}

// Currencies returns the currencies that have accounts.
func (c *LedgerChecker) Currencies(ctx context.Context) ([]string, error) {
	rows, err := c.q(ctx).ListCurrencies(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: list currencies: %w", err)
	}
	return rows, nil
}

// DriftedAccounts recomputes every account's balance from the entry log and returns the
// ones that disagree with what is materialized.
//
// This is the check nothing else can perform. The domain validates a transaction in
// isolation and the database validates it at commit, but neither can notice that a
// balance drifted three weeks ago — only replaying the log against the stored figure can.
func (c *LedgerChecker) DriftedAccounts(ctx context.Context) ([]workers.AccountDrift, error) {
	rows, err := c.q(ctx).DriftedAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: drifted accounts: %w", err)
	}

	out := make([]workers.AccountDrift, 0, len(rows))
	for _, row := range rows {
		out = append(out, workers.AccountDrift{
			AccountID:    row.AccountID,
			Materialized: row.Materialized,
			Derived:      row.Derived,
		})
	}
	return out, nil
}
