// Package config loads process configuration from the environment.
//
// Configuration is read once at startup and passed explicitly down the call graph.
// Nothing in this repository reads os.Getenv outside of this package, so the full set
// of knobs a deployment can turn is the set of fields on Config.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Isolation selects the ledger's concurrency strategy. Ordered row locking under READ
// COMMITTED is the production setting; SERIALIZABLE exists so the concurrency suite can
// assert that both strategies produce identical final state (see DESIGN.md §6.2).
type Isolation string

const (
	IsolationReadCommitted Isolation = "read_committed"
	IsolationSerializable  Isolation = "serializable"
)

// Config is the complete runtime configuration for both binaries.
type Config struct {
	Env  string // "dev" | "staging" | "prod"
	HTTP HTTPConfig
	DB   DBConfig
	Log  LogConfig

	Ledger   LedgerConfig
	Webhooks WebhooksConfig
	Worker   WorkerConfig
}

type HTTPConfig struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	HandlerTimeout  time.Duration
	ShutdownTimeout time.Duration
	MaxBodyBytes    int64
	RateLimitPerMin int
}

type DBConfig struct {
	URL              string
	MaxConns         int32
	MinConns         int32
	MaxConnLifetime  time.Duration
	MaxConnIdleTime  time.Duration
	ConnectTimeout   time.Duration
	LockTimeout      time.Duration
	StatementTimeout time.Duration
}

type LogConfig struct {
	Level  string // debug | info | warn | error
	Format string // json | text
}

type LedgerConfig struct {
	Isolation Isolation
	// AuthorizationTTL is how long a pending authorization survives before the expiry
	// sweep releases the hold.
	AuthorizationTTL time.Duration
	// RefundPercentageFee reports whether the percentage component of a fee is returned
	// on refund. The fixed component is always retained.
	RefundPercentageFee bool
}

type WebhooksConfig struct {
	// AllowInsecure permits plain http:// endpoints. Local testing only — see
	// DESIGN.md §8.5, this is half of the SSRF story.
	AllowInsecure bool
	// AllowPrivateIPs disables the dial-time private-range guard. Local testing only.
	AllowPrivateIPs bool

	BackoffBase time.Duration
	BackoffCap  time.Duration
	MaxAttempts int

	RequestTimeout time.Duration
	ConnectTimeout time.Duration
	MaxRespBytes   int64

	// SecretKey is the 32-byte AES-GCM key used to encrypt endpoint secrets at rest,
	// hex-encoded in the environment.
	SecretKey string
}

type WorkerConfig struct {
	DispatchBatchSize  int
	DispatchPollPeriod time.Duration
	DeliveryBatchSize  int
	DeliveryWorkers    int
	DeliveryLease      time.Duration
	ReconcilePeriod    time.Duration
	ReapPeriod         time.Duration
	ReapBatchSize      int
	IdempotencyTTL     time.Duration
}

// Load reads configuration from the environment, applying defaults, and validates it.
// It returns every problem it finds at once rather than the first, because fixing a
// deployment's environment one restart at a time is miserable.
func Load() (*Config, error) {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	cfg := &Config{
		Env: envStr("LEDGERD_ENV", "dev"),
		HTTP: HTTPConfig{
			Addr:            envStr("HTTP_ADDR", ":8080"),
			ReadTimeout:     envDur("HTTP_READ_TIMEOUT", 10*time.Second, fail),
			WriteTimeout:    envDur("HTTP_WRITE_TIMEOUT", 30*time.Second, fail),
			IdleTimeout:     envDur("HTTP_IDLE_TIMEOUT", 120*time.Second, fail),
			HandlerTimeout:  envDur("HTTP_HANDLER_TIMEOUT", 30*time.Second, fail),
			ShutdownTimeout: envDur("HTTP_SHUTDOWN_TIMEOUT", 30*time.Second, fail),
			MaxBodyBytes:    int64(envInt("HTTP_MAX_BODY_BYTES", 1<<20, fail)),
			RateLimitPerMin: envInt("HTTP_RATE_LIMIT_PER_MIN", 600, fail),
		},
		DB: DBConfig{
			URL:              envStr("DATABASE_URL", ""),
			MaxConns:         int32(envInt("DB_MAX_CONNS", 20, fail)),
			MinConns:         int32(envInt("DB_MIN_CONNS", 2, fail)),
			MaxConnLifetime:  envDur("DB_MAX_CONN_LIFETIME", time.Hour, fail),
			MaxConnIdleTime:  envDur("DB_MAX_CONN_IDLE_TIME", 30*time.Minute, fail),
			ConnectTimeout:   envDur("DB_CONNECT_TIMEOUT", 5*time.Second, fail),
			LockTimeout:      envDur("DB_LOCK_TIMEOUT", 3*time.Second, fail),
			StatementTimeout: envDur("DB_STATEMENT_TIMEOUT", 10*time.Second, fail),
		},
		Log: LogConfig{
			Level:  strings.ToLower(envStr("LOG_LEVEL", "info")),
			Format: strings.ToLower(envStr("LOG_FORMAT", "json")),
		},
		Ledger: LedgerConfig{
			Isolation:           Isolation(strings.ToLower(envStr("LEDGER_ISOLATION", string(IsolationReadCommitted)))),
			AuthorizationTTL:    envDur("LEDGER_AUTHORIZATION_TTL", 7*24*time.Hour, fail),
			RefundPercentageFee: envBool("LEDGER_REFUND_PERCENTAGE_FEE", true, fail),
		},
		Webhooks: WebhooksConfig{
			AllowInsecure:   envBool("ALLOW_INSECURE_WEBHOOKS", false, fail),
			AllowPrivateIPs: envBool("ALLOW_PRIVATE_IP_WEBHOOKS", false, fail),
			BackoffBase:     envDur("WEBHOOK_BACKOFF_BASE", 10*time.Second, fail),
			BackoffCap:      envDur("WEBHOOK_BACKOFF_CAP", 6*time.Hour, fail),
			MaxAttempts:     envInt("WEBHOOK_MAX_ATTEMPTS", 16, fail),
			RequestTimeout:  envDur("WEBHOOK_REQUEST_TIMEOUT", 5*time.Second, fail),
			ConnectTimeout:  envDur("WEBHOOK_CONNECT_TIMEOUT", 2*time.Second, fail),
			MaxRespBytes:    int64(envInt("WEBHOOK_MAX_RESP_BYTES", 64<<10, fail)),
			SecretKey:       envStr("WEBHOOK_SECRET_KEY", ""),
		},
		Worker: WorkerConfig{
			DispatchBatchSize:  envInt("WORKER_DISPATCH_BATCH", 100, fail),
			DispatchPollPeriod: envDur("WORKER_DISPATCH_POLL", time.Second, fail),
			DeliveryBatchSize:  envInt("WORKER_DELIVERY_BATCH", 50, fail),
			DeliveryWorkers:    envInt("WORKER_DELIVERY_WORKERS", 8, fail),
			DeliveryLease:      envDur("WORKER_DELIVERY_LEASE", 60*time.Second, fail),
			ReconcilePeriod:    envDur("WORKER_RECONCILE_PERIOD", 5*time.Minute, fail),
			ReapPeriod:         envDur("WORKER_REAP_PERIOD", 5*time.Minute, fail),
			ReapBatchSize:      envInt("WORKER_REAP_BATCH", 1000, fail),
			IdempotencyTTL:     envDur("IDEMPOTENCY_TTL", 24*time.Hour, fail),
		},
	}

	if err := cfg.validate(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("config: %w", errors.Join(errs...))
	}
	return cfg, nil
}

