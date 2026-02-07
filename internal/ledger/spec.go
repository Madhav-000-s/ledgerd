package ledger

import (
	"fmt"

	"github.com/Madhav-000-s/ledgerd/internal/money"
)

// EntrySpec is one requested posting.
type EntrySpec struct {
	AccountID string
	Direction Direction
	Amount    money.Money
}

// TransactionSpec is a requested transaction. It is the only way to write to the
// ledger: Post is the single write primitive, and everything above this package —
// payments, refunds, fee splits — composes specs rather than touching entries.
type TransactionSpec struct {
	MerchantID  string
	Description string
	ExternalRef string
	Entries     []EntrySpec
	Pending     bool // true: entries land as holds, moving Pending rather than Posted
	Metadata    map[string]any
}

// Validate enforces the invariants that can be checked without touching the database:
// I1 (debits equal credits), I2 (one currency), and I3 (amounts are positive).
//
// This is the first of the three layers that enforce the balance invariant. The
// database constraint trigger is the second and the reconciler the third. The
// duplication is deliberate: this layer gives a caller a precise error, the trigger
// makes an unbalanced write impossible from any code path including a manual psql
// session, and the reconciler catches whatever both somehow missed.
func (s TransactionSpec) Validate() error {
	if s.MerchantID == "" {
		return fmt.Errorf("%w: merchant id is empty", ErrInvalidAccount)
	}
	if len(s.Entries) == 0 {
		return ErrNoEntries
	}

	var (
		debits   int64
		credits  int64
		currency money.Currency
	)

	for i, e := range s.Entries {
		if e.AccountID == "" {
			return fmt.Errorf("%w: entries[%d] has no account", ErrInvalidAccount, i)
		}
		if !e.Direction.Valid() {
			return fmt.Errorf("%w: entries[%d] direction %q", ErrInvalidDirection, i, e.Direction)
		}

		// I3: the sign of a posting lives in its direction. A zero or negative amount
		// would let the same movement be expressed two ways, and only one of them would
		// balance.
		amount := e.Amount.Amount()
		if amount <= 0 {
			return fmt.Errorf("%w: entries[%d] amount is %d", ErrInvalidAmount, i, amount)
		}

		// I2: one currency per transaction. Cross-currency movement is modelled as two
		// single-currency transactions linked by an FX record, never as one mixed
		// transaction, because a mixed transaction has no meaningful balance check.
		if i == 0 {
			currency = e.Amount.Currency()
			if !currency.Valid() {
				return fmt.Errorf("%w: %q", money.ErrUnknownCurrency, string(currency))
			}
		} else if e.Amount.Currency() != currency {
			return fmt.Errorf("%w: %s and %s", ErrMultiCurrency, currency, e.Amount.Currency())
		}

		// Sum with overflow detection: a wrapped total would balance against an equally
		// wrapped other side and pass the check while being nonsense.
		var ok bool
		if e.Direction == Debit {
			if debits, ok = addInt64(debits, amount); !ok {
				return fmt.Errorf("%w: debits overflow int64", money.ErrOverflow)
			}
		} else {
			if credits, ok = addInt64(credits, amount); !ok {
				return fmt.Errorf("%w: credits overflow int64", money.ErrOverflow)
			}
		}
	}

	// I1: the invariant the whole system exists to preserve.
	if debits != credits {
		return fmt.Errorf("%w: debits=%d credits=%d", ErrUnbalanced, debits, credits)
	}
	if debits == 0 {
		return fmt.Errorf("%w: all entries are zero", ErrNoEntries)
	}

	return nil
}

// Currency returns the transaction's single currency. It is only meaningful once
// Validate has passed.
func (s TransactionSpec) Currency() money.Currency {
	if len(s.Entries) == 0 {
		return ""
	}
	return s.Entries[0].Amount.Currency()
}

// AccountIDs returns the distinct accounts this spec touches, sorted ascending.
//
// The sort is not cosmetic. It is the total order in which locks are acquired, and a
// total order is what makes deadlock structurally impossible rather than something to
// be recovered from by retrying. Every caller that locks accounts must lock them in
// this order.
func (s TransactionSpec) AccountIDs() []string {
	seen := make(map[string]struct{}, len(s.Entries))
	ids := make([]string, 0, len(s.Entries))
	for _, e := range s.Entries {
		if _, dup := seen[e.AccountID]; dup {
			continue
		}
		seen[e.AccountID] = struct{}{}
		ids = append(ids, e.AccountID)
	}
	sortStrings(ids)
	return ids
}

// addInt64 adds with overflow detection.
func addInt64(a, b int64) (int64, bool) {
	s := a + b
	if (a > 0 && b > 0 && s < 0) || (a < 0 && b < 0 && s >= 0) {
		return 0, false
	}
	return s, true
}

// sortStrings is an insertion sort. Transactions touch a handful of accounts, so this
// beats pulling in sort.Strings' machinery and — more usefully — it is obviously a
// total order, which is the property the locking argument depends on.
func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
