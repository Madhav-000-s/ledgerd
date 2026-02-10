package httpapi

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/money"
)

// accountResponse is the wire form of an account.
//
// Every response carries an "object" discriminator so a client can dispatch on the type
// of a value without inferring it from the endpoint it came from.
type accountResponse struct {
	Object        string         `json:"object"`
	ID            string         `json:"id"`
	MerchantID    string         `json:"merchant_id"`
	Name          string         `json:"name"`
	Type          string         `json:"type"`
	NormalBalance string         `json:"normal_balance"`
	Currency      string         `json:"currency"`
	AllowNegative bool           `json:"allow_negative"`
	Metadata      map[string]any `json:"metadata"`
	CreatedAt     string         `json:"created_at"`
}

func newAccountResponse(a *ledger.Account) accountResponse {
	metadata := a.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	return accountResponse{
		Object:        "account",
		ID:            a.ID,
		MerchantID:    a.MerchantID,
		Name:          a.Name,
		Type:          string(a.Type),
		NormalBalance: string(a.NormalBalance),
		Currency:      string(a.Currency),
		AllowNegative: a.AllowNegative,
		Metadata:      metadata,
		CreatedAt:     a.CreatedAt.UTC().Format(rfc3339Milli),
	}
}

const rfc3339Milli = "2006-01-02T15:04:05.000Z07:00"

// balanceResponse exposes available as a computed field.
//
// It is never stored. A third number that could drift away from posted and pending is
// exactly the kind of thing the reconciler would eventually page someone about.
type balanceResponse struct {
	Object    string `json:"object"`
	AccountID string `json:"account_id"`
	Currency  string `json:"currency"`
	Posted    int64  `json:"posted"`
	Pending   int64  `json:"pending"`
	Available int64  `json:"available"`
	UpdatedAt string `json:"updated_at"`
}

func newBalanceResponse(b ledger.Balance) balanceResponse {
	return balanceResponse{
		Object:    "balance",
		AccountID: b.AccountID,
		Currency:  string(b.Currency),
		Posted:    b.Posted,
		Pending:   b.Pending,
		Available: b.Available(),
		UpdatedAt: b.UpdatedAt.UTC().Format(rfc3339Milli),
	}
}