func (c *Config) validate() error {
	var errs []error

	if c.DB.URL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	} else if _, err := url.Parse(c.DB.URL); err != nil {
		errs = append(errs, fmt.Errorf("DATABASE_URL is not a valid URL: %w", err))
	}

	switch c.Ledger.Isolation {
	case IsolationReadCommitted, IsolationSerializable:
	default:
		errs = append(errs, fmt.Errorf("LEDGER_ISOLATION must be %q or %q, got %q",
			IsolationReadCommitted, IsolationSerializable, c.Ledger.Isolation))
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("LOG_LEVEL must be debug|info|warn|error, got %q", c.Log.Level))
	}

	switch c.Log.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("LOG_FORMAT must be json|text, got %q", c.Log.Format))
	}

	if c.DB.MinConns > c.DB.MaxConns {
		errs = append(errs, fmt.Errorf("DB_MIN_CONNS (%d) exceeds DB_MAX_CONNS (%d)", c.DB.MinConns, c.DB.MaxConns))
	}
	if c.Webhooks.MaxAttempts < 1 {
		errs = append(errs, fmt.Errorf("WEBHOOK_MAX_ATTEMPTS must be >= 1, got %d", c.Webhooks.MaxAttempts))
	}
	if c.Webhooks.BackoffBase <= 0 || c.Webhooks.BackoffCap < c.Webhooks.BackoffBase {
		errs = append(errs, errors.New("WEBHOOK_BACKOFF_CAP must be >= WEBHOOK_BACKOFF_BASE > 0"))
	}
	if c.Worker.DeliveryWorkers < 1 {
		errs = append(errs, fmt.Errorf("WORKER_DELIVERY_WORKERS must be >= 1, got %d", c.Worker.DeliveryWorkers))
	}

	if c.IsProd() {
		if c.Webhooks.AllowInsecure {
			errs = append(errs, errors.New("ALLOW_INSECURE_WEBHOOKS must not be set in prod"))
		}
		if c.Webhooks.AllowPrivateIPs {
			errs = append(errs, errors.New("ALLOW_PRIVATE_IP_WEBHOOKS must not be set in prod"))
		}
		if len(c.Webhooks.SecretKey) != 64 {
			errs = append(errs, errors.New("WEBHOOK_SECRET_KEY must be a 32-byte hex string in prod"))
		}
	}

	return errors.Join(errs...)
}

// IsProd reports whether this process is running in a production environment.
func (c *Config) IsProd() bool { return c.Env == "prod" }

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int, fail func(string, ...any)) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		fail("%s: %q is not an integer", key, v)
		return def
	}
	return n
}

func envDur(key string, def time.Duration, fail func(string, ...any)) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fail("%s: %q is not a duration (e.g. 30s, 5m, 6h)", key, v)
		return def
	}
	return d
}

func envBool(key string, def bool, fail func(string, ...any)) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		fail("%s: %q is not a boolean", key, v)
		return def
	}
	return b
}
