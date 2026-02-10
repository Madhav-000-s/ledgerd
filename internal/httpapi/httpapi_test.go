package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/ledger/memory"
	"github.com/Madhav-000-s/ledgerd/internal/money"
	"github.com/Madhav-000-s/ledgerd/internal/platform/config"
)

// fakeKeyStore serves keys from a map keyed by lookup prefix.
type fakeKeyStore struct {
	byLookup map[string]*APIKeyRecord
	err      error
}

func (s *fakeKeyStore) FindByLookup(_ context.Context, lookup string) (*APIKeyRecord, error) {
	if s.err != nil {
		return nil, s.err
	}
	rec, ok := s.byLookup[lookup]
	if !ok {
		return nil, ErrAPIKeyNotFound
	}
	return rec, nil
}

// passthroughTx runs the closure directly. The in-memory repository has no transactions,
// and pretending otherwise would let a bug pass here that a real database would catch —
// which is why atomicity is asserted in the integration suite instead.
type passthroughTx struct{}

func (passthroughTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

type okHealth struct{}

func (okHealth) Ping(context.Context) error { return nil }

type harness struct {
	t      *testing.T
	server *Server
	store  *memory.Store
	svc    ledger.Service
	keys   *fakeKeyStore

	writeKey string
	readKey  string
	otherKey string // a second merchant, for isolation tests
}

const (
	merchantA = "mer_a"
	merchantB = "mer_b"
)

func newHarness(t *testing.T) *harness {
	t.Helper()

	store := memory.New()
	svc := ledger.NewService(store)

	keys := &fakeKeyStore{byLookup: map[string]*APIKeyRecord{}}
	h := &harness{t: t, store: store, svc: svc, keys: keys}

	h.writeKey = h.issueKey(merchantA, RoleWrite)
	h.readKey = h.issueKey(merchantA, RoleRead)
	h.otherKey = h.issueKey(merchantB, RoleWrite)

	h.server = NewServer(Options{
		Config: config.HTTPConfig{
			HandlerTimeout:  5 * time.Second,
			MaxBodyBytes:    1 << 20,
			RateLimitPerMin: 0, // disabled unless a test asks for it
		},
		Ledger: svc,
		Tx:     passthroughTx{},
		Keys:   keys,
		Health: okHealth{},
	})
	return h
}

func (h *harness) issueKey(merchantID string, role Role) string {
	h.t.Helper()

	secret, rec, err := GenerateAPIKey(merchantID, role, false)
	if err != nil {
		h.t.Fatalf("GenerateAPIKey = %v", err)
	}
	h.keys.byLookup[rec.Lookup] = rec
	return secret
}

// do issues a request and returns the response.
func (h *harness) do(method, path, key string, body any) *httptest.ResponseRecorder {
	h.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshalling request: %v", err)
		}
		reader = bytes.NewReader(raw)
	}

	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)
	return rec
}

// doRaw issues a request with a literal body, for malformed-input tests.
func (h *harness) doRaw(method, path, key, body string) *httptest.ResponseRecorder {
	h.t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()

	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding response: %v\nbody: %s", err, rec.Body.String())
	}
	return out
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) ErrorBody {
	t.Helper()
	return decodeBody[ErrorResponse](t, rec).Error
}

// createAccount adds an account and returns its id.
func (h *harness) createAccount(name, typ string, allowNegative bool) string {
	h.t.Helper()

	rec := h.do(http.MethodPost, "/v1/accounts", h.writeKey, map[string]any{
		"name":           name,
		"type":           typ,
		"currency":       "USD",
		"allow_negative": allowNegative,
	})
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("creating %s: status %d, body %s", name, rec.Code, rec.Body.String())
	}
	return decodeBody[accountResponse](h.t, rec).ID
}

// ── authentication ──────────────────────────────────────────────────────────

