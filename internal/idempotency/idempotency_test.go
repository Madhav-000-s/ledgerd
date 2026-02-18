package idempotency

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── canonicalization ────────────────────────────────────────────────────────

// A client that reserializes its request map in a different key order — which any
// language with unordered maps does routinely — must not get a spurious mismatch on a
// legitimate retry.
func TestCanonicalJSONIsOrderIndependent(t *testing.T) {
	a := []byte(`{"amount":10000,"currency":"USD","metadata":{"z":1,"a":2}}`)
	b := []byte(`{"metadata":{"a":2,"z":1},"currency":"USD","amount":10000}`)

	ca, err := CanonicalJSON(a)
	if err != nil {
		t.Fatalf("CanonicalJSON(a) = %v", err)
	}
	cb, err := CanonicalJSON(b)
	if err != nil {
		t.Fatalf("CanonicalJSON(b) = %v", err)
	}

	if !bytes.Equal(ca, cb) {
		t.Errorf("key order changed the canonical form:\n a: %s\n b: %s", ca, cb)
	}
}

func TestCanonicalJSONIgnoresWhitespace(t *testing.T) {
	a := []byte(`{"amount":10000}`)
	b := []byte("{\n  \"amount\" :  10000\n}\n")

	ca, _ := CanonicalJSON(a)
	cb, _ := CanonicalJSON(b)
	if !bytes.Equal(ca, cb) {
		t.Errorf("whitespace changed the canonical form:\n a: %s\n b: %s", ca, cb)
	}
}

// Numbers are normalized so that 100, 100.0 and 1e2 fingerprint identically.
func TestCanonicalNumbers(t *testing.T) {
	groups := [][]string{
		{`{"n":100}`, `{"n":100.0}`, `{"n":1e2}`, `{"n":1.00e2}`, `{"n":100.000}`},
		{`{"n":0}`, `{"n":0.0}`, `{"n":-0.0}`, `{"n":0e5}`},
		{`{"n":0.1}`, `{"n":0.10}`, `{"n":1e-1}`},
		{`{"n":-5}`, `{"n":-5.0}`},
	}

	for _, group := range groups {
		want, err := CanonicalJSON([]byte(group[0]))
		if err != nil {
			t.Fatalf("CanonicalJSON(%s) = %v", group[0], err)
		}
		for _, variant := range group[1:] {
			got, err := CanonicalJSON([]byte(variant))
			if err != nil {
				t.Fatalf("CanonicalJSON(%s) = %v", variant, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s canonicalized to %s, but %s gave %s", variant, got, group[0], want)
			}
		}
	}
}

// Two genuinely different large integers must not collide. A float64 round trip would
// make these equal, which is exactly the mistake this codebase avoids elsewhere.
func TestCanonicalNumbersPreserveLargeIntegers(t *testing.T) {
	a, err := CanonicalJSON([]byte(`{"n":9007199254740993}`)) // 2^53+1
	if err != nil {
		t.Fatalf("CanonicalJSON = %v", err)
	}
	b, err := CanonicalJSON([]byte(`{"n":9007199254740992}`)) // 2^53
	if err != nil {
		t.Fatalf("CanonicalJSON = %v", err)
	}
	if bytes.Equal(a, b) {
		t.Errorf("two distinct integers past 2^53 canonicalized identically: %s", a)
	}
	if !strings.Contains(string(a), "9007199254740993") {
		t.Errorf("the value was rounded: %s", a)
	}
}

func TestCanonicalJSONHandlesEmptyAndNested(t *testing.T) {
	got, err := CanonicalJSON(nil)
	if err != nil {
		t.Fatalf("CanonicalJSON(nil) = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("CanonicalJSON(nil) = %q, want empty", got)
	}

	nested := []byte(`{"b":[{"y":1,"x":2},null,true],"a":"s"}`)
	out, err := CanonicalJSON(nested)
	if err != nil {
		t.Fatalf("CanonicalJSON = %v", err)
	}
	if string(out) != `{"a":"s","b":[{"x":2,"y":1},null,true]}` {
		t.Errorf("canonical form = %s", out)
	}
}

func TestCanonicalJSONRejectsGarbage(t *testing.T) {
	for _, body := range []string{`{`, `{"a":}`, `{"a":1}{"b":2}`, `nope`} {
		if _, err := CanonicalJSON([]byte(body)); err == nil {
			t.Errorf("CanonicalJSON(%q) = nil, want an error", body)
		}
	}
}

func TestFingerprintDistinguishesMethodAndPath(t *testing.T) {
	body := []byte(`{"amount":100}`)

	base, _ := Fingerprint("POST", "/v1/transfers", body)
	otherPath, _ := Fingerprint("POST", "/v1/refunds", body)
	otherMethod, _ := Fingerprint("PUT", "/v1/transfers", body)
	otherBody, _ := Fingerprint("POST", "/v1/transfers", []byte(`{"amount":101}`))

	for name, got := range map[string][]byte{
		"path":   otherPath,
		"method": otherMethod,
		"body":   otherBody,
	} {
		if bytes.Equal(base, got) {
			t.Errorf("changing the %s did not change the fingerprint", name)
		}
	}

	// Reordering the body must not.
	reordered, _ := Fingerprint("POST", "/v1/transfers", []byte(` { "amount" : 100 } `))
	if !bytes.Equal(base, reordered) {
		t.Error("reformatting the body changed the fingerprint")
	}
}

func TestValidateKey(t *testing.T) {
	if err := ValidateKey("idem-key-123"); err != nil {
		t.Errorf("ValidateKey(valid) = %v", err)
	}
	if err := ValidateKey(""); !errors.Is(err, ErrKeyRequired) {
		t.Errorf("ValidateKey(\"\") = %v, want ErrKeyRequired", err)
	}
	if err := ValidateKey(strings.Repeat("k", 256)); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("ValidateKey(256 chars) = %v, want ErrInvalidKey", err)
	}
	// A newline in a key would forge a second entry in a log line.
	for _, bad := range []string{"key\nwith-newline", "key\twith-tab", "key\x00null", "kéy"} {
		if err := ValidateKey(bad); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ValidateKey(%q) = %v, want ErrInvalidKey", bad, err)
		}
	}
}

