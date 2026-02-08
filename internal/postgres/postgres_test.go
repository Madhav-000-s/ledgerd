package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
)

// These cover the parts of the persistence layer that are pure functions. Everything
// that depends on Postgres semantics — the locking order, the balance trigger, the
// append-only rule — is tested against a real database in test/integration, because a
// mock would only validate the mock.

func TestInTxReportsContextState(t *testing.T) {
	if InTx(context.Background()) {
		t.Error("InTx(background) = true, want false")
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]any
	}{
		{"nil", nil},
		{"empty", map[string]any{}},
		{"populated", map[string]any{"reverses": "txn_1", "reason": "disputed"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := marshalMetadata(tc.in)
			if err != nil {
				t.Fatalf("marshalMetadata = %v", err)
			}
			got, err := unmarshalMetadata(b)
			if err != nil {
				t.Fatalf("unmarshalMetadata = %v", err)
			}
			if got == nil {
				t.Fatal("unmarshalMetadata returned nil; callers index into it without a check")
			}
			if len(got) != len(tc.in) {
				t.Errorf("got %d keys, want %d", len(got), len(tc.in))
			}
			for k, v := range tc.in {
				if got[k] != v {
					t.Errorf("key %q = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestUnmarshalMetadataHandlesEmptyBytes(t *testing.T) {
	got, err := unmarshalMetadata(nil)
	if err != nil {
		t.Fatalf("unmarshalMetadata(nil) = %v", err)
	}
	if got == nil {
		t.Error("unmarshalMetadata(nil) returned a nil map")
	}
}

func TestUnmarshalMetadataRejectsGarbage(t *testing.T) {
	if _, err := unmarshalMetadata([]byte("{not json")); err == nil {
		t.Error("unmarshalMetadata accepted malformed JSON")
	}
}

// The domain uses "" for an unset reference; the partial unique index on reverses_id
// only behaves as intended if that becomes SQL NULL rather than an empty string.
func TestNullString(t *testing.T) {
	if got := nullString(""); got != nil {
		t.Errorf("nullString(\"\") = %v, want nil", got)
	}
	got := nullString("txn_1")
	if got == nil || *got != "txn_1" {
		t.Errorf("nullString(\"txn_1\") = %v, want a pointer to txn_1", got)
	}

	if got := derefString(nil); got != "" {
		t.Errorf("derefString(nil) = %q, want \"\"", got)
	}
	s := "txn_2"
	if got := derefString(&s); got != "txn_2" {
		t.Errorf("derefString(&s) = %q, want txn_2", got)
	}
}

func TestTimestamptzConversions(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()

	ts := timestamptz(at)
	if !ts.Valid || !ts.Time.Equal(at) {
		t.Errorf("timestamptz(%v) = %+v", at, ts)
	}

	if got := timestamptzPtr(nil); got.Valid {
		t.Error("timestamptzPtr(nil) is Valid, want NULL")
	}
	if got := timestamptzPtr(&at); !got.Valid || !got.Time.Equal(at) {
		t.Errorf("timestamptzPtr(&at) = %+v", got)
	}
}

// The constraint trigger is the layer that catches what the Go code missed. Its raw
// message must not reach a caller, so each invariant's distinct wording maps back to the
// domain error it corresponds to.
func TestWrapEntryErrorMapsTriggerViolations(t *testing.T) {
	tests := []struct {
		name    string
		code    string
		message string
		want    error
	}{
		{
			"unbalanced",
			codeCheckViolation,
			"txn txn_1 unbalanced: debits=100 credits=99",
			ledger.ErrUnbalanced,
		},
		{
			"multi currency",
			codeCheckViolation,
			"txn txn_1 spans 2 currencies",
			ledger.ErrMultiCurrency,
		},
		{
			"no entries",
			codeCheckViolation,
			"txn txn_1 has no entries",
			ledger.ErrNoEntries,
		},
		{
			"non-positive amount",
			codeCheckViolation,
			`new row for relation "entries" violates check constraint "entries_amount_minor_check"`,
			ledger.ErrInvalidAmount,
		},
		{
			"unrecognized check violation still maps to a domain error",
			codeCheckViolation,
			"something else entirely",
			ledger.ErrUnbalanced,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := wrapEntryError(&pgconn.PgError{Code: tc.code, Message: tc.message})
			if !errors.Is(err, tc.want) {
				t.Errorf("wrapEntryError(%q) = %v, want %v", tc.message, err, tc.want)
			}
		})
	}
}

func TestWrapEntryErrorPassesThroughOtherErrors(t *testing.T) {
	sentinel := errors.New("connection reset")

	err := wrapEntryError(sentinel)
	if !errors.Is(err, sentinel) {
		t.Errorf("wrapEntryError lost the underlying error: %v", err)
	}
	for _, domain := range []error{ledger.ErrUnbalanced, ledger.ErrMultiCurrency, ledger.ErrNoEntries} {
		if errors.Is(err, domain) {
			t.Errorf("a transport error was reported as %v", domain)
		}
	}
}

func TestMillis(t *testing.T) {
	if got := millis(3 * time.Second); got != "3000" {
		t.Errorf("millis(3s) = %q, want \"3000\"", got)
	}
	if got := millis(10 * time.Second); got != "10000" {
		t.Errorf("millis(10s) = %q, want \"10000\"", got)
	}
}
