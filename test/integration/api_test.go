//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/httpapi"
	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/payments"
	"github.com/Madhav-000-s/ledgerd/internal/platform/config"
	"github.com/Madhav-000-s/ledgerd/internal/postgres"
)

// apiEnv is the whole stack — router, auth, idempotency, payments — over real Postgres.
//
// Everything below the HTTP boundary is the production wiring. The API-key store, the
// idempotency store and the ledger repository are the real ones, so this exercises the
// paths that a unit test with an in-memory repository cannot reach.
type apiEnv struct {
	*env
	server   *httpapi.Server
	keyRepo  *postgres.APIKeyRepo
	writeKey string
	readKey  string
	otherKey string
}

func newAPIEnv(t *testing.T) *apiEnv {
	t.Helper()

	e := newEnv(t, config.IsolationReadCommitted)

	ledgerRepo := postgres.NewLedgerRepo(e.db)
	ledgerSvc := ledger.NewService(ledgerRepo)

	paySvc, err := payments.NewService(ledgerSvc, payments.Options{
		Fees:                payments.FeeSchedule{Bps: 290, Fixed: 30},
		RefundPercentageFee: true,
	})
	if err != nil {
		t.Fatalf("payments.NewService = %v", err)
	}

	keyRepo := postgres.NewAPIKeyRepo(e.db)

	a := &apiEnv{
		env:     e,
		keyRepo: keyRepo,
		server: httpapi.NewServer(httpapi.Options{
			Config: config.HTTPConfig{
				HandlerTimeout: 10 * time.Second,
				MaxBodyBytes:   1 << 20,
			},
			Ledger:      ledgerSvc,
			Tx:          e.txm,
			Keys:        keyRepo,
			Health:      e.db,
			Payments:    paySvc,
			Idempotency: postgres.NewIdempotencyRepo(e.db),
		}),
	}

	a.writeKey = a.issueKey(t, testMerchant, httpapi.RoleWrite)
	a.readKey = a.issueKey(t, testMerchant, httpapi.RoleRead)
	a.otherKey = a.issueKey(t, "mer_other", httpapi.RoleWrite)
	return a
}

// issueKey mints a real API key and stores it, hash and all.
func (a *apiEnv) issueKey(t *testing.T, merchantID string, role httpapi.Role) string {
	t.Helper()

	secret, rec, err := httpapi.GenerateAPIKey(merchantID, role, false)
	if err != nil {
		t.Fatalf("GenerateAPIKey = %v", err)
	}
	if err := a.keyRepo.Create(a.ctx, rec, string(role)+" key"); err != nil {
		t.Fatalf("Create api key = %v", err)
	}
	return secret
}

func (a *apiEnv) do(t *testing.T, method, path, key string, body any, idemKey string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshalling: %v", err)
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
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}

	rec := httptest.NewRecorder()
	a.server.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()

	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v\nbody: %s", err, rec.Body.String())
	}
	return out
}

type apiPayment struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	Amount     int64  `json:"amount"`
	Fee        int64  `json:"fee"`
	Net        int64  `json:"net"`
	Refunded   int64  `json:"refunded"`
	Refundable int64  `json:"refundable"`
}

type apiAccount struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type apiBalance struct {
	Posted    int64 `json:"posted"`
	Available int64 `json:"available"`
}

// Authentication against the real store: a key minted and hashed by the production path
// verifies, and a plausible-looking forgery does not.
func TestRealAPIKeyAuthentication(t *testing.T) {
	a := newAPIEnv(t)

	if rec := a.do(t, http.MethodGet, "/v1/accounts", a.readKey, nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("a valid key was rejected: status %d, body %s", rec.Code, rec.Body.String())
	}

	// Same lookup prefix, wrong secret body — the case the prefix optimization has to get
	// right, since it is what narrows the argon2id comparison to one candidate.
	forged := a.writeKey[:len(a.writeKey)-6] + "ZZZZZZ"
	if rec := a.do(t, http.MethodGet, "/v1/accounts", forged, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("a forged secret with a valid prefix was accepted: status %d", rec.Code)
	}

	if rec := a.do(t, http.MethodGet, "/v1/accounts", "", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated request was accepted: status %d", rec.Code)
	}
}