// ── the middleware ──────────────────────────────────────────────────────────

// memStore is an in-memory Store. It exists to exercise the middleware's decision flow;
// the atomicity of Complete with a ledger write is a property of the database and is
// asserted in the integration suite.
type memStore struct {
	mu      sync.Mutex
	byKey   map[string]*Record
	byID    map[string]*Record
	acquire int
}

func newMemStore() *memStore {
	return &memStore{byKey: map[string]*Record{}, byID: map[string]*Record{}}
}

func key(merchantID, k string) string { return merchantID + "\x00" + k }

func (s *memStore) Acquire(_ context.Context, rec *Record) (*Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.acquire++
	k := key(rec.MerchantID, rec.Key)
	if existing, ok := s.byKey[k]; ok {
		clone := *existing
		return &clone, false, nil
	}

	stored := *rec
	s.byKey[k] = &stored
	s.byID[rec.ID] = &stored
	return rec, true, nil
}

func (s *memStore) Get(_ context.Context, merchantID, k string) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.byKey[key(merchantID, k)]
	if !ok {
		return nil, ErrKeyNotFound
	}
	clone := *rec
	return &clone, nil
}

func (s *memStore) Complete(_ context.Context, recordID string, state State, status int, body []byte, resourceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.byID[recordID]
	if !ok {
		return ErrKeyNotFound
	}
	rec.State = state
	rec.RecoveryPoint = PointFinished
	rec.ResponseStatus = status
	rec.ResponseBody = append([]byte(nil), body...)
	rec.ResourceID = resourceID
	rec.LockExpiresAt = nil
	return nil
}

func (s *memStore) Advance(_ context.Context, recordID string, point RecoveryPoint, leaseUntil time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.byID[recordID]
	if !ok {
		return ErrKeyNotFound
	}
	rec.RecoveryPoint = point
	rec.LockExpiresAt = &leaseUntil
	return nil
}

func (s *memStore) TakeOver(_ context.Context, recordID string, leaseUntil time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.byID[recordID]
	if !ok || rec.State != StateInProgress {
		return false, nil
	}
	if rec.LockExpiresAt != nil && rec.LockExpiresAt.After(time.Now().UTC()) {
		return false, nil
	}
	rec.LockExpiresAt = &leaseUntil
	return true, nil
}

func (s *memStore) Release(_ context.Context, recordID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.byID[recordID]
	if !ok || rec.State != StateInProgress {
		return nil
	}
	delete(s.byKey, key(rec.MerchantID, rec.Key))
	delete(s.byID, recordID)
	return nil
}

func (s *memStore) DeleteExpired(_ context.Context, before time.Time, limit int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var n int64
	for k, rec := range s.byKey {
		if n >= int64(limit) {
			break
		}
		if rec.ExpiresAt.Before(before) {
			delete(s.byKey, k)
			delete(s.byID, rec.ID)
			n++
		}
	}
	return n, nil
}

