// Package events is the transactional outbox.
//
// An event is written in the same database transaction as the ledger entries that
// produced it. That single fact is the whole design: it is impossible to move money
// without producing an event, and impossible to produce an event for money that did not
// move.
//
// Any alternative that publishes to a broker from application code after committing has
// a window in which those two facts disagree. In most systems that window costs a missed
// notification; in payments it costs a reconciliation incident, because the books and
// everything downstream of them now tell different stories about the same money.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/platform/id"
)

// Type is an event type, in resource.past_tense form.
type Type string

const (
	TypePaymentSucceeded         Type = "payment.succeeded"
	TypePaymentRequiresCapture   Type = "payment.requires_capture"
	TypePaymentCaptured          Type = "payment.captured"
	TypePaymentCanceled          Type = "payment.canceled"
	TypePaymentRefunded          Type = "payment.refunded"
	TypePaymentPartiallyRefunded Type = "payment.partially_refunded"

	TypeTransferPosted   Type = "transfer.posted"
	TypeTransferReversed Type = "transfer.reversed"

	TypeAccountCreated Type = "account.created"

	// TypeEndpointDegraded is emitted to a merchant's *other* endpoints when one of them
	// starts failing, because telling an endpoint it is broken by delivering to that
	// endpoint is not a plan.
	TypeEndpointDegraded Type = "endpoint.degraded"
	TypeEndpointDisabled Type = "endpoint.disabled"
)

// AllTypes is every type the system emits. The API advertises it, and endpoint
// validation checks subscriptions against it so a typo in a filter is caught at
// registration rather than discovered as silence.
func AllTypes() []Type {
	return []Type{
		TypePaymentSucceeded, TypePaymentRequiresCapture, TypePaymentCaptured,
		TypePaymentCanceled, TypePaymentRefunded, TypePaymentPartiallyRefunded,
		TypeTransferPosted, TypeTransferReversed,
		TypeAccountCreated,
		TypeEndpointDegraded, TypeEndpointDisabled,
	}
}

// Valid reports whether t is a known event type.
func (t Type) Valid() bool {
	for _, known := range AllTypes() {
		if t == known {
			return true
		}
	}
	return false
}

func (t Type) String() string { return string(t) }

// Event is one thing that happened.
type Event struct {
	ID           string
	MerchantID   string
	Type         Type
	ResourceID   string
	Payload      json.RawMessage
	DispatchedAt *time.Time
	CreatedAt    time.Time
}

// Envelope is the JSON body delivered to an endpoint.
//
// The id is at the top level because consumers are required to dedupe on it: delivery is
// at-least-once, and a consumer that does not dedupe will eventually process the same
// event twice.
type Envelope struct {
	ID         string          `json:"id"`
	Object     string          `json:"object"`
	Type       Type            `json:"type"`
	CreatedAt  string          `json:"created_at"`
	ResourceID string          `json:"resource_id"`
	Data       json.RawMessage `json:"data"`
}

// Sentinel errors.
var (
	ErrInvalidType    = errors.New("events: unknown event type")
	ErrEventNotFound  = errors.New("events: event not found")
	ErrInvalidPayload = errors.New("events: payload is not valid JSON")
)

// Envelope renders the event as it will be delivered.
func (e *Event) Envelope() Envelope {
	return Envelope{
		ID:         e.ID,
		Object:     "event",
		Type:       e.Type,
		CreatedAt:  e.CreatedAt.UTC().Format(time.RFC3339Nano),
		ResourceID: e.ResourceID,
		Data:       e.Payload,
	}
}

// Store persists events. Declared here and implemented by the postgres package.
//
// Append must be callable inside the caller's transaction. That is the entire point of
// the outbox: the event and the entries commit together or neither does.
type Store interface {
	// Append writes an event. It must run in the caller's transaction.
	Append(ctx context.Context, e *Event) error

	// Get returns one event.
	Get(ctx context.Context, merchantID, eventID string) (*Event, error)

	// List returns a merchant's events, newest first, with the next cursor.
	List(ctx context.Context, merchantID string, limit int, before string) ([]*Event, string, error)

	// ClaimUndispatched takes a batch of events that have not been fanned out, locking
	// them so that competing dispatchers do not expand the same event twice.
	ClaimUndispatched(ctx context.Context, limit int) ([]*Event, error)

	// MarkDispatched records that an event has been expanded into deliveries.
	MarkDispatched(ctx context.Context, eventIDs []string) error
}

// Recorder writes events into the caller's transaction.
type Recorder struct {
	store Store
	now   func() time.Time
}

// NewRecorder returns a Recorder over store.
func NewRecorder(store Store, now func() time.Time) *Recorder {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Recorder{store: store, now: now}
}

// Record appends an event.
//
// It must be called inside the same transaction as the state change it describes. There
// is no variant that opens its own transaction, because having one would make it
// possible to get this wrong by accident.
func (r *Recorder) Record(ctx context.Context, merchantID string, typ Type, resourceID string, data any) (*Event, error) {
	if !typ.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidType, string(typ))
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidPayload, err)
	}

	e := &Event{
		ID:         id.New(id.PrefixEvent),
		MerchantID: merchantID,
		Type:       typ,
		ResourceID: resourceID,
		Payload:    payload,
		CreatedAt:  r.now(),
	}

	if err := r.store.Append(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

// MatchesFilter reports whether an event type is covered by a subscription pattern.
//
// A pattern is either an exact type, a prefix glob such as "payment.*", or "*" for
// everything. Globs are deliberately limited to a trailing star: richer matching would
// invite patterns whose behavior a merchant cannot predict, and silence is the failure
// mode of a webhook filter that does not do what its author expected.
func MatchesFilter(pattern string, typ Type) bool {
	switch {
	case pattern == "*":
		return true
	case strings.HasSuffix(pattern, ".*"):
		return strings.HasPrefix(string(typ), strings.TrimSuffix(pattern, "*"))
	default:
		return pattern == string(typ)
	}
}

// MatchesAny reports whether any of the patterns covers typ.
func MatchesAny(patterns []string, typ Type) bool {
	for _, p := range patterns {
		if MatchesFilter(p, typ) {
			return true
		}
	}
	return false
}

// ValidateFilters checks that every pattern could match something, so a typo is caught
// at registration rather than presenting as an endpoint that silently receives nothing.
func ValidateFilters(patterns []string) error {
	if len(patterns) == 0 {
		return fmt.Errorf("%w: an endpoint must subscribe to at least one type", ErrInvalidType)
	}

	for _, p := range patterns {
		if p == "*" {
			continue
		}
		matched := false
		for _, known := range AllTypes() {
			if MatchesFilter(p, known) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%w: %q matches no known event type", ErrInvalidType, p)
		}
	}
	return nil
}