func TestAuthRequired(t *testing.T) {
	h := newHarness(t)

	rec := h.do(http.MethodGet, "/v1/accounts", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	body := decodeError(t, rec)
	if body.Type != TypeAuthentication {
		t.Errorf("type = %q, want %q", body.Type, TypeAuthentication)
	}
	if body.RequestID == "" {
		t.Error("error body has no request_id")
	}
}

// An unknown key, a revoked key and a wrong secret must be indistinguishable, or the
// endpoint becomes a way to discover which keys exist.
func TestAuthFailuresAreIndistinguishable(t *testing.T) {
	h := newHarness(t)

	revokedSecret, revokedRec, err := GenerateAPIKey(merchantA, RoleWrite, false)
	if err != nil {
		t.Fatalf("GenerateAPIKey = %v", err)
	}
	revokedRec.Revoked = true
	h.keys.byLookup[revokedRec.Lookup] = revokedRec

	// A key with a valid lookup prefix but the wrong secret body.
	wrongSecret := h.writeKey[:len(h.writeKey)-4] + "ZZZZ"

	cases := map[string]string{
		"unknown key":   "sk_test_UNKNOWNKEYVALUEHERE00000000000000",
		"revoked key":   revokedSecret,
		"wrong secret":  wrongSecret,
		"empty bearer":  "",
		"nonsense text": "not-a-key",
	}

	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			rec := h.do(http.MethodGet, "/v1/accounts", key, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := decodeError(t, rec).Code; got != "unauthorized" {
				t.Errorf("code = %q, want unauthorized", got)
			}
		})
	}
}

