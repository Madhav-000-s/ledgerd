package events

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// memStore is an in-memory outbox.
type memStore struct {
	appended  []*Event
	byID      map[string]*Event
	appendErr error
}

func newMemStore() *memStore {
	return &memStore{byID: map[string]*Event{}}
}

func (s *memStore) Append(_ context.Context, e *Event) error {
	if s.appendErr != nil {
		return s.appendErr
	}
	clone := *e
	s.appended = append(s.appended, &clone)
	s.byID[e.ID] = &clone
	return nil
}

func (s *memStore) Get(_ context.Context, _, eventID string) (*Event, error) {
	e, ok := s.byID[eventID]
	if !ok {
		return nil, ErrEventNotFound
	}
	return e, nil
}

func (s *memStore) List(context.Context, string, int, string) ([]*Event, string, error) {
	return s.appended, "", nil
}

func (s *memStore) ClaimUndispatched(_ context.Context, limit int) ([]*Event, error) {
	out := make([]*Event, 0, limit)
	for _, e := range s.appended {
		if e.DispatchedAt == nil && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *memStore) MarkDispatched(_ context.Context, ids []string) error {
	now := time.Now().UTC()
	for _, id := range ids {
		if e, ok := s.byID[id]; ok {
			e.DispatchedAt = &now
		}
	}
	return nil
}

func TestRecordAppendsAnEvent(t *testing.T) {
	store := newMemStore()
	at := time.Unix(1_700_000_000, 0).UTC()

	rec := NewRecorder(store, func() time.Time { return at })

	e, err := rec.Record(context.Background(), "mer_1", TypePaymentSucceeded, "pay_1", map[string]any{
		"amount": 10000,
	})
	if err != nil {
		t.Fatalf("Record = %v", err)
	}

	if !strings.HasPrefix(e.ID, "evt_") {
		t.Errorf("id = %q, want an evt_ prefix", e.ID)
	}
	if e.Type != TypePaymentSucceeded {
		t.Errorf("type = %q", e.Type)
	}
	if !e.CreatedAt.Equal(at) {
		t.Errorf("created_at = %v, want %v", e.CreatedAt, at)
	}
	if len(store.appended) != 1 {
		t.Fatalf("%d events appended, want 1", len(store.appended))
	}

	var payload map[string]any
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if payload["amount"] != float64(10000) {
		t.Errorf("payload = %v", payload)
	}
}

// An unknown type is refused before anything is written. A typo would otherwise create an
// event no endpoint can subscribe to, which presents as silence.
func TestRecordRejectsUnknownType(t *testing.T) {
	store := newMemStore()

	_, err := NewRecorder(store, nil).Record(context.Background(), "mer_1", "payment.exploded", "pay_1", nil)
	if !errors.Is(err, ErrInvalidType) {
		t.Fatalf("Record = %v, want ErrInvalidType", err)
	}
	if len(store.appended) != 0 {
		t.Error("an event with an unknown type was written")
	}
}

func TestRecordPropagatesStoreFailure(t *testing.T) {
	sentinel := errors.New("constraint violation")
	store := newMemStore()
	store.appendErr = sentinel

	_, err := NewRecorder(store, nil).Record(context.Background(), "mer_1", TypePaymentSucceeded, "pay_1", nil)
	if !errors.Is(err, sentinel) {
		t.Errorf("Record = %v, want the underlying error", err)
	}
}

func TestRecordRejectsUnmarshalablePayload(t *testing.T) {
	// A channel cannot be marshalled.
	_, err := NewRecorder(newMemStore(), nil).Record(
		context.Background(), "mer_1", TypePaymentSucceeded, "pay_1", make(chan int))
	if !errors.Is(err, ErrInvalidPayload) {
		t.Errorf("Record = %v, want ErrInvalidPayload", err)
	}
}

// The id is at the top level of the envelope because consumers are required to dedupe on
// it: delivery is at-least-once, and a consumer that does not dedupe will eventually
// process the same event twice.
func TestEnvelopeShape(t *testing.T) {
	e := &Event{
		ID:         "evt_1",
		Type:       TypePaymentSucceeded,
		ResourceID: "pay_1",
		Payload:    json.RawMessage(`{"amount":100}`),
		CreatedAt:  time.Unix(1_700_000_000, 0).UTC(),
	}

	raw, err := json.Marshal(e.Envelope())
	if err != nil {
		t.Fatalf("Marshal = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal = %v", err)
	}

	for _, key := range []string{"id", "object", "type", "created_at", "resource_id", "data"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("envelope has no %q field: %s", key, raw)
		}
	}
	if decoded["id"] != "evt_1" {
		t.Errorf("id = %v", decoded["id"])
	}
	if decoded["object"] != "event" {
		t.Errorf("object = %v", decoded["object"])
	}
}

