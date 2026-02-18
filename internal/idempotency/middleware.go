package idempotency

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/platform/id"
)

// LeaseDuration is how long a request may hold a key before another may take it over.
//
// Short enough that a crashed holder does not block a retry for long; long enough that a
// slow-but-alive request is not stolen from underneath itself. Multi-phase operations
// extend it on each phase.
const LeaseDuration = 60 * time.Second

// TTL is how long a completed record is replayable.
const TTL = 24 * time.Hour

// MerchantFunc extracts the tenant from a request. Keys are merchant-scoped, so this has
// to run after authentication.
type MerchantFunc func(*http.Request) (string, bool)

// ErrorWriter renders an error using the API's envelope. Passed in so this package does
// not import the HTTP layer, which would make the dependency circular.
type ErrorWriter func(w http.ResponseWriter, r *http.Request, err error)

// RequestIDFunc reads the transport's request id, so a replay can report which original
// request produced the stored response. Passed in rather than read from a header: a
// client-supplied id must never be trusted, and importing the HTTP layer here would make
// the dependency circular.
type RequestIDFunc func(context.Context) string

// Middleware applies idempotency to money-mutating routes.
type Middleware struct {
	store     Store
	merchant  MerchantFunc
	writeErr  ErrorWriter
	requestID RequestIDFunc
	now       func() time.Time
}

// New builds the middleware.
func New(store Store, merchant MerchantFunc, writeErr ErrorWriter, opts ...Option) *Middleware {
	m := &Middleware{
		store:     store,
		merchant:  merchant,
		writeErr:  writeErr,
		requestID: func(context.Context) string { return "" },
		now:       func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Option configures the middleware.
type Option func(*Middleware)

// WithClock replaces the clock, so lease expiry can be tested without sleeping.
func WithClock(now func() time.Time) Option {
	return func(m *Middleware) { m.now = now }
}

// WithRequestID supplies the accessor for the transport's request id.
func WithRequestID(fn RequestIDFunc) Option {
	return func(m *Middleware) { m.requestID = fn }
}

type ctxKeyRecord struct{}

// RecordFrom returns the record this request holds, if any.
func RecordFrom(ctx context.Context) (*Record, bool) {
	rec, ok := ctx.Value(ctxKeyRecord{}).(*Record)
	return rec, ok && rec != nil
}

// Handler wraps a money-mutating handler.
//
// The decision flow is DESIGN.md §7.2:
//
//	no key                       -> 400, the route requires one
//	key, no existing record      -> we own it; run the handler
//	key, different fingerprint   -> 422, the key was reused for another request
//	key, terminal record         -> replay the stored response
//	key, live lease              -> 409, retry shortly
//	key, expired lease           -> the holder crashed; take over and re-run
func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if err := ValidateKey(key); err != nil {
			m.writeErr(w, r, err)
			return
		}

		merchantID, ok := m.merchant(r)
		if !ok || merchantID == "" {
			// Unreachable while this sits after authentication in the chain, which it
			// must: the key is merchant-scoped.
			m.writeErr(w, r, ErrKeyRequired)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			m.writeErr(w, r, err)
			return
		}
		_ = r.Body.Close()
		// The handler still needs to read the body.
		r.Body = io.NopCloser(bytes.NewReader(body))

		hash, err := Fingerprint(r.Method, r.URL.Path, body)
		if err != nil {
			m.writeErr(w, r, err)
			return
		}

		now := m.now()
		candidate := &Record{
			ID:            id.New(id.PrefixIdempotency),
			MerchantID:    merchantID,
			Key:           key,
			Method:        r.Method,
			Path:          r.URL.Path,
			RequestHash:   hash,
			State:         StateInProgress,
			RecoveryPoint: PointStarted,
			LockExpiresAt: ptr(now.Add(LeaseDuration)),
			RequestID:     m.requestID(r.Context()),
			CreatedAt:     now,
			ExpiresAt:     now.Add(TTL),
		}

		rec, acquired, err := m.store.Acquire(r.Context(), candidate)
		if err != nil {
			m.writeErr(w, r, err)
			return
		}

		if !acquired {
			// The fingerprint check comes first. A key reused for a different request is
			// a client bug, and replaying the first response would answer a question the
			// client did not ask.
			if !bytes.Equal(rec.RequestHash, hash) {
				m.writeErr(w, r, ErrFingerprintMismatch)
				return
			}

			if rec.IsTerminal() {
				m.replay(w, r, rec)
				return
			}

			if !rec.LockExpired(now) {
				w.Header().Set("Retry-After", "1")
				m.writeErr(w, r, ErrInProgress)
				return
			}

			// The lease expired, so the previous holder crashed. Taking over is a
			// compare-and-set, so only one of several waiting retries wins.
			won, err := m.store.TakeOver(r.Context(), rec.ID, now.Add(LeaseDuration))
			if err != nil {
				m.writeErr(w, r, err)
				return
			}
			if !won {
				w.Header().Set("Retry-After", "1")
				m.writeErr(w, r, ErrInProgress)
				return
			}
			rec.LockExpiresAt = ptr(now.Add(LeaseDuration))
		}

		capture := &capturingWriter{ResponseWriter: w, body: &bytes.Buffer{}}
		ctx := context.WithValue(r.Context(), ctxKeyRecord{}, rec)

		next.ServeHTTP(capture, r.WithContext(ctx))

		m.finish(ctx, rec, capture)
	})
}