// A revoked key must stop working, and revocation must not delete the key: an audit trail
// that cannot say which key authorized a movement is not much of an audit trail.
func TestRevokedKeyIsRejected(t *testing.T) {
	a := newAPIEnv(t)

	secret, rec, err := httpapi.GenerateAPIKey(testMerchant, httpapi.RoleWrite, false)
	if err != nil {
		t.Fatalf("GenerateAPIKey = %v", err)
	}
	if err := a.keyRepo.Create(a.ctx, rec, "doomed"); err != nil {
		t.Fatalf("Create = %v", err)
	}

	if got := a.do(t, http.MethodGet, "/v1/accounts", secret, nil, ""); got.Code != http.StatusOK {
		t.Fatalf("the key did not work before revocation: status %d", got.Code)
	}

	if err := a.keyRepo.Revoke(a.ctx, rec.ID); err != nil {
		t.Fatalf("Revoke = %v", err)
	}

	if got := a.do(t, http.MethodGet, "/v1/accounts", secret, nil, ""); got.Code != http.StatusUnauthorized {
		t.Errorf("a revoked key still works: status %d", got.Code)
	}

	// The row survives.
	var count int64
	if err := a.db.Pool().QueryRow(a.ctx,
		`SELECT count(*) FROM api_keys WHERE id = $1`, rec.ID).Scan(&count); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if count != 1 {
		t.Error("revocation deleted the key rather than marking it")
	}
}

// The whole flow over HTTP, against real Postgres: charge, capture, refund, read back.
func TestFullPaymentFlowOverHTTP(t *testing.T) {
	a := newAPIEnv(t)

	// An authorization, then a partial capture.
	capture := false
	created := decode[apiPayment](t, a.do(t, http.MethodPost, "/v1/payments", a.writeKey, map[string]any{
		"amount": 10000, "currency": "USD", "capture": &capture,
	}, "idem-auth-1"))
	if created.State != string(payments.StateRequiresCapture) {
		t.Fatalf("state = %q, want requires_capture", created.State)
	}

	captured := decode[apiPayment](t, a.do(t, http.MethodPost,
		"/v1/payments/"+created.ID+"/capture", a.writeKey, map[string]any{"amount": 6000}, "idem-cap-1"))
	if captured.State != string(payments.StateSucceeded) {
		t.Errorf("state = %q, want succeeded", captured.State)
	}
	if captured.Amount != 6000 || captured.Fee != 204 {
		t.Errorf("amount/fee = %d/%d, want 6000/204", captured.Amount, captured.Fee)
	}

	// A partial refund.
	refunded := decode[apiPayment](t, a.do(t, http.MethodPost, "/v1/refunds", a.writeKey, map[string]any{
		"payment_id": created.ID, "amount": 2000,
	}, "idem-ref-1"))
	if refunded.State != string(payments.StatePartiallyRefunded) {
		t.Errorf("state = %q, want partially_refunded", refunded.State)
	}
	if refunded.Refunded != 2000 || refunded.Refundable != 4000 {
		t.Errorf("refunded/refundable = %d/%d, want 2000/4000", refunded.Refunded, refunded.Refundable)
	}

	// Reading it back gives the same answer, because the state is derived from the books.
	reread := decode[apiPayment](t, a.do(t, http.MethodGet, "/v1/payments/"+created.ID, a.readKey, nil, ""))
	if reread.State != refunded.State || reread.Refunded != refunded.Refunded {
		t.Errorf("re-reading gave %+v, want %+v", reread, refunded)
	}

	// The books balance across every account the flow touched.
	accounts := decode[struct {
		Data []apiAccount `json:"data"`
	}](t, a.do(t, http.MethodGet, "/v1/accounts", a.readKey, nil, ""))

	var trial int64
	for _, acct := range accounts.Data {
		bal := decode[apiBalance](t, a.do(t, http.MethodGet,
			"/v1/accounts/"+acct.ID+"/balance", a.readKey, nil, ""))
		trial += ledger.AccountType(acct.Type).EquationSign() * bal.Posted
	}
	if trial != 0 {
		t.Errorf("trial balance across the API = %d, want 0", trial)
	}
}

