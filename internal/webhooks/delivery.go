package webhooks

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/events"
)

// DeliveryState is where a delivery is in its lifecycle.
type DeliveryState string

const (
	StatePending    DeliveryState = "pending"
	StateDelivering DeliveryState = "delivering"
	StateSucceeded  DeliveryState = "succeeded"
	// StateExhausted is the dead-letter queue. It is a state rather than a separate
	// table, so a replay is a state change instead of a migration between stores.
	StateExhausted DeliveryState = "exhausted"
	StateDisabled  DeliveryState = "disabled"
)

// EndpointStatus is an endpoint's health.
type EndpointStatus string

const (
	EndpointActive   EndpointStatus = "active"
	EndpointDegraded EndpointStatus = "degraded"
	EndpointDisabled EndpointStatus = "disabled"
)

// Thresholds for endpoint health, from DESIGN.md §8.6.
const (
	// DegradeAfter consecutive exhausted deliveries marks an endpoint degraded and
	// raises an alert.
	DegradeAfter = 20
	// DisableAfter consecutive exhausted deliveries disables it, requiring a manual
	// re-enable. Retrying forever at an endpoint that has been gone for a week is
	// spending real resources on an outcome that is not going to change.
	DisableAfter = 100
)

// MaxAttempts is the default retry budget.
const MaxAttempts = 16

// Endpoint is a merchant's webhook destination.
type Endpoint struct {
	ID                  string
	MerchantID          string
	URL                 string
	EnabledEvents       []string
	Status              EndpointStatus
	ConsecutiveFailures int
	Description         string
	CreatedAt           time.Time
	DisabledAt          *time.Time
}

// Wants reports whether this endpoint is subscribed to an event type and is able to
// receive it.
func (e *Endpoint) Wants(typ events.Type) bool {
	if e.Status == EndpointDisabled {
		return false
	}
	return events.MatchesAny(e.EnabledEvents, typ)
}

// Secret is one signing secret for an endpoint.
type Secret struct {
	ID         string
	EndpointID string
	// Plaintext is only populated after decryption, in the worker that signs with it.
	Plaintext string
	Active    bool
	CreatedAt time.Time
	RetiredAt *time.Time
}

// Delivery is one event queued for one endpoint.
type Delivery struct {
	ID             string
	EventID        string
	EndpointID     string
	MerchantID     string
	State          DeliveryState
	Attempt        int
	NextAttemptAt  time.Time
	LeasedUntil    *time.Time
	LastStatus     *int
	LastError      string
	LastDurationMS *int
	CreatedAt      time.Time
	CompletedAt    *time.Time
}

// Attempt is one recorded try, kept as an append-only audit.
type Attempt struct {
	ID          int64
	DeliveryID  string
	Attempt     int
	StatusCode  *int
	Error       string
	DurationMS  *int
	AttemptedAt time.Time
}

// Outcome is what to do with a delivery after an attempt.
type Outcome int

const (
	// OutcomeSucceeded means the endpoint accepted it.
	OutcomeSucceeded Outcome = iota
	// OutcomeRetry means try again after the backoff.
	OutcomeRetry
	// OutcomeExhausted means the retry budget is spent; the delivery goes to the DLQ.
	OutcomeExhausted
	// OutcomeDisabled means the endpoint asked us to stop.
	OutcomeDisabled
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSucceeded:
		return "succeeded"
	case OutcomeRetry:
		return "retry"
	case OutcomeExhausted:
		return "exhausted"
	case OutcomeDisabled:
		return "disabled"
	default:
		return "unknown"
	}
}

// Classify decides what an attempt's result means, per DESIGN.md §8.4.
//
// The interesting choices are the ones that are not obvious:
//
//   - 410 Gone disables the endpoint immediately. It is the one status whose meaning is
//     "stop sending", and continuing to retry for 35 hours after being told that is
//     rude and pointless.
//   - Other 4xx are retried. A 404 during a deploy is common, and treating it as
//     permanent would drop events for an endpoint that came back thirty seconds later.
//     This costs some wasted attempts against a genuinely wrong URL, which is the
//     cheaper mistake.
//   - Everything transport-level — timeout, DNS, TLS, refused — is retried.
func Classify(resp Response, attempt, maxAttempts int) Outcome {
	if maxAttempts <= 0 {
		maxAttempts = MaxAttempts
	}

	if resp.Succeeded() {
		return OutcomeSucceeded
	}

	// A redirect is not a transport failure but an endpoint doing something we refuse to
	// follow. Retrying is right: the merchant may fix their configuration.
	if resp.Err == nil && resp.StatusCode == http.StatusGone {
		return OutcomeDisabled
	}

	if attempt+1 >= maxAttempts {
		return OutcomeExhausted
	}
	return OutcomeRetry
}