// finish records the outcome for handlers that did not record it themselves.
//
// A money-moving handler calls Complete inside its own transaction, so that the response
// and the ledger entries commit together; by the time control returns here the record is
// already terminal and this is a no-op. This path covers handlers with nothing to commit,
// and failures that never reached the transaction at all.
func (m *Middleware) finish(ctx context.Context, rec *Record, capture *capturingWriter) {
	if rec.CompletedInTx() {
		return
	}

	status := capture.status
	if status == 0 {
		status = http.StatusOK
	}

	// A 5xx is recorded as released rather than stored. A retry after a server error
	// must be allowed to genuinely re-execute: the operation may never have happened,
	// and replaying a 500 forever would strand the client.
	if status >= http.StatusInternalServerError {
		// Detached from the request context, which is usually already cancelled by the
		// time a request fails this way.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = m.store.Release(releaseCtx, rec.ID)
		return
	}

	state := StateSucceeded
	if status >= http.StatusBadRequest {
		state = StateFailed
	}

	completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = m.store.Complete(completeCtx, rec.ID, state, status, capture.body.Bytes(), "")
}

// replay returns the stored response byte for byte.
//
// The original status and body are reproduced exactly, plus headers that make the replay
// visible: a client that cannot tell a replay from a fresh execution cannot tell whether
// its retry was necessary.
func (m *Middleware) replay(w http.ResponseWriter, r *http.Request, rec *Record) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Idempotent-Replay", "true")
	if rec.RequestID != "" {
		w.Header().Set("Original-Request-Id", rec.RequestID)
	}

	status := rec.ResponseStatus
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(rec.ResponseBody)
}

// Complete records the response inside the caller's transaction.
//
// This is the ordering property the whole package exists for. Called from within the
// same transaction as the ledger write, it guarantees there is no instant at which the
// entries are committed but the response is not — so a crash leaves either nothing or
// everything, and the retry replays rather than re-charges.
func Complete(ctx context.Context, store Store, status int, body []byte, resourceID string) error {
	rec, ok := RecordFrom(ctx)
	if !ok {
		// Not an idempotent route. Nothing to record.
		return nil
	}

	state := StateSucceeded
	if status >= http.StatusBadRequest {
		state = StateFailed
	}

	if err := store.Complete(ctx, rec.ID, state, status, body, resourceID); err != nil {
		return err
	}
	rec.MarkCompletedInTx()
	return nil
}

// capturingWriter records the response so it can be persisted for replay.
type capturingWriter struct {
	http.ResponseWriter
	status int
	body   *bytes.Buffer
}

func (w *capturingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *capturingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

// Unwrap keeps http.ResponseController working through the wrapper.
func (w *capturingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func ptr[T any](v T) *T { return &v }