type entryResponse struct {
	Object        string `json:"object"`
	ID            int64  `json:"id"`
	TransactionID string `json:"transaction_id"`
	AccountID     string `json:"account_id"`
	Direction     string `json:"direction"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Status        string `json:"status"`
	Seq           int16  `json:"seq"`
	BalanceAfter  int64  `json:"balance_after"`
	CreatedAt     string `json:"created_at"`
}

func newEntryResponse(e ledger.Entry) entryResponse {
	return entryResponse{
		Object:        "entry",
		ID:            e.ID,
		TransactionID: e.TransactionID,
		AccountID:     e.AccountID,
		Direction:     string(e.Direction),
		Amount:        e.Amount,
		Currency:      string(e.Currency),
		Status:        string(e.Status),
		Seq:           e.Seq,
		BalanceAfter:  e.BalanceAfter,
		CreatedAt:     e.CreatedAt.UTC().Format(rfc3339Milli),
	}
}

type transactionResponse struct {
	Object      string          `json:"object"`
	ID          string          `json:"id"`
	MerchantID  string          `json:"merchant_id"`
	Status      string          `json:"status"`
	Currency    string          `json:"currency"`
	Description string          `json:"description"`
	ReversesID  string          `json:"reverses_id,omitempty"`
	ExternalRef string          `json:"external_ref,omitempty"`
	Metadata    map[string]any  `json:"metadata"`
	Entries     []entryResponse `json:"entries"`
	CreatedAt   string          `json:"created_at"`
	PostedAt    string          `json:"posted_at,omitempty"`
}

func newTransactionResponse(t *ledger.Transaction) transactionResponse {
	entries := make([]entryResponse, 0, len(t.Entries))
	for _, e := range t.Entries {
		entries = append(entries, newEntryResponse(e))
	}

	metadata := t.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}

	out := transactionResponse{
		Object:      "transaction",
		ID:          t.ID,
		MerchantID:  t.MerchantID,
		Status:      string(t.Status),
		Currency:    string(t.Currency),
		Description: t.Description,
		ReversesID:  t.ReversesID,
		ExternalRef: t.ExternalRef,
		Metadata:    metadata,
		Entries:     entries,
		CreatedAt:   t.CreatedAt.UTC().Format(rfc3339Milli),
	}
	if t.PostedAt != nil {
		out.PostedAt = t.PostedAt.UTC().Format(rfc3339Milli)
	}
	return out
}

// ── handlers ────────────────────────────────────────────────────────────────

type createAccountRequest struct {
	Name          string         `json:"name"`
	Type          string         `json:"type"`
	Currency      string         `json:"currency"`
	AllowNegative bool           `json:"allow_negative"`
	Metadata      map[string]any `json:"metadata"`
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	auth, _ := AuthFrom(r.Context())

	var req createAccountRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}

	currency, err := money.ParseCurrency(req.Currency)
	if err != nil {
		writeError(w, r, err)
		return
	}

	var account *ledger.Account
	err = s.tx.InTx(r.Context(), func(ctx context.Context) error {
		var err error
		account, err = s.ledger.CreateAccount(ctx, &ledger.Account{
			// The merchant comes from the API key, never from the body. Taking it from
			// the request would let any authenticated caller write into another
			// tenant's chart of accounts.
			MerchantID:    auth.MerchantID,
			Name:          req.Name,
			Type:          ledger.AccountType(req.Type),
			Currency:      currency,
			AllowNegative: req.AllowNegative,
			Metadata:      req.Metadata,
		})
		return err
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusCreated, newAccountResponse(account))
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	account, err := s.authorizedAccount(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newAccountResponse(account))
}

func (s *Server) handleGetBalance(w http.ResponseWriter, r *http.Request) {
	account, err := s.authorizedAccount(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	balance, err := s.ledger.Balance(r.Context(), account.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newBalanceResponse(balance))
}

func (s *Server) handleListEntries(w http.ResponseWriter, r *http.Request) {
	account, err := s.authorizedAccount(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	page, err := pageFrom(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	entries, next, err := s.ledger.ListEntries(r.Context(), account.ID, page)
	if err != nil {
		writeError(w, r, err)
		return
	}

	data := make([]entryResponse, 0, len(entries))
	for _, e := range entries {
		data = append(data, newEntryResponse(e))
	}
	writeJSON(w, r, http.StatusOK, newListResponse(data, next))
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	auth, _ := AuthFrom(r.Context())

	accounts, err := s.ledger.ListAccounts(r.Context(), auth.MerchantID)
	if err != nil {
		writeError(w, r, err)
		return
	}

	data := make([]accountResponse, 0, len(accounts))
	for _, a := range accounts {
		data = append(data, newAccountResponse(a))
	}
	writeJSON(w, r, http.StatusOK, newListResponse(data, ""))
}

func (s *Server) handleGetTransaction(w http.ResponseWriter, r *http.Request) {
	auth, _ := AuthFrom(r.Context())

	txn, err := s.ledger.GetTransaction(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if txn.MerchantID != auth.MerchantID {
		// Report another tenant's transaction as missing rather than forbidden. A 403
		// would confirm the id exists, which is enough to enumerate them.
		writeError(w, r, ledger.ErrTransactionNotFound)
		return
	}

	writeJSON(w, r, http.StatusOK, newTransactionResponse(txn))
}

// authorizedAccount loads the account named in the path and confirms it belongs to the
// authenticated merchant.
//
// A cross-tenant read is reported as not found rather than forbidden, for the same
// reason as above: a distinguishable 403 turns the endpoint into an existence oracle.
func (s *Server) authorizedAccount(r *http.Request) (*ledger.Account, error) {
	auth, ok := AuthFrom(r.Context())
	if !ok {
		return nil, errUnauthorized
	}

	account, err := s.ledger.GetAccount(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		return nil, err
	}
	if account.MerchantID != auth.MerchantID {
		return nil, ledger.ErrAccountNotFound
	}
	return account, nil
}
