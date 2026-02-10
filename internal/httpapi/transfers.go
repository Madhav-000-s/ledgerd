package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/money"
)

// transferEntry is one requested posting on the wire.
//
// Amounts are integers in minor units, matching the ledger. There is no decimal string
// and no float: a JSON number that has been through a float64 parser has already lost
// the argument, and this is the boundary where that would happen.
type transferEntry struct {
	AccountID string `json:"account_id"`
	Direction string `json:"direction"`
	Amount    int64  `json:"amount"`
}

type createTransferRequest struct {
	Currency    string          `json:"currency"`
	Description string          `json:"description"`
	ExternalRef string          `json:"external_ref"`
	Pending     bool            `json:"pending"`
	Entries     []transferEntry `json:"entries"`
	Metadata    map[string]any  `json:"metadata"`
}

// handleCreateTransfer posts a raw balanced movement.
//
// This is the ledger's primitive exposed directly: the caller supplies both sides and
// the service enforces that they balance. Higher-level shapes — charges, refunds, fee
// splits — compose it rather than bypass it.
func (s *Server) handleCreateTransfer(w http.ResponseWriter, r *http.Request) {
	auth, _ := AuthFrom(r.Context())

	var req createTransferRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}

	if len(req.Entries) == 0 {
		writeError(w, r, toAPIError(ledger.ErrNoEntries))
		return
	}

	currency, err := money.ParseCurrency(req.Currency)
	if err != nil {
		writeError(w, r, err)
		return
	}

	entries := make([]ledger.EntrySpec, 0, len(req.Entries))
	for i, e := range req.Entries {
		direction := ledger.Direction(e.Direction)
		if !direction.Valid() {
			writeError(w, r, Errorf(http.StatusBadRequest, TypeInvalidRequest, "invalid_direction",
				`direction must be "debit" or "credit".`).WithParam(entryParam(i, "direction")))
			return
		}

		amount, err := money.New(e.Amount, currency)
		if err != nil {
			writeError(w, r, err)
			return
		}

		entries = append(entries, ledger.EntrySpec{
			AccountID: e.AccountID,
			Direction: direction,
			Amount:    amount,
		})
	}

	// Every account named must belong to the caller. Without this a merchant could post
	// entries against another tenant's accounts — the transaction would balance, and the
	// books of two merchants would both be wrong.
	if err := s.assertAccountsBelongTo(r.Context(), auth.MerchantID, entries); err != nil {
		writeError(w, r, err)
		return
	}

	var txn *ledger.Transaction
	err = s.tx.InTx(r.Context(), func(ctx context.Context) error {
		var err error
		txn, err = s.ledger.Post(ctx, ledger.TransactionSpec{
			MerchantID:  auth.MerchantID,
			Description: req.Description,
			ExternalRef: req.ExternalRef,
			Entries:     entries,
			Pending:     req.Pending,
			Metadata:    req.Metadata,
		})
		return err
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusCreated, newTransactionResponse(txn))
}

type reverseTransactionRequest struct {
	Reason string `json:"reason"`
}

// handleReverseTransaction posts the mirror image of a transaction.
//
// There is no endpoint that edits or deletes one. A correction is always a new
// transaction, which is what keeps the history of both the mistake and its correction
// permanent.
func (s *Server) handleReverseTransaction(w http.ResponseWriter, r *http.Request) {
	auth, _ := AuthFrom(r.Context())
	txnID := chi.URLParam(r, "id")

	var req reverseTransactionRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, r, err)
			return
		}
	}

	original, err := s.ledger.GetTransaction(r.Context(), txnID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if original.MerchantID != auth.MerchantID {
		writeError(w, r, ledger.ErrTransactionNotFound)
		return
	}

	var reversal *ledger.Transaction
	err = s.tx.InTx(r.Context(), func(ctx context.Context) error {
		var err error
		reversal, err = s.ledger.Reverse(ctx, txnID, req.Reason)
		return err
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusCreated, newTransactionResponse(reversal))
}

// assertAccountsBelongTo verifies every account in the spec is the caller's.
//
// A missing account and another tenant's account are both reported as not found, so the
// endpoint cannot be used to discover which account ids exist.
func (s *Server) assertAccountsBelongTo(ctx context.Context, merchantID string, entries []ledger.EntrySpec) error {
	seen := make(map[string]struct{}, len(entries))

	for _, e := range entries {
		if _, done := seen[e.AccountID]; done {
			continue
		}
		seen[e.AccountID] = struct{}{}

		account, err := s.ledger.GetAccount(ctx, e.AccountID)
		if err != nil {
			return err
		}
		if account.MerchantID != merchantID {
			return ledger.ErrAccountNotFound
		}
	}
	return nil
}

func entryParam(i int, field string) string {
	return "entries[" + strconv.Itoa(i) + "]." + field
}