// expire forces a record's lease into the past, standing in for a crashed holder.
func (s *memStore) expire(merchantID, k string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if rec, ok := s.byKey[key(merchantID, k)]; ok {
		past := time.Now().UTC().Add(-time.Hour)
		rec.LockExpiresAt = &past
	}
}

const testMerchant = "mer_test"

type mwHarness struct {
	t       *testing.T
	store   *memStore
	handler http.Handler
	calls   *int32Counter
}

type int32Counter struct {
	mu sync.Mutex
	n  int
}

func (c *int32Counter) inc() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

func (c *int32Counter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// writeErrJSON is a stand-in for the API's error envelope.
func writeErrJSON(w http.ResponseWriter, _ *http.Request, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrKeyRequired), errors.Is(err, ErrInvalidKey):
		status = http.StatusBadRequest
	case errors.Is(err, ErrFingerprintMismatch):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, ErrInProgress):
		status = http.StatusConflict
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// newMWHarness builds the middleware over a handler that records how often it ran.
func newMWHarness(t *testing.T, inner http.HandlerFunc) *mwHarness {
	t.Helper()

	store := newMemStore()
	calls := &int32Counter{}

	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.inc()
		inner(w, r)
	})

	mw := New(store,
		func(*http.Request) (string, bool) { return testMerchant, true },
		writeErrJSON,
	)

	return &mwHarness{t: t, store: store, handler: mw.Handler(wrapped), calls: calls}
}

func (h *mwHarness) post(key, body string) *httptest.ResponseRecorder {
	h.t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/v1/transfers", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// okHandler writes a 201 with a body derived from a counter, so a replay is
// distinguishable from a re-execution.
func okHandler(counter *int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		*counter++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]int{"execution": *counter})
	}
}

func TestKeyIsRequired(t *testing.T) {
	var n int
	h := newMWHarness(t, okHandler(&n))

	rec := h.post("", `{"amount":100}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if h.calls.get() != 0 {
		t.Error("the handler ran without an idempotency key")
	}
}

// The core guarantee: the same key with the same body executes exactly once, and every
// later attempt replays the first response byte for byte.
func TestSameKeySameBodyReplays(t *testing.T) {
	var n int
	h := newMWHarness(t, okHandler(&n))

	first := h.post("key-1", `{"amount":100}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.Code)
	}

	for i := 0; i < 3; i++ {
		again := h.post("key-1", `{"amount":100}`)

		if again.Code != first.Code {
			t.Errorf("replay %d status = %d, want %d", i, again.Code, first.Code)
		}
		if again.Body.String() != first.Body.String() {
			t.Errorf("replay %d body = %q, want %q", i, again.Body.String(), first.Body.String())
		}
		if again.Header().Get("Idempotent-Replay") != "true" {
			t.Errorf("replay %d has no Idempotent-Replay header", i)
		}
	}

	if h.calls.get() != 1 {
		t.Errorf("the handler ran %d times, want 1", h.calls.get())
	}
}

// A reordered body is the same request. Rejecting it would break every client whose
// language serializes maps in nondeterministic order.
func TestReorderedBodyIsTheSameRequest(t *testing.T) {
	var n int
	h := newMWHarness(t, okHandler(&n))

	first := h.post("key-1", `{"amount":100,"currency":"USD"}`)
	again := h.post("key-1", `{"currency":"USD","amount":100}`)

	if again.Code != first.Code || again.Body.String() != first.Body.String() {
		t.Error("a reordered body was treated as a different request")
	}
	if h.calls.get() != 1 {
		t.Errorf("the handler ran %d times, want 1", h.calls.get())
	}
}

// A key reused for a different request is a client bug. Replaying the first response
// would answer a question the client did not ask.
func TestSameKeyDifferentBodyIsRejected(t *testing.T) {
	var n int
	h := newMWHarness(t, okHandler(&n))

	h.post("key-1", `{"amount":100}`)
	rec := h.post("key-1", `{"amount":999}`)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if h.calls.get() != 1 {
		t.Errorf("the handler ran %d times, want 1", h.calls.get())
	}
}

