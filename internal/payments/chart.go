// Package payments composes ledger transactions into charges, authorizations, captures
// and refunds.
//
// Nothing about a payment is special to the ledger. A charge is a balanced transaction,
// an authorization is a pending one, a refund is a reversal — payments only decides which
// accounts to touch and for how much, and Post enforces that the result balances.
//
// The public state of a payment is derived from those ledger facts rather than stored in
// a mutable column of its own, so it cannot disagree with the books.
package payments

import (
	"context"
	"fmt"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/money"
)

// The conventional account names a merchant's chart uses.
const (
	AccountClearing   = "platform_clearing"
	AccountPayable    = "merchant_payable"
	AccountFeeRevenue = "fee_revenue"
	AccountHolds      = "authorization_holds"
)

// Chart is the set of accounts a payment moves money between.
type Chart struct {
	// Clearing is an asset: money the platform is holding.
	Clearing string
	// Payable is a liability: money owed to the merchant.
	Payable string
	// FeeRevenue is revenue: the platform's cut.
	FeeRevenue string
	// Holds is a liability: the contra account for uncaptured authorizations.
	Holds string

	Currency money.Currency
}

// chartSpec describes one account to resolve or create.
type chartSpec struct {
	name string
	typ  ledger.AccountType
	// allowNegative is set on the platform's own accounts, which go negative in the
	// ordinary course of business. A merchant-facing balance never should.
	allowNegative bool
}

var chartSpecs = []chartSpec{
	{AccountClearing, ledger.Asset, true},
	{AccountPayable, ledger.Liability, true},
	{AccountFeeRevenue, ledger.Revenue, true},
	{AccountHolds, ledger.Liability, true},
}

// AccountName qualifies a conventional account name with its currency.
//
// Account names are unique per merchant, not per merchant and currency, so an
// unqualified name would make a merchant's second currency collide with its first. A
// transaction is single-currency by invariant I2, so every currency needs its own chart
// regardless.
func AccountName(base string, currency money.Currency) string {
	return base + ":" + string(currency)
}

// EnsureChart resolves a merchant's payment accounts for one currency, creating any that
// do not exist.
//
// It is idempotent: an account that is already present is reused, so calling this on
// every startup — or on the first payment for a new merchant — is safe. Account names are
// unique per merchant in the database, so a race between two callers ends with one of
// them reusing the other's account rather than with a duplicate.
func EnsureChart(ctx context.Context, svc ledger.Service, merchantID string, currency money.Currency) (*Chart, error) {
	if !currency.Valid() {
		return nil, fmt.Errorf("%w: %q", money.ErrUnknownCurrency, string(currency))
	}

	existing, err := svc.ListAccounts(ctx, merchantID)
	if err != nil {
		return nil, err
	}

	byName := make(map[string]*ledger.Account, len(existing))
	for _, a := range existing {
		byName[a.Name] = a
	}

	chart := &Chart{Currency: currency}
	for _, spec := range chartSpecs {
		name := AccountName(spec.name, currency)

		account, ok := byName[name]
		if !ok {
			account, err = svc.CreateAccount(ctx, &ledger.Account{
				MerchantID:    merchantID,
				Name:          name,
				Type:          spec.typ,
				Currency:      currency,
				AllowNegative: spec.allowNegative,
			})
			if err != nil {
				return nil, fmt.Errorf("payments: creating %s: %w", name, err)
			}
		}

		switch spec.name {
		case AccountClearing:
			chart.Clearing = account.ID
		case AccountPayable:
			chart.Payable = account.ID
		case AccountFeeRevenue:
			chart.FeeRevenue = account.ID
		case AccountHolds:
			chart.Holds = account.ID
		}
	}

	return chart, nil
}

// Valid reports whether every account is populated.
func (c *Chart) Valid() bool {
	return c != nil && c.Clearing != "" && c.Payable != "" && c.FeeRevenue != "" && c.Holds != ""
}