func TestAuthAcceptsBasicAuthUsername(t *testing.T) {
	h := newHarness(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/accounts", nil)
	req.SetBasicAuth(h.readKey, "")

	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
}

// A store failure is a server problem and must not be reported as an authentication
// failure, or an outage looks like a credential problem to every caller.
func TestAuthStoreFailureIsFiveHundred(t *testing.T) {
	h := newHarness(t)
	h.keys.err = context.DeadlineExceeded

	rec := h.do(http.MethodGet, "/v1/accounts", h.readKey, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := decodeError(t, rec).Type; got != TypeAPI {
		t.Errorf("type = %q, want %q", got, TypeAPI)
	}
}

func TestRoleEnforcement(t *testing.T) {
	h := newHarness(t)

	// A read key may read.
	if rec := h.do(http.MethodGet, "/v1/accounts", h.readKey, nil); rec.Code != http.StatusOK {
		t.Errorf("read key GET: status %d, want 200", rec.Code)
	}

	// A read key may not write.
	rec := h.do(http.MethodPost, "/v1/accounts", h.readKey, map[string]any{
		"name": "cash", "type": "asset", "currency": "USD",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read key POST: status %d, want 403", rec.Code)
	}
	if got := decodeError(t, rec).Code; got != "insufficient_permissions" {
		t.Errorf("code = %q", got)
	}
}

func TestRoleRanking(t *testing.T) {
	if !RoleAdmin.AtLeast(RoleWrite) || !RoleAdmin.AtLeast(RoleRead) {
		t.Error("admin does not outrank write/read")
	}
	if !RoleWrite.AtLeast(RoleRead) {
		t.Error("write does not outrank read")
	}
	if RoleRead.AtLeast(RoleWrite) {
		t.Error("read outranks write")
	}
	if Role("nonsense").Valid() {
		t.Error("an unknown role reported Valid")
	}
}

func TestGeneratedKeysAreDistinctAndVerifiable(t *testing.T) {
	secret, rec, err := GenerateAPIKey(merchantA, RoleWrite, true)
	if err != nil {
		t.Fatalf("GenerateAPIKey = %v", err)
	}

	if !strings.HasPrefix(secret, prefixLive) {
		t.Errorf("livemode key %q does not carry the live prefix", secret)
	}
	if lookupOf(secret) != rec.Lookup {
		t.Errorf("lookupOf(secret) = %q, record has %q", lookupOf(secret), rec.Lookup)
	}
	if !verifySecret(secret, rec.SecretHash) {
		t.Error("the generated secret does not verify against its own hash")
	}
	if verifySecret(secret+"x", rec.SecretHash) {
		t.Error("a modified secret verified")
	}

	// The lookup prefix must not be enough to reconstruct the key.
	if strings.Contains(string(rec.SecretHash), secret) {
		t.Error("the stored hash contains the secret")
	}

	other, _, err := GenerateAPIKey(merchantA, RoleWrite, true)
	if err != nil {
		t.Fatalf("GenerateAPIKey = %v", err)
	}
	if other == secret {
		t.Error("two generated keys are identical")
	}
}

// ── accounts ────────────────────────────────────────────────────────────────

func TestCreateAndReadAccount(t *testing.T) {
	h := newHarness(t)

	rec := h.do(http.MethodPost, "/v1/accounts", h.writeKey, map[string]any{
		"name": "platform_clearing", "type": "asset", "currency": "USD",
		"metadata": map[string]any{"team": "payments"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	created := decodeBody[accountResponse](t, rec)
	if created.Object != "account" {
		t.Errorf("object = %q, want account", created.Object)
	}
	if created.NormalBalance != "debit" {
		t.Errorf("normal_balance = %q, want debit (assets are debit-normal)", created.NormalBalance)
	}
	if created.MerchantID != merchantA {
		t.Errorf("merchant_id = %q, want %q", created.MerchantID, merchantA)
	}

	got := decodeBody[accountResponse](t, h.do(http.MethodGet, "/v1/accounts/"+created.ID, h.readKey, nil))
	if got.ID != created.ID {
		t.Errorf("GET returned id %q, want %q", got.ID, created.ID)
	}
	if got.Metadata["team"] != "payments" {
		t.Errorf("metadata not round-tripped: %+v", got.Metadata)
	}
}

// The merchant comes from the API key, never the body. Otherwise any authenticated
// caller could write into another tenant's chart of accounts.
func TestCreateAccountIgnoresMerchantInBody(t *testing.T) {
	h := newHarness(t)

	rec := h.doRaw(http.MethodPost, "/v1/accounts", h.writeKey,
		`{"name":"sneaky","type":"asset","currency":"USD","merchant_id":"mer_b"}`)

	// Unknown fields are rejected outright, which is the stronger guarantee: the field
	// cannot be silently ignored either.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Code; got != "malformed_json" {
		t.Errorf("code = %q, want malformed_json", got)
	}
}

func TestCreateAccountValidation(t *testing.T) {
	h := newHarness(t)

	tests := []struct {
		name     string
		body     map[string]any
		wantCode string
	}{
		{"unknown currency", map[string]any{"name": "a", "type": "asset", "currency": "XYZ"}, "unknown_currency"},
		{"unknown type", map[string]any{"name": "a", "type": "gizmo", "currency": "USD"}, "invalid_account_type"},
		{"no name", map[string]any{"name": "", "type": "asset", "currency": "USD"}, "invalid_account"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(http.MethodPost, "/v1/accounts", h.writeKey, tc.body)
			if rec.Code < 400 || rec.Code >= 500 {
				t.Fatalf("status = %d, want a 4xx; body %s", rec.Code, rec.Body.String())
			}
			if got := decodeError(t, rec).Code; got != tc.wantCode {
				t.Errorf("code = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

func TestDuplicateAccountName(t *testing.T) {
	h := newHarness(t)
	h.createAccount("cash", "asset", true)

	rec := h.do(http.MethodPost, "/v1/accounts", h.writeKey, map[string]any{
		"name": "cash", "type": "asset", "currency": "USD",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if got := decodeError(t, rec).Code; got != "account_exists" {
		t.Errorf("code = %q, want account_exists", got)
	}
}

// Another merchant's account must read as missing, not forbidden. A distinguishable 403
// confirms the id exists, which is enough to enumerate them.
func TestCrossTenantAccountReadsAsNotFound(t *testing.T) {
	h := newHarness(t)
	accountID := h.createAccount("cash", "asset", true)

	for _, path := range []string{
		"/v1/accounts/" + accountID,
		"/v1/accounts/" + accountID + "/balance",
		"/v1/accounts/" + accountID + "/entries",
	} {
		rec := h.do(http.MethodGet, path, h.otherKey, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestListAccountsIsScopedToMerchant(t *testing.T) {
	h := newHarness(t)
	h.createAccount("cash", "asset", true)
	h.createAccount("payable", "liability", true)

	mine := decodeBody[listResponse[accountResponse]](t, h.do(http.MethodGet, "/v1/accounts", h.readKey, nil))
	if len(mine.Data) != 2 {
		t.Errorf("got %d accounts, want 2", len(mine.Data))
	}
	if mine.Object != "list" {
		t.Errorf("object = %q, want list", mine.Object)
	}

	theirs := decodeBody[listResponse[accountResponse]](t, h.do(http.MethodGet, "/v1/accounts", h.otherKey, nil))
	if len(theirs.Data) != 0 {
		t.Errorf("the other merchant sees %d accounts, want 0", len(theirs.Data))
	}
	// An empty collection must serialize as [] rather than null.
	if !strings.Contains(h.do(http.MethodGet, "/v1/accounts", h.otherKey, nil).Body.String(), `"data":[]`) {
		t.Error("an empty list serialized as null rather than []")
	}
}

// ── transfers ───────────────────────────────────────────────────────────────

func TestCreateTransfer(t *testing.T) {
	h := newHarness(t)
	cash := h.createAccount("cash", "asset", true)
	payable := h.createAccount("payable", "liability", true)
	fees := h.createAccount("fees", "revenue", true)

	rec := h.do(http.MethodPost, "/v1/transfers", h.writeKey, map[string]any{
		"currency":    "USD",
		"description": "charge",
		"entries": []map[string]any{
			{"account_id": cash, "direction": "debit", "amount": 10000},
			{"account_id": payable, "direction": "credit", "amount": 9680},
			{"account_id": fees, "direction": "credit", "amount": 320},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	txn := decodeBody[transactionResponse](t, rec)
	if txn.Status != "posted" {
		t.Errorf("status = %q, want posted", txn.Status)
	}
	if len(txn.Entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(txn.Entries))
	}

	balance := decodeBody[balanceResponse](t, h.do(http.MethodGet, "/v1/accounts/"+fees+"/balance", h.readKey, nil))
	if balance.Posted != 320 {
		t.Errorf("fees posted = %d, want 320", balance.Posted)
	}
	if balance.Available != 320 {
		t.Errorf("fees available = %d, want 320", balance.Available)
	}
}

func TestTransferValidation(t *testing.T) {
	h := newHarness(t)
	cash := h.createAccount("cash", "asset", true)
	payable := h.createAccount("payable", "liability", true)

	tests := []struct {
		name     string
		body     map[string]any
		wantCode string
	}{
		{
			"unbalanced",
			map[string]any{"currency": "USD", "entries": []map[string]any{
				{"account_id": cash, "direction": "debit", "amount": 100},
				{"account_id": payable, "direction": "credit", "amount": 99},
			}},
			"transaction_unbalanced",
		},
		{
			"no entries",
			map[string]any{"currency": "USD", "entries": []map[string]any{}},
			"no_entries",
		},
		{
			"bad direction",
			map[string]any{"currency": "USD", "entries": []map[string]any{
				{"account_id": cash, "direction": "sideways", "amount": 100},
			}},
			"invalid_direction",
		},
		{
			"zero amount",
			map[string]any{"currency": "USD", "entries": []map[string]any{
				{"account_id": cash, "direction": "debit", "amount": 0},
				{"account_id": payable, "direction": "credit", "amount": 0},
			}},
			"invalid_amount",
		},
		{
			"unknown account",
			map[string]any{"currency": "USD", "entries": []map[string]any{
				{"account_id": "acct_nope", "direction": "debit", "amount": 100},
				{"account_id": payable, "direction": "credit", "amount": 100},
			}},
			"account_not_found",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(http.MethodPost, "/v1/transfers", h.writeKey, tc.body)
			if rec.Code < 400 || rec.Code >= 500 {
				t.Fatalf("status = %d, want a 4xx; body %s", rec.Code, rec.Body.String())
			}
			if got := decodeError(t, rec).Code; got != tc.wantCode {
				t.Errorf("code = %q, want %q; body %s", got, tc.wantCode, rec.Body.String())
			}
		})
	}
}

// Posting against another tenant's account would balance perfectly and leave two
// merchants' books wrong.
func TestTransferCannotTouchAnotherMerchantsAccount(t *testing.T) {
	h := newHarness(t)
	mine := h.createAccount("cash", "asset", true)

	rec := h.do(http.MethodPost, "/v1/accounts", h.otherKey, map[string]any{
		"name": "their_cash", "type": "asset", "currency": "USD",
	})
	theirs := decodeBody[accountResponse](t, rec).ID

	got := h.do(http.MethodPost, "/v1/transfers", h.writeKey, map[string]any{
		"currency": "USD",
		"entries": []map[string]any{
			{"account_id": mine, "direction": "debit", "amount": 100},
			{"account_id": theirs, "direction": "credit", "amount": 100},
		},
	})
	if got.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", got.Code, got.Body.String())
	}
}

func TestInsufficientFundsIsUnprocessable(t *testing.T) {
	h := newHarness(t)
	cash := h.createAccount("cash", "asset", true)
	customer := h.createAccount("customer", "liability", false)

	h.do(http.MethodPost, "/v1/transfers", h.writeKey, map[string]any{
		"currency": "USD",
		"entries": []map[string]any{
			{"account_id": cash, "direction": "debit", "amount": 5000},
			{"account_id": customer, "direction": "credit", "amount": 5000},
		},
	})

	rec := h.do(http.MethodPost, "/v1/transfers", h.writeKey, map[string]any{
		"currency": "USD",
		"entries": []map[string]any{
			{"account_id": customer, "direction": "debit", "amount": 5001},
			{"account_id": cash, "direction": "credit", "amount": 5001},
		},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body.String())
	}

	body := decodeError(t, rec)
	if body.Type != TypeLedger {
		t.Errorf("type = %q, want %q; a well-formed request the books refuse is not an invalid request", body.Type, TypeLedger)
	}
	if body.Code != "insufficient_funds" {
		t.Errorf("code = %q", body.Code)
	}
}

func TestReverseTransaction(t *testing.T) {
	h := newHarness(t)
	cash := h.createAccount("cash", "asset", true)
	payable := h.createAccount("payable", "liability", true)

	created := decodeBody[transactionResponse](t, h.do(http.MethodPost, "/v1/transfers", h.writeKey, map[string]any{
		"currency": "USD",
		"entries": []map[string]any{
			{"account_id": cash, "direction": "debit", "amount": 2500},
			{"account_id": payable, "direction": "credit", "amount": 2500},
		},
	}))

	rec := h.do(http.MethodPost, "/v1/transactions/"+created.ID+"/reverse", h.writeKey,
		map[string]any{"reason": "customer disputed"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody[transactionResponse](t, rec).ReversesID; got != created.ID {
		t.Errorf("reverses_id = %q, want %q", got, created.ID)
	}

	balance := decodeBody[balanceResponse](t, h.do(http.MethodGet, "/v1/accounts/"+cash+"/balance", h.readKey, nil))
	if balance.Posted != 0 {
		t.Errorf("balance after reversal = %d, want 0", balance.Posted)
	}

	// A second reversal must conflict.
	again := h.do(http.MethodPost, "/v1/transactions/"+created.ID+"/reverse", h.writeKey, nil)
	if again.Code != http.StatusConflict {
		t.Errorf("second reversal: status = %d, want 409", again.Code)
	}
}

// ── pagination ──────────────────────────────────────────────────────────────

func TestListEntriesPagination(t *testing.T) {
	h := newHarness(t)
	cash := h.createAccount("cash", "asset", true)
	payable := h.createAccount("payable", "liability", true)

	const n = 25
	for i := 0; i < n; i++ {
		h.do(http.MethodPost, "/v1/transfers", h.writeKey, map[string]any{
			"currency": "USD",
			"entries": []map[string]any{
				{"account_id": cash, "direction": "debit", "amount": i + 1},
				{"account_id": payable, "direction": "credit", "amount": i + 1},
			},
		})
	}

	seen := map[int64]bool{}
	cursor := ""
	for page := 0; ; page++ {
		if page > n {
			t.Fatal("pagination did not terminate")
		}

		path := "/v1/accounts/" + cash + "/entries?limit=10"
		if cursor != "" {
			path += "&starting_after=" + cursor
		}

		body := decodeBody[listResponse[entryResponse]](t, h.do(http.MethodGet, path, h.readKey, nil))
		for _, e := range body.Data {
			if seen[e.ID] {
				t.Fatalf("entry %d returned twice", e.ID)
			}
			seen[e.ID] = true
		}
		if !body.HasMore {
			if body.NextCursor != "" {
				t.Error("has_more is false but next_cursor is set")
			}
			break
		}
		cursor = body.NextCursor
	}

	if len(seen) != n {
		t.Errorf("saw %d entries, want %d", len(seen), n)
	}
}

func TestPaginationRejectsBadInput(t *testing.T) {
	h := newHarness(t)
	cash := h.createAccount("cash", "asset", true)

	tests := []struct {
		query    string
		wantCode string
	}{
		{"?limit=abc", "invalid_limit"},
		{"?limit=0", "invalid_limit"},
		{"?limit=100000", "invalid_limit"},
		{"?starting_after=not-a-cursor", "invalid_cursor"},
	}

	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			rec := h.do(http.MethodGet, "/v1/accounts/"+cash+"/entries"+tc.query, h.readKey, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
			}
			if got := decodeError(t, rec).Code; got != tc.wantCode {
				t.Errorf("code = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

// ── request handling ────────────────────────────────────────────────────────

// A caller that misspells a field and gets a 201 has been failed by the API, and in a
// payments context that failure moves money.
func TestUnknownFieldsAreRejected(t *testing.T) {
	h := newHarness(t)

	rec := h.doRaw(http.MethodPost, "/v1/accounts", h.writeKey,
		`{"name":"cash","type":"asset","currency":"USD","allow_negatve":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Code; got != "malformed_json" {
		t.Errorf("code = %q, want malformed_json", got)
	}
}

func TestMalformedBodies(t *testing.T) {
	h := newHarness(t)

	tests := []struct {
		name string
		body string
	}{
		{"not json", `{`},
		{"wrong type", `{"name":123,"type":"asset","currency":"USD"}`},
		{"two values", `{"name":"a","type":"asset","currency":"USD"}{"name":"b"}`},
		{"empty", ``},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.doRaw(http.MethodPost, "/v1/accounts", h.writeKey, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestWrongContentType(t *testing.T) {
	h := newHarness(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/accounts", strings.NewReader(`{"name":"a"}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+h.writeKey)

	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", rec.Code)
	}
}

func TestBodyLimit(t *testing.T) {
	h := newHarness(t)
	h.server = NewServer(Options{
		Config: config.HTTPConfig{HandlerTimeout: 5 * time.Second, MaxBodyBytes: 64},
		Ledger: h.svc, Tx: passthroughTx{}, Keys: h.keys, Health: okHealth{},
	})

	huge := `{"name":"` + strings.Repeat("x", 500) + `","type":"asset","currency":"USD"}`
	rec := h.doRaw(http.MethodPost, "/v1/accounts", h.writeKey, huge)

	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 413 or 400; body %s", rec.Code, rec.Body.String())
	}
}

// ── envelope, routing and health ────────────────────────────────────────────

// Every error carries the same shape, so a client can parse failures generically.
func TestErrorEnvelopeShape(t *testing.T) {
	h := newHarness(t)

	rec := h.do(http.MethodGet, "/v1/accounts/acct_missing", h.readKey, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(envelope) != 1 {
		t.Errorf("envelope has %d top-level keys, want 1", len(envelope))
	}
	if _, ok := envelope["error"]; !ok {
		t.Fatal("envelope has no error key")
	}

	body := decodeError(t, rec)
	for name, value := range map[string]string{
		"type":       string(body.Type),
		"code":       body.Code,
		"message":    body.Message,
		"request_id": body.RequestID,
		"doc_url":    body.DocURL,
	} {
		if value == "" {
			t.Errorf("error body field %s is empty", name)
		}
	}
	if !strings.HasSuffix(body.DocURL, body.Code) {
		t.Errorf("doc_url %q does not end with the code %q", body.DocURL, body.Code)
	}
}

// Every domain sentinel needs a row in the table. A missing row means a generic 500 for
// a condition the caller could have fixed.
func TestEveryTableEntryMapsToASensibleStatus(t *testing.T) {
	for _, m := range errorTable {
		got := toAPIError(m.sentinel)
		if got.Status != m.status {
			t.Errorf("%v: status = %d, want %d", m.sentinel, got.Status, m.status)
		}
		if got.Code != m.code {
			t.Errorf("%v: code = %q, want %q", m.sentinel, got.Code, m.code)
		}
		if got.Status < 400 || got.Status >= 600 {
			t.Errorf("%v: status %d is not an error status", m.sentinel, got.Status)
		}
	}
}

// An unmapped error must not leak its message to the caller.
func TestUnmappedErrorsBecomeOpaqueFiveHundreds(t *testing.T) {
	secret := "connection to db-primary-3.internal:5432 refused"

	got := toAPIError(errors.New(secret))
	if got.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", got.Status)
	}
	if strings.Contains(got.Message, secret) {
		t.Error("the internal error message reached the response body")
	}
	if got.Err == nil || !strings.Contains(got.Err.Error(), secret) {
		t.Error("the cause was dropped and cannot be logged")
	}
}

func TestRequestIDIsGeneratedAndNotTrusted(t *testing.T) {
	h := newHarness(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Request-Id", "req_attacker_supplied")

	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)

	got := rec.Header().Get("Request-Id")
	if got == "" {
		t.Fatal("no Request-Id header on the response")
	}
	if got == "req_attacker_supplied" {
		t.Error("a client-supplied Request-Id was echoed back")
	}
	if !strings.HasPrefix(got, "req_") {
		t.Errorf("Request-Id = %q, want a req_ prefix", got)
	}
}

func TestHealthEndpointsNeedNoAuth(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := h.do(http.MethodGet, path, "", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
	}
}

type failingHealth struct{}

func (failingHealth) Ping(context.Context) error { return context.DeadlineExceeded }

// Readiness must fail when the database is unreachable, but liveness must not: an
// orchestrator that restarts on a brief database blip kills a process that would have
// recovered.
func TestReadinessFailsWhenDatabaseIsDown(t *testing.T) {
	h := newHarness(t)
	h.server = NewServer(Options{
		Config: config.HTTPConfig{HandlerTimeout: 5 * time.Second, MaxBodyBytes: 1 << 20},
		Ledger: h.svc, Tx: passthroughTx{}, Keys: h.keys, Health: failingHealth{},
	})

	if rec := h.do(http.MethodGet, "/readyz", "", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz: status = %d, want 503", rec.Code)
	}
	if rec := h.do(http.MethodGet, "/healthz", "", nil); rec.Code != http.StatusOK {
		t.Errorf("/healthz: status = %d, want 200 regardless of the database", rec.Code)
	}
}

func TestRoutingErrors(t *testing.T) {
	h := newHarness(t)

	if rec := h.do(http.MethodGet, "/v1/nonexistent", h.readKey, nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path: status = %d, want 404", rec.Code)
	}
	if rec := h.do(http.MethodDelete, "/v1/accounts", h.writeKey, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("bad method: status = %d, want 405", rec.Code)
	}
}

func TestRateLimiting(t *testing.T) {
	h := newHarness(t)
	h.server = NewServer(Options{
		Config: config.HTTPConfig{HandlerTimeout: 5 * time.Second, MaxBodyBytes: 1 << 20, RateLimitPerMin: 3},
		Ledger: h.svc, Tx: passthroughTx{}, Keys: h.keys, Health: okHealth{},
	})

	var limited bool
	for i := 0; i < 6; i++ {
		rec := h.do(http.MethodGet, "/v1/accounts", h.readKey, nil)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			if rec.Header().Get("Retry-After") == "" {
				t.Error("a 429 carried no Retry-After header")
			}
			if got := decodeError(t, rec).Type; got != TypeRateLimit {
				t.Errorf("type = %q, want %q", got, TypeRateLimit)
			}
			break
		}
	}
	if !limited {
		t.Error("no request was rate limited despite a limit of 3/min")
	}
}

// One merchant exhausting its allowance must not affect another.
func TestRateLimitIsPerKey(t *testing.T) {
	h := newHarness(t)
	h.server = NewServer(Options{
		Config: config.HTTPConfig{HandlerTimeout: 5 * time.Second, MaxBodyBytes: 1 << 20, RateLimitPerMin: 2},
		Ledger: h.svc, Tx: passthroughTx{}, Keys: h.keys, Health: okHealth{},
	})

	for i := 0; i < 5; i++ {
		h.do(http.MethodGet, "/v1/accounts", h.readKey, nil)
	}

	if rec := h.do(http.MethodGet, "/v1/accounts", h.otherKey, nil); rec.Code == http.StatusTooManyRequests {
		t.Error("one key's limit affected another key")
	}
}

func TestRateLimiterRefillsOverTime(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newRateLimiter(60) // one per second
	l.now = func() time.Time { return now }

	// Drain the bucket.
	for i := 0; i < 60; i++ {
		if !l.allow("k") {
			t.Fatalf("denied at request %d while the bucket should still be full", i)
		}
	}
	if l.allow("k") {
		t.Fatal("allowed a 61st request from an empty bucket")
	}

	now = now.Add(2 * time.Second)
	if !l.allow("k") {
		t.Error("the bucket did not refill after two seconds")
	}
}

func TestRateLimiterSweepDropsIdleBuckets(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newRateLimiter(10)
	l.now = func() time.Time { return now }

	l.allow("old")
	now = now.Add(time.Hour)
	l.allow("fresh")

	l.sweep(30 * time.Minute)

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.buckets["old"]; ok {
		t.Error("an idle bucket survived the sweep")
	}
	if _, ok := l.buckets["fresh"]; !ok {
		t.Error("an active bucket was swept")
	}
}

// A panic must become a 500 rather than a dropped connection, and its message must not
// reach the caller.
func TestPanicBecomesFiveHundred(t *testing.T) {
	h := newHarness(t)
	h.server.handler = recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret internal detail")
	}))

	rec := h.do(http.MethodGet, "/anything", "", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret internal detail") {
		t.Error("the panic message reached the response body")
	}
}

func TestMoneyAmountsAreIntegersOnTheWire(t *testing.T) {
	h := newHarness(t)
	cash := h.createAccount("cash", "asset", true)
	payable := h.createAccount("payable", "liability", true)

	h.do(http.MethodPost, "/v1/transfers", h.writeKey, map[string]any{
		"currency": "USD",
		"entries": []map[string]any{
			{"account_id": cash, "direction": "debit", "amount": 9007199254740993}, // 2^53+1
			{"account_id": payable, "direction": "credit", "amount": 9007199254740993},
		},
	})

	body := h.do(http.MethodGet, "/v1/accounts/"+cash+"/balance", h.readKey, nil).Body.String()
	if !strings.Contains(body, "9007199254740993") {
		t.Errorf("a value past 2^53 did not survive the wire: %s", body)
	}
	if strings.Contains(body, "9007199254740992") {
		t.Error("the amount was rounded, so it went through a float somewhere")
	}
}

func TestMoneyParsesCurrencyCaseInsensitively(t *testing.T) {
	h := newHarness(t)

	rec := h.do(http.MethodPost, "/v1/accounts", h.writeKey, map[string]any{
		"name": "yen", "type": "asset", "currency": "jpy",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody[accountResponse](t, rec).Currency; got != string(money.JPY) {
		t.Errorf("currency = %q, want JPY", got)
	}
}