// Fifty concurrent retries of the same request must produce exactly one execution. Every
// other attempt is a replay or a conflict — there is no third outcome.
func TestConcurrentSameKeyExecutesOnce(t *testing.T) {
	const attempts = 50

	var n int
	var mu sync.Mutex
	h := newMWHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()

		// Long enough that the other attempts genuinely overlap.
		time.Sleep(20 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	var (
		wg        sync.WaitGroup
		statuses  = make([]int, attempts)
		startGate = make(chan struct{})
	)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-startGate
			statuses[i] = h.post("key-1", `{"amount":100}`).Code
		}(i)
	}
	close(startGate)
	wg.Wait()

	if h.calls.get() != 1 {
		t.Fatalf("the handler ran %d times, want exactly 1", h.calls.get())
	}

	var created, conflict, replay, other int
	for _, status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		case http.StatusOK:
			replay++
		default:
			other++
		}
	}

	if created != 1 {
		t.Errorf("%d attempts got a 201, want exactly 1", created)
	}
	if other != 0 {
		t.Errorf("%d attempts got an unexpected status", other)
	}
	if created+conflict+replay != attempts {
		t.Errorf("statuses do not account for every attempt: %d+%d+%d != %d",
			created, conflict, replay, attempts)
	}
}

// A live holder means the caller should retry shortly, not that the request failed.
func TestInProgressReturnsConflict(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	var n int
	h := newMWHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		okHandler(&n)(w, nil)
	})

	go func() { h.post("key-1", `{"amount":100}`) }()
	<-started

	rec := h.post("key-1", `{"amount":100}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 409 carried no Retry-After header")
	}

	close(release)
}

// A holder that crashed leaves an expired lease. The next retry takes it over and
// genuinely re-executes, rather than waiting forever on a request that will never finish.
func TestExpiredLockIsTakenOver(t *testing.T) {
	var n int
	h := newMWHarness(t, okHandler(&n))

	// Claim the key without completing it, as a crashed request would leave it.
	rec := &Record{
		ID: "idem_stale", MerchantID: testMerchant, Key: "key-1",
		Method: http.MethodPost, Path: "/v1/transfers",
		State: StateInProgress, RecoveryPoint: PointStarted,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(TTL),
	}
	hash, err := Fingerprint(http.MethodPost, "/v1/transfers", []byte(`{"amount":100}`))
	if err != nil {
		t.Fatalf("Fingerprint = %v", err)
	}
	rec.RequestHash = hash

	lease := time.Now().UTC().Add(LeaseDuration)
	rec.LockExpiresAt = &lease
	if _, _, err := h.store.Acquire(context.Background(), rec); err != nil {
		t.Fatalf("seeding the store: %v", err)
	}

	// While the lease is live, a retry conflicts.
	if got := h.post("key-1", `{"amount":100}`).Code; got != http.StatusConflict {
		t.Fatalf("status with a live lease = %d, want 409", got)
	}

	h.store.expire(testMerchant, "key-1")

	if got := h.post("key-1", `{"amount":100}`).Code; got != http.StatusCreated {
		t.Fatalf("status after the lease expired = %d, want 201", got)
	}
	if h.calls.get() != 1 {
		t.Errorf("the handler ran %d times, want 1", h.calls.get())
	}
}

// A 5xx must not lock the key. The operation may never have happened, and replaying a
// 500 forever would strand the client with no way to make progress.
func TestServerErrorDoesNotLockTheKey(t *testing.T) {
	var attempt int
	h := newMWHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		attempt++
		if attempt == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"transient"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	if got := h.post("key-1", `{"amount":100}`).Code; got != http.StatusInternalServerError {
		t.Fatalf("first status = %d, want 500", got)
	}

	second := h.post("key-1", `{"amount":100}`)
	if second.Code != http.StatusCreated {
		t.Fatalf("retry after a 500 = %d, want 201; the key was locked by a server error", second.Code)
	}
	if second.Header().Get("Idempotent-Replay") == "true" {
		t.Error("the retry replayed the 500 instead of re-executing")
	}
	if h.calls.get() != 2 {
		t.Errorf("the handler ran %d times, want 2", h.calls.get())
	}
}

// A 4xx is a real outcome and is replayed like any other, because re-running the request
// would produce the same rejection.
func TestClientErrorIsRecordedAndReplayed(t *testing.T) {
	h := newMWHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"insufficient_funds"}`))
	})

	first := h.post("key-1", `{"amount":100}`)
	if first.Code != http.StatusUnprocessableEntity {
		t.Fatalf("first status = %d, want 422", first.Code)
	}

	second := h.post("key-1", `{"amount":100}`)
	if second.Code != http.StatusUnprocessableEntity {
		t.Errorf("replay status = %d, want 422", second.Code)
	}
	if second.Header().Get("Idempotent-Replay") != "true" {
		t.Error("the 4xx was not replayed")
	}
	if h.calls.get() != 1 {
		t.Errorf("the handler ran %d times, want 1", h.calls.get())
	}
}