// Idempotency end to end, over HTTP and a real database: the same key with the same body
// charges once, and every later attempt replays the stored response byte for byte.
func TestIdempotentChargeOverHTTP(t *testing.T) {
	a := newAPIEnv(t)

	body := map[string]any{"amount": 10000, "currency": "USD"}

	first := a.do(t, http.MethodPost, "/v1/payments", a.writeKey, body, "idem-charge-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", first.Code, first.Body.String())
	}

	for i := 0; i < 3; i++ {
		again := a.do(t, http.MethodPost, "/v1/payments", a.writeKey, body, "idem-charge-1")
		if again.Code != first.Code {
			t.Errorf("replay %d status = %d, want %d", i, again.Code, first.Code)
		}
		if again.Body.String() != first.Body.String() {
			t.Errorf("replay %d body differs from the original", i)
		}
		if again.Header().Get("Idempotent-Replay") != "true" {
			t.Errorf("replay %d has no Idempotent-Replay header", i)
		}
	}

	// Exactly one charge reached the ledger.
	var charges int64
	if err := a.db.Pool().QueryRow(a.ctx,
		`SELECT count(*) FROM transactions WHERE metadata->>'payment_role' = 'charge'`).Scan(&charges); err != nil {
		t.Fatalf("counting charges: %v", err)
	}
	if charges != 1 {
		t.Errorf("%d charges in the ledger, want 1", charges)
	}

	// The same key with a different body is a client bug, not a retry.
	mismatch := a.do(t, http.MethodPost, "/v1/payments", a.writeKey,
		map[string]any{"amount": 99999, "currency": "USD"}, "idem-charge-1")
	if mismatch.Code != http.StatusUnprocessableEntity {
		t.Errorf("a reused key with a different body = %d, want 422", mismatch.Code)
	}
}

// The guarantee under concurrency, over the real stack: many simultaneous retries of one
// request produce exactly one charge, with no third outcome.
func TestConcurrentIdempotentChargesOverHTTP(t *testing.T) {
	a := newAPIEnv(t)

	const attempts = 30
	body := map[string]any{"amount": 10000, "currency": "USD"}

	var (
		created  atomic.Int64
		conflict atomic.Int64
		replay   atomic.Int64
		other    atomic.Int64
		wg       sync.WaitGroup
		gate     = make(chan struct{})
	)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate

			rec := a.do(t, http.MethodPost, "/v1/payments", a.writeKey, body, "idem-race-1")
			switch {
			case rec.Code == http.StatusCreated && rec.Header().Get("Idempotent-Replay") != "true":
				created.Add(1)
			case rec.Header().Get("Idempotent-Replay") == "true":
				replay.Add(1)
			case rec.Code == http.StatusConflict:
				conflict.Add(1)
			default:
				other.Add(1)
				t.Errorf("unexpected outcome: status %d, body %s", rec.Code, rec.Body.String())
			}
		}()
	}
	close(gate)
	wg.Wait()

	if got := created.Load(); got != 1 {
		t.Errorf("%d requests charged, want exactly 1", got)
	}
	if got := other.Load(); got != 0 {
		t.Errorf("%d requests had an outcome that is neither a charge, a replay nor a conflict", got)
	}
	if created.Load()+replay.Load()+conflict.Load() != attempts {
		t.Errorf("outcomes do not account for all %d attempts", attempts)
	}

	var charges int64
	if err := a.db.Pool().QueryRow(a.ctx,
		`SELECT count(*) FROM transactions WHERE metadata->>'payment_role' = 'charge'`).Scan(&charges); err != nil {
		t.Fatalf("counting charges: %v", err)
	}
	if charges != 1 {
		t.Errorf("%d charges in the ledger after %d concurrent retries, want 1", charges, attempts)
	}
}