func TestMatchesFilter(t *testing.T) {
	tests := []struct {
		pattern string
		typ     Type
		want    bool
	}{
		{"*", TypePaymentSucceeded, true},
		{"*", TypeTransferPosted, true},
		{"payment.*", TypePaymentSucceeded, true},
		{"payment.*", TypePaymentRefunded, true},
		{"payment.*", TypeTransferPosted, false},
		{"payment.succeeded", TypePaymentSucceeded, true},
		{"payment.succeeded", TypePaymentRefunded, false},
		{"transfer.*", TypeTransferReversed, true},
		// A prefix that is not followed by a dot must not match by accident.
		{"pay.*", TypePaymentSucceeded, false},
	}

	for _, tc := range tests {
		if got := MatchesFilter(tc.pattern, tc.typ); got != tc.want {
			t.Errorf("MatchesFilter(%q, %q) = %v, want %v", tc.pattern, tc.typ, got, tc.want)
		}
	}
}

func TestMatchesAny(t *testing.T) {
	patterns := []string{"transfer.*", "payment.refunded"}

	if !MatchesAny(patterns, TypePaymentRefunded) {
		t.Error("an exact pattern did not match")
	}
	if !MatchesAny(patterns, TypeTransferPosted) {
		t.Error("a glob did not match")
	}
	if MatchesAny(patterns, TypePaymentSucceeded) {
		t.Error("an unsubscribed type matched")
	}
	if MatchesAny(nil, TypePaymentSucceeded) {
		t.Error("an empty pattern list matched")
	}
}

// A filter typo presents as an endpoint that silently receives nothing, so it is caught
// at registration instead.
func TestValidateFilters(t *testing.T) {
	if err := ValidateFilters([]string{"*"}); err != nil {
		t.Errorf("ValidateFilters(*) = %v", err)
	}
	if err := ValidateFilters([]string{"payment.*", "transfer.posted"}); err != nil {
		t.Errorf("ValidateFilters = %v", err)
	}

	for _, bad := range [][]string{
		nil,
		{},
		{"paymnet.*"},           // typo in the resource
		{"payment.exploded"},    // no such type
		{"payment.*", "nope.*"}, // one good, one bad
	} {
		if err := ValidateFilters(bad); !errors.Is(err, ErrInvalidType) {
			t.Errorf("ValidateFilters(%v) = %v, want ErrInvalidType", bad, err)
		}
	}
}

func TestTypeValid(t *testing.T) {
	for _, typ := range AllTypes() {
		if !typ.Valid() {
			t.Errorf("%s.Valid() = false", typ)
		}
		// Every type must be resource.past_tense, so a glob on the resource works.
		if !strings.Contains(string(typ), ".") {
			t.Errorf("%s does not follow resource.action form", typ)
		}
	}
	if Type("nonsense").Valid() {
		t.Error("an unknown type reported Valid")
	}
	if TypePaymentSucceeded.String() != "payment.succeeded" {
		t.Errorf("String() = %q", TypePaymentSucceeded.String())
	}
}
