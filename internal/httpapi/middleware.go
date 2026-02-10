package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/platform/id"
	"github.com/Madhav-000-s/ledgerd/internal/platform/log"
)

type ctxKeyRequestID struct{}

// RequestIDFrom returns the request id carried by ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID{}).(string)
	return v
}

// requestID assigns every request an identifier, echoes it in a header and puts it on
// every log line for the request.
//
// A client-supplied Request-Id is deliberately ignored. Accepting one lets a caller
// collide with or forge another tenant's identifier, and the value's only job is to
// join a support conversation to a log line.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := id.New(id.PrefixRequest)

		ctx := context.WithValue(r.Context(), ctxKeyRequestID{}, rid)
		ctx = log.With(ctx, slog.String(log.KeyRequestID, rid))

		w.Header().Set("Request-Id", rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// statusWriter records the status and byte count for logging and metrics.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, so wrapping does not
// silently disable flushing or hijacking.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// structuredLogger logs one line per request, after it completes.
func structuredLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		sw := &statusWriter{ResponseWriter: w}

		next.ServeHTTP(sw, r)

		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		log.From(r.Context()).Info("request",
			slog.String("method", r.Method),
			slog.String(log.KeyRoute, routePattern(r)),
			slog.Int("status", sw.status),
			slog.Int("bytes", sw.bytes),
			slog.Duration("duration", time.Since(started)),
		)
	})
}

// recoverer turns a panic into a 500 rather than a dropped connection.
//
// The panic value is logged and never sent: a panic message routinely contains a nil
// pointer's type, a file path or a fragment of a query.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// A client disconnecting mid-write surfaces as a panic with this value and
			// is not a server fault; there is also nobody left to answer.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}

			log.From(r.Context()).Error("panic recovered",
				slog.Any("panic", rec),
				slog.String("stack", stack()),
			)
			writeError(w, r, Errorf(http.StatusInternalServerError, TypeAPI, "internal_error",
				"An unexpected error occurred. The request id identifies this failure in our logs."))
		}()

		next.ServeHTTP(w, r)
	})
}

// bodyLimit caps the request body.
//
// It sits before idempotency because the idempotency layer hashes the body, and hashing
// an unbounded body is a denial-of-service waiting to happen.
func bodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

// timeout abandons a request that has run too long, so a stuck handler cannot hold a
// connection — or a row lock — indefinitely.
func timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// rateLimiter is a per-key token bucket.
//
// In-process, and therefore correct only for a single replica. This is a documented
// limitation rather than a quiet inaccuracy: with N replicas a caller gets N times the
// intended allowance. Making it correct when scaled out means moving the counter into
// Postgres or Redis, which is deferred (DESIGN.md Q7).
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	perMin  int
	now     func() time.Time
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

func newRateLimiter(perMin int) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*bucket),
		perMin:  perMin,
		now:     time.Now,
	}
}

// allow reports whether the key may proceed, consuming a token if so.
func (l *rateLimiter) allow(key string) bool {
	if l.perMin <= 0 {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	capacity := float64(l.perMin)
	refillPerSecond := capacity / 60

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: capacity, lastSeen: now}
		l.buckets[key] = b
	} else {
		elapsed := now.Sub(b.lastSeen).Seconds()
		b.tokens = minFloat(capacity, b.tokens+elapsed*refillPerSecond)
		b.lastSeen = now
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets untouched for longer than idle, so a service handling many
// short-lived keys does not accumulate them forever.
func (l *rateLimiter) sweep(idle time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := l.now().Add(-idle)
	for key, b := range l.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(l.buckets, key)
		}
	}
}

// middleware applies the limit, keyed by API key so that one noisy merchant cannot
// exhaust another's allowance.
func (l *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := "anonymous"
		if auth, ok := AuthFrom(r.Context()); ok {
			key = auth.KeyID
		}

		if !l.allow(key) {
			w.Header().Set("Retry-After", strconv.Itoa(1))
			writeError(w, r, errRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