// Keys are merchant-scoped, so one tenant cannot collide with or probe another's.
func TestKeysAreScopedByMerchant(t *testing.T) {
	store := newMemStore()
	calls := &int32Counter{}

	var merchant string
	mw := New(store,
		func(*http.Request) (string, bool) { return merchant, true },
		writeErrJSON,
	)
	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.inc()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/transfers", strings.NewReader(`{"amount":100}`))
		req.Header.Set("Idempotency-Key", "shared-key")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	merchant = "mer_a"
	if got := post().Code; got != http.StatusCreated {
		t.Fatalf("merchant A status = %d, want 201", got)
	}

	merchant = "mer_b"
	if got := post().Code; got != http.StatusCreated {
		t.Fatalf("merchant B was blocked by merchant A's key: status = %d, want 201", got)
	}

	if calls.get() != 2 {
		t.Errorf("the handler ran %d times, want 2 (once per merchant)", calls.get())
	}
}

// The handler still has to be able to read the body after the middleware hashed it.
func TestBodyIsReadableByTheHandler(t *testing.T) {
	var seen string
	h := newMWHarness(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("the handler could not read the body: %v", err)
		}
		seen, _ = body["currency"].(string)
		w.WriteHeader(http.StatusCreated)
	})

	h.post("key-1", `{"amount":100,"currency":"USD"}`)
	if seen != "USD" {
		t.Errorf("the handler read currency = %q, want USD", seen)
	}
}

func TestMalformedBodyIsRejectedBeforeTheHandler(t *testing.T) {
	var n int
	h := newMWHarness(t, okHandler(&n))

	if got := h.post("key-1", `{not json`).Code; got != http.StatusInternalServerError {
		// writeErrJSON maps an unrecognized error to 500; the API's real envelope maps
		// it to 400. What matters here is that the handler never ran.
		t.Logf("status = %d", got)
	}
	if h.calls.get() != 0 {
		t.Error("the handler ran with a body that could not be fingerprinted")
	}
}

// Complete inside a transaction must stop the middleware writing the record again
// afterwards — doing so outside that transaction would reopen the window the design
// closes.
func TestCompleteInsideTransactionIsNotOverwritten(t *testing.T) {
	store := newMemStore()

	mw := New(store,
		func(*http.Request) (string, bool) { return testMerchant, true },
		writeErrJSON,
	)

	handler := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := []byte(`{"id":"txn_1","recorded":"in-transaction"}`)
		if err := Complete(r.Context(), store, http.StatusCreated, body, "txn_1"); err != nil {
			t.Errorf("Complete = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/transfers", strings.NewReader(`{"amount":100}`))
	req.Header.Set("Idempotency-Key", "key-1")
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	rec, err := store.Get(context.Background(), testMerchant, "key-1")
	if err != nil {
		t.Fatalf("Get = %v", err)
	}
	if rec.ResourceID != "txn_1" {
		t.Errorf("resource_id = %q, want txn_1; the transactional record was overwritten", rec.ResourceID)
	}
	if rec.RecoveryPoint != PointFinished {
		t.Errorf("recovery_point = %q, want finished", rec.RecoveryPoint)
	}
	if !strings.Contains(string(rec.ResponseBody), "in-transaction") {
		t.Errorf("stored body = %s", rec.ResponseBody)
	}
}

// Complete on a request that is not idempotent must be a no-op rather than an error, so
// a shared handler works on both kinds of route.
func TestCompleteOutsideAnIdempotentRequestIsANoOp(t *testing.T) {
	if err := Complete(context.Background(), newMemStore(), http.StatusOK, nil, ""); err != nil {
		t.Errorf("Complete outside an idempotent request = %v, want nil", err)
	}
}

func TestRecordHelpers(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()

	live := now.Add(time.Minute)
	rec := &Record{State: StateInProgress, LockExpiresAt: &live}
	if rec.IsTerminal() {
		t.Error("an in-progress record reported terminal")
	}
	if rec.LockExpired(now) {
		t.Error("a live lease reported expired")
	}

	past := now.Add(-time.Minute)
	rec.LockExpiresAt = &past
	if !rec.LockExpired(now) {
		t.Error("an expired lease reported live")
	}

	rec.LockExpiresAt = nil
	if !rec.LockExpired(now) {
		t.Error("a record with no lease reported live")
	}

	for _, state := range []State{StateSucceeded, StateFailed} {
		if !(&Record{State: state}).IsTerminal() {
			t.Errorf("%s did not report terminal", state)
		}
	}
}
