package httpapi

import (
	"context"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/platform/config"
)

// TxRunner composes several service calls into one database transaction. The HTTP layer
// only ever hands it a closure; it never opens or commits a transaction itself.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Health reports whether dependencies are reachable.
type Health interface {
	Ping(ctx context.Context) error
}

// Server holds everything the handlers need.
type Server struct {
	cfg     config.HTTPConfig
	ledger  ledger.Service
	tx      TxRunner
	keys    APIKeyStore
	health  Health
	limiter *rateLimiter
	handler http.Handler
}

// Options are the server's collaborators.
type Options struct {
	Config config.HTTPConfig
	Ledger ledger.Service
	Tx     TxRunner
	Keys   APIKeyStore
	Health Health
}

// NewServer wires the router.
func NewServer(opts Options) *Server {
	s := &Server{
		cfg:     opts.Config,
		ledger:  opts.Ledger,
		tx:      opts.Tx,
		keys:    opts.Keys,
		health:  opts.Health,
		limiter: newRateLimiter(opts.Config.RateLimitPerMin),
	}
	s.handler = s.routes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// routes builds the router and the middleware chain.
//
// The order in the chain is load-bearing, not stylistic:
//
//	requestID    first, so every later line and every error carries it
//	realIP       before logging, or every request appears to come from the proxy
//	recoverer    outside the handler but inside logging, so a panic is still logged
//	logger       wraps the writer to capture the status
//	timeout      before anything that can block on the database
//	bodyLimit    before auth, so an unauthenticated caller cannot stream forever
//	auth         before rate limiting, because the limit is per key
//	rateLimit    before the handler
//
// The idempotency middleware inserted in a later phase belongs after auth (its keys are
// merchant-scoped) and after bodyLimit (it hashes the body, which must be bounded).
func (s *Server) routes() http.Handler {
	r := chi.NewRouter()

	r.Use(requestID)
	r.Use(middleware.RealIP)
	r.Use(recoverer)
	r.Use(structuredLogger)
	r.Use(timeout(s.cfg.HandlerTimeout))
	r.Use(bodyLimit(s.cfg.MaxBodyBytes))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, errNotFound)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, errMethodNotAllowed)
	})

	// Operational endpoints are unauthenticated and outside the rate limiter: a probe
	// that can be rate limited will eventually take a healthy service out of rotation.
	r.Get("/healthz", s.handleLive)
	r.Get("/readyz", s.handleReady)

	r.Route("/v1", func(r chi.Router) {
		r.Use(authenticate(s.keys))
		r.Use(s.limiter.middleware)

		r.Group(func(r chi.Router) {
			r.Use(requireRole(RoleRead))

			r.Get("/accounts", s.handleListAccounts)
			r.Get("/accounts/{id}", s.handleGetAccount)
			r.Get("/accounts/{id}/balance", s.handleGetBalance)
			r.Get("/accounts/{id}/entries", s.handleListEntries)
			r.Get("/transactions/{id}", s.handleGetTransaction)
		})

		r.Group(func(r chi.Router) {
			r.Use(requireRole(RoleWrite))

			r.Post("/accounts", s.handleCreateAccount)
			r.Post("/transfers", s.handleCreateTransfer)
			r.Post("/transactions/{id}/reverse", s.handleReverseTransaction)
		})
	})

	return r
}

// routePattern returns the matched route template rather than the concrete path, so that
// metrics and logs group by endpoint instead of exploding into one series per account id.
func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if p := rctx.RoutePattern(); p != "" {
			return p
		}
	}
	return "unmatched"
}

func stack() string { return string(debug.Stack()) }

// handleLive answers without touching any dependency. Liveness must not fail because the
// database is briefly unreachable, or an orchestrator will restart a process that is
// working perfectly and would have recovered on its own.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "ok"})
}

// handleReady pings the database, because a process that cannot reach it should be taken
// out of rotation rather than serve errors.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if s.health != nil {
		if err := s.health.Ping(ctx); err != nil {
			writeJSON(w, r, http.StatusServiceUnavailable, map[string]any{
				"status": "unavailable",
				"reason": "database unreachable",
			})
			return
		}
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "ok"})
}