// RetryAfter reads a Retry-After hint from the endpoint, honouring it up to the cap.
//
// An endpoint that says "not for another 90 seconds" is telling us something useful
// about its own load, and ignoring it in favour of our own schedule is how a service
// that is already struggling gets pushed over.
func RetryAfter(header string, now time.Time, capacity time.Duration) (time.Duration, bool) {
	if header == "" {
		return 0, false
	}

	if seconds, err := strconv.Atoi(header); err == nil {
		if seconds < 0 {
			return 0, false
		}
		d := time.Duration(seconds) * time.Second
		if capacity > 0 && d > capacity {
			d = capacity
		}
		return d, true
	}

	if at, err := http.ParseTime(header); err == nil {
		d := at.Sub(now)
		if d < 0 {
			return 0, true // due immediately
		}
		if capacity > 0 && d > capacity {
			d = capacity
		}
		return d, true
	}
	return 0, false
}

// Store persists endpoints, secrets and the delivery queue.
//
// Declared here and implemented by the postgres package, so the dispatcher and worker
// can be tested without a database.
type Store interface {
	// ── endpoints ──
	CreateEndpoint(ctx context.Context, e *Endpoint) error
	GetEndpoint(ctx context.Context, merchantID, endpointID string) (*Endpoint, error)
	ListEndpoints(ctx context.Context, merchantID string) ([]*Endpoint, error)
	// ActiveEndpointsForEvent returns the endpoints a given event should fan out to.
	ActiveEndpointsForEvent(ctx context.Context, merchantID string) ([]*Endpoint, error)
	SetEndpointStatus(ctx context.Context, endpointID string, status EndpointStatus) error
	// RecordEndpointFailure increments the consecutive-failure counter and returns the
	// new value, so the caller can act on the degrade and disable thresholds.
	RecordEndpointFailure(ctx context.Context, endpointID string) (int, error)
	ResetEndpointFailures(ctx context.Context, endpointID string) error

	// ── secrets ──
	CreateSecret(ctx context.Context, s *Secret, hash, encrypted []byte) error
	ActiveSecrets(ctx context.Context, endpointID string) ([]*Secret, error)
	RetireSecret(ctx context.Context, secretID string) error

	// ── the queue ──
	// EnqueueDeliveries inserts fan-out rows. It must be idempotent on
	// (event_id, endpoint_id) so a re-run of a partially completed dispatch is harmless.
	EnqueueDeliveries(ctx context.Context, deliveries []*Delivery) error
	// ClaimDue takes a batch of due deliveries with FOR UPDATE SKIP LOCKED, so competing
	// workers need no coordination at all.
	ClaimDue(ctx context.Context, limit int, leaseUntil time.Time) ([]*Delivery, error)
	// CompleteDelivery records a terminal outcome.
	CompleteDelivery(ctx context.Context, deliveryID string, state DeliveryState, resp Response) error
	// RescheduleDelivery puts a delivery back in the queue after a failed attempt.
	//
	// The backoff is passed as a delay rather than an absolute time so the schedule stays
	// on the database's clock, which is the one the claim compares against.
	RescheduleDelivery(ctx context.Context, deliveryID string, delay time.Duration, resp Response) error
	// RecordAttempt appends to the audit of tries.
	RecordAttempt(ctx context.Context, a *Attempt) error
	// ReclaimExpiredLeases resets rows whose worker died mid-POST. This is the
	// crash-recovery path, and it is why delivery is at-least-once rather than
	// at-most-once.
	ReclaimExpiredLeases(ctx context.Context, now time.Time) (int64, error)

	// ListDeliveries pages an endpoint's deliveries, optionally filtered by state, which
	// is how the DLQ is read.
	ListDeliveries(ctx context.Context, endpointID string, state DeliveryState, limit int) ([]*Delivery, error)
	// ReplayDelivery resets an exhausted delivery to pending.
	ReplayDelivery(ctx context.Context, merchantID, deliveryID string) error
	// GetEvent loads the event a delivery carries.
	GetEvent(ctx context.Context, eventID string) (*events.Event, error)
}

// Sentinel errors.
var (
	ErrEndpointNotFound = errors.New("webhooks: endpoint not found")
	ErrDeliveryNotFound = errors.New("webhooks: delivery not found")
	ErrSecretNotFound   = errors.New("webhooks: no active signing secret")
	ErrNotReplayable    = errors.New("webhooks: only an exhausted delivery can be replayed")
	ErrEndpointDisabled = errors.New("webhooks: endpoint is disabled")
)
