package config

import (
	"strings"
	"testing"
	"time"
)

// minimalEnv sets only what Load requires, so each test can layer its own overrides on
// top and assert on one thing at a time.
func minimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://ledgerd:ledgerd@localhost:5432/ledgerd?sslmode=disable")
}

func TestLoadDefaults(t *testing.T) {
	minimalEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("HTTP.Addr = %q, want \":8080\"", cfg.HTTP.Addr)
	}
	if cfg.Ledger.Isolation != IsolationReadCommitted {
		t.Errorf("Ledger.Isolation = %q, want %q", cfg.Ledger.Isolation, IsolationReadCommitted)
	}
	if cfg.DB.LockTimeout != 3*time.Second {
		t.Errorf("DB.LockTimeout = %v, want 3s", cfg.DB.LockTimeout)
	}
	if cfg.Webhooks.MaxAttempts != 16 {
		t.Errorf("Webhooks.MaxAttempts = %d, want 16", cfg.Webhooks.MaxAttempts)
	}
	// The SSRF guards must default to on; a missing env var must never open them.
	if cfg.Webhooks.AllowInsecure || cfg.Webhooks.AllowPrivateIPs {
		t.Error("webhook SSRF guards default to disabled, want enabled")
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil, want an error when DATABASE_URL is unset")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error %q does not mention DATABASE_URL", err)
	}
}

func TestLoadOverrides(t *testing.T) {
	minimalEnv(t)
	t.Setenv("HTTP_ADDR", ":9999")
	t.Setenv("LEDGER_ISOLATION", "serializable")
	t.Setenv("WEBHOOK_BACKOFF_BASE", "250ms")
	t.Setenv("WEBHOOK_BACKOFF_CAP", "5s")
	t.Setenv("WORKER_DELIVERY_WORKERS", "3")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	if cfg.HTTP.Addr != ":9999" {
		t.Errorf("HTTP.Addr = %q, want \":9999\"", cfg.HTTP.Addr)
	}
	if cfg.Ledger.Isolation != IsolationSerializable {
		t.Errorf("Ledger.Isolation = %q, want %q", cfg.Ledger.Isolation, IsolationSerializable)
	}
	if cfg.Webhooks.BackoffBase != 250*time.Millisecond {
		t.Errorf("Webhooks.BackoffBase = %v, want 250ms", cfg.Webhooks.BackoffBase)
	}
	if cfg.Worker.DeliveryWorkers != 3 {
		t.Errorf("Worker.DeliveryWorkers = %d, want 3", cfg.Worker.DeliveryWorkers)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
		want string
	}{
		{"unknown isolation", "LEDGER_ISOLATION", "snapshot", "LEDGER_ISOLATION"},
		{"unknown log level", "LOG_LEVEL", "verbose", "LOG_LEVEL"},
		{"unknown log format", "LOG_FORMAT", "xml", "LOG_FORMAT"},
		{"non-integer", "DB_MAX_CONNS", "many", "DB_MAX_CONNS"},
		{"non-duration", "DB_LOCK_TIMEOUT", "3 seconds", "DB_LOCK_TIMEOUT"},
		{"non-boolean", "ALLOW_INSECURE_WEBHOOKS", "sure", "ALLOW_INSECURE_WEBHOOKS"},
		{"zero attempts", "WEBHOOK_MAX_ATTEMPTS", "0", "WEBHOOK_MAX_ATTEMPTS"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			minimalEnv(t)
			t.Setenv(tc.key, tc.val)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() = nil, want an error for %s=%q", tc.key, tc.val)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %s", err, tc.want)
			}
		})
	}
}

// Load reports every problem at once. Fixing a deployment's environment one restart at
// a time is miserable, so this behavior is worth a test of its own.
func TestLoadReportsAllErrors(t *testing.T) {
	minimalEnv(t)
	t.Setenv("LOG_LEVEL", "verbose")
	t.Setenv("LEDGER_ISOLATION", "snapshot")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil, want an error")
	}
	for _, want := range []string{"LOG_LEVEL", "LEDGER_ISOLATION"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s; expected all problems reported at once", err, want)
		}
	}
}

// The local-development escape hatches must be impossible to leave on in production.
func TestProdRejectsInsecureWebhookSettings(t *testing.T) {
	minimalEnv(t)
	t.Setenv("LEDGERD_ENV", "prod")
	t.Setenv("ALLOW_INSECURE_WEBHOOKS", "true")
	t.Setenv("ALLOW_PRIVATE_IP_WEBHOOKS", "true")
	t.Setenv("WEBHOOK_SECRET_KEY", strings.Repeat("a", 64))

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil, want an error in prod with SSRF guards disabled")
	}
	for _, want := range []string{"ALLOW_INSECURE_WEBHOOKS", "ALLOW_PRIVATE_IP_WEBHOOKS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestProdRequiresWebhookSecretKey(t *testing.T) {
	minimalEnv(t)
	t.Setenv("LEDGERD_ENV", "prod")
	t.Setenv("WEBHOOK_SECRET_KEY", "tooshort")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil, want an error for a short WEBHOOK_SECRET_KEY in prod")
	}
	if !strings.Contains(err.Error(), "WEBHOOK_SECRET_KEY") {
		t.Errorf("error %q does not mention WEBHOOK_SECRET_KEY", err)
	}
}

func TestMinConnsCannotExceedMaxConns(t *testing.T) {
	minimalEnv(t)
	t.Setenv("DB_MIN_CONNS", "50")
	t.Setenv("DB_MAX_CONNS", "10")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil, want an error when DB_MIN_CONNS > DB_MAX_CONNS")
	}
}
