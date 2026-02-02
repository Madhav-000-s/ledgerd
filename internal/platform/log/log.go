// Package log builds the process logger and carries it through context.
//
// Every line carries request_id, merchant_id and route where they are known. Money
// amounts are logged as integers with their currency, never as formatted strings, so
// that a log search for an exact amount is possible. Secrets never reach a log line:
// types that hold them implement slog.LogValuer and render as [REDACTED], which makes
// redaction a property of the type rather than a rule contributors must remember.
package log

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

// Redacted is the placeholder rendered in place of any secret value.
const Redacted = "[REDACTED]"

type ctxKey struct{}

// Standard attribute keys. Using constants keeps them greppable and stops "requestID"
// and "request_id" from both appearing in the same index.
const (
	KeyRequestID  = "request_id"
	KeyMerchantID = "merchant_id"
	KeyRoute      = "route"
	KeyAccountID  = "account_id"
	KeyTxnID      = "transaction_id"
	KeyEventID    = "event_id"
	KeyDeliveryID = "delivery_id"
	KeyEndpointID = "endpoint_id"
	KeyAttempt    = "attempt"
	KeyOutcome    = "outcome"
	KeyErr        = "error"
)

// Options configures the process logger.
type Options struct {
	Level  string // debug | info | warn | error
	Format string // json | text
	Source bool   // annotate lines with file:line
}

// New builds a logger writing to w.
func New(w io.Writer, opts Options) *slog.Logger {
	handlerOpts := &slog.HandlerOptions{
		Level:     parseLevel(opts.Level),
		AddSource: opts.Source,
	}

	var h slog.Handler
	if strings.EqualFold(opts.Format, "text") {
		h = slog.NewTextHandler(w, handlerOpts)
	} else {
		h = slog.NewJSONHandler(w, handlerOpts)
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Into returns a context carrying l.
func Into(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// From returns the logger carried by ctx, or the default logger if there is none.
// It never returns nil, so callers can log unconditionally.
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// With returns a context whose logger carries the supplied attributes, so that a
// request-scoped field set once is present on every subsequent line.
func With(ctx context.Context, args ...any) context.Context {
	return Into(ctx, From(ctx).With(args...))
}

// Err wraps an error as an attribute. It tolerates a nil error so call sites do not
// need to branch.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.String(KeyErr, err.Error())
}

// Money renders an amount as an integer in minor units alongside its currency, never
// as a formatted decimal string.
func Money(key string, minor int64, currency string) slog.Attr {
	return slog.Group(key,
		slog.Int64("amount_minor", minor),
		slog.String("currency", currency),
	)
}

// Secret wraps a sensitive string so that it renders as [REDACTED] wherever it is
// logged. It is a defence against accident, not against a determined caller: the
// underlying value is still reachable through Reveal.
type Secret string

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(Redacted) }

// String implements fmt.Stringer so that %s and %v are also safe.
func (s Secret) String() string { return Redacted }

// Reveal returns the underlying value. Every call site should be obviously correct.
func (s Secret) Reveal() string { return string(s) }