// A money-moving route without a key is refused, so a caller cannot opt out of
// idempotency by omitting the header.
func TestIdempotencyKeyIsRequiredOverHTTP(t *testing.T) {
	a := newAPIEnv(t)

	rec := a.do(t, http.MethodPost, "/v1/payments", a.writeKey,
		map[string]any{"amount": 10000, "currency": "USD"}, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "idempotency_key_required") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestTransactionReadAndReversalOverHTTP(t *testing.T) {
	a := newAPIEnv(t)

	cash := decode[apiAccount](t, a.do(t, http.MethodPost, "/v1/accounts", a.writeKey, map[string]any{
		"name": "cash", "type": "asset", "currency": "USD", "allow_negative": true,
	}, ""))
	payable := decode[apiAccount](t, a.do(t, http.MethodPost, "/v1/accounts", a.writeKey, map[string]any{
		"name": "payable", "type": "liability", "currency": "USD", "allow_negative": true,
	}, ""))

	txn := decode[struct {
		ID string `json:"id"`
	}](t, a.do(t, http.MethodPost, "/v1/transfers", a.writeKey, map[string]any{
		"currency": "USD",
		"entries": []map[string]any{
			{"account_id": cash.ID, "direction": "debit", "amount": 2500},
			{"account_id": payable.ID, "direction": "credit", "amount": 2500},
		},
	}, "idem-transfer-1"))

	// Read it back with its entries expanded.
	got := decode[struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Entries []struct {
			Direction string `json:"direction"`
			Amount    int64  `json:"amount"`
		} `json:"entries"`
	}](t, a.do(t, http.MethodGet, "/v1/transactions/"+txn.ID, a.readKey, nil, ""))

	if got.ID != txn.ID || got.Status != "posted" {
		t.Errorf("read back %+v", got)
	}
	if len(got.Entries) != 2 {
		t.Errorf("got %d entries, want 2", len(got.Entries))
	}

	// Another merchant must see it as missing rather than forbidden.
	if rec := a.do(t, http.MethodGet, "/v1/transactions/"+txn.ID, a.otherKey, nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant read = %d, want 404", rec.Code)
	}

	// Reverse it, and the balance returns to zero.
	if rec := a.do(t, http.MethodPost, "/v1/transactions/"+txn.ID+"/reverse", a.writeKey,
		map[string]any{"reason": "disputed"}, "idem-reverse-1"); rec.Code != http.StatusCreated {
		t.Fatalf("reverse = %d, body %s", rec.Code, rec.Body.String())
	}

	bal := decode[apiBalance](t, a.do(t, http.MethodGet,
		"/v1/accounts/"+cash.ID+"/balance", a.readKey, nil, ""))
	if bal.Posted != 0 {
		t.Errorf("balance after reversal = %d, want 0", bal.Posted)
	}
}

func TestEntriesPaginationOverHTTP(t *testing.T) {
	a := newAPIEnv(t)

	cash := decode[apiAccount](t, a.do(t, http.MethodPost, "/v1/accounts", a.writeKey, map[string]any{
		"name": "cash", "type": "asset", "currency": "USD", "allow_negative": true,
	}, ""))
	payable := decode[apiAccount](t, a.do(t, http.MethodPost, "/v1/accounts", a.writeKey, map[string]any{
		"name": "payable", "type": "liability", "currency": "USD", "allow_negative": true,
	}, ""))

	const n = 23
	for i := 0; i < n; i++ {
		rec := a.do(t, http.MethodPost, "/v1/transfers", a.writeKey, map[string]any{
			"currency": "USD",
			"entries": []map[string]any{
				{"account_id": cash.ID, "direction": "debit", "amount": i + 1},
				{"account_id": payable.ID, "direction": "credit", "amount": i + 1},
			},
		}, "idem-page-"+strings.Repeat("x", i)+"1")
		if rec.Code != http.StatusCreated {
			t.Fatalf("transfer %d = %d, body %s", i, rec.Code, rec.Body.String())
		}
	}

	seen := map[int64]bool{}
	cursor := ""
	for page := 0; ; page++ {
		if page > n {
			t.Fatal("pagination did not terminate")
		}

		path := "/v1/accounts/" + cash.ID + "/entries?limit=10"
		if cursor != "" {
			path += "&starting_after=" + cursor
		}

		body := decode[struct {
			Data []struct {
				ID int64 `json:"id"`
			} `json:"data"`
			HasMore    bool   `json:"has_more"`
			NextCursor string `json:"next_cursor"`
		}](t, a.do(t, http.MethodGet, path, a.readKey, nil, ""))

		for _, e := range body.Data {
			if seen[e.ID] {
				t.Fatalf("entry %d returned twice", e.ID)
			}
			seen[e.ID] = true
		}
		if !body.HasMore {
			break
		}
		cursor = body.NextCursor
	}

	if len(seen) != n {
		t.Errorf("saw %d entries, want %d", len(seen), n)
	}
}

func TestHealthEndpointsAgainstRealDatabase(t *testing.T) {
	a := newAPIEnv(t)

	for _, path := range []string{"/healthz", "/readyz"} {
		if rec := a.do(t, http.MethodGet, path, "", nil, ""); rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, rec.Code)
		}
	}
}
