package log

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %s", err, buf.String())
	}
	return m
}

func TestNewJSONHandler(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, Options{Level: "info", Format: "json"})

	l.Info("posted", slog.String(KeyTxnID, "txn_123"))

	m := decode(t, &buf)
	if m["msg"] != "posted" {
		t.Errorf("msg = %v, want \"posted\"", m["msg"])
	}
	if m[KeyTxnID] != "txn_123" {
		t.Errorf("%s = %v, want \"txn_123\"", KeyTxnID, m[KeyTxnID])
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, Options{Level: "warn", Format: "json"})

	l.Info("should not appear")
	if buf.Len() != 0 {
		t.Errorf("info line emitted at warn level: %s", buf.String())
	}

	l.Warn("should appear")
	if buf.Len() == 0 {
		t.Error("warn line suppressed at warn level")
	}
}

// A secret must render as [REDACTED] however it is logged: as a value, inside a group,
// or through fmt's stringer path.
func TestSecretIsRedacted(t *testing.T) {
	const raw = "whsec_supersecretvalue"

	var buf bytes.Buffer
	l := New(&buf, Options{Level: "info", Format: "json"})
	l.Info("endpoint created", slog.Any("secret", Secret(raw)))

	line := buf.String()
	if strings.Contains(line, raw) {
		t.Fatalf("secret leaked into log output: %s", line)
	}
	if !strings.Contains(line, Redacted) {
		t.Errorf("log line %q does not contain %s", line, Redacted)
	}

	if got := Secret(raw).String(); got != Redacted {
		t.Errorf("Secret.String() = %q, want %q", got, Redacted)
	}
	if got := Secret(raw).Reveal(); got != raw {
		t.Errorf("Secret.Reveal() = %q, want the original value", got)
	}
}

// Money is logged as an integer plus currency so that searching for an exact amount
// works, and so that no formatting decision can round it.
func TestMoneyAttrLogsMinorUnits(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, Options{Level: "info", Format: "json"})

	l.Info("charge", Money("amount", 9680, "USD"))

	m := decode(t, &buf)
	amount, ok := m["amount"].(map[string]any)
	if !ok {
		t.Fatalf("amount is not a group: %#v", m["amount"])
	}
	if amount["amount_minor"] != float64(9680) {
		t.Errorf("amount_minor = %v, want 9680", amount["amount_minor"])
	}
	if amount["currency"] != "USD" {
		t.Errorf("currency = %v, want USD", amount["currency"])
	}
	if strings.Contains(buf.String(), "96.80") {
		t.Error("money was formatted as a decimal string")
	}
}

func TestContextRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, Options{Level: "info", Format: "json"})

	ctx := Into(context.Background(), l)
	if From(ctx) != l {
		t.Error("From(Into(ctx, l)) did not return l")
	}

	// With must accumulate attributes onto every subsequent line.
	ctx = With(ctx, slog.String(KeyRequestID, "req_1"))
	ctx = With(ctx, slog.String(KeyMerchantID, "mer_2"))
	From(ctx).Info("handled")

	m := decode(t, &buf)
	if m[KeyRequestID] != "req_1" {
		t.Errorf("%s = %v, want req_1", KeyRequestID, m[KeyRequestID])
	}
	if m[KeyMerchantID] != "mer_2" {
		t.Errorf("%s = %v, want mer_2", KeyMerchantID, m[KeyMerchantID])
	}
}

// From must never return nil, so that call sites can log unconditionally.
func TestFromEmptyContext(t *testing.T) {
	if From(context.Background()) == nil {
		t.Fatal("From(context.Background()) = nil, want the default logger")
	}
	//nolint:staticcheck // deliberately exercising the nil-context guard
	if From(context.TODO()) == nil {
		t.Fatal("From(context.TODO()) = nil, want the default logger")
	}
}

func TestErrAttr(t *testing.T) {
	if got := Err(nil); got.Key != "" {
		t.Errorf("Err(nil) = %v, want the empty attr", got)
	}

	attr := Err(errors.New("insufficient funds"))
	if attr.Key != KeyErr {
		t.Errorf("key = %q, want %q", attr.Key, KeyErr)
	}
	if attr.Value.String() != "insufficient funds" {
		t.Errorf("value = %q, want \"insufficient funds\"", attr.Value.String())
	}
}
