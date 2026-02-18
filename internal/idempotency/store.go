package idempotency

import (
	"context"
	"errors"
	"time"
)

// State is the lifecycle of an idempotency record.
type State string

const (
	// StateInProgress means a request holding this key is running, or was when its
	// holder last checked in.
	StateInProgress State = "in_progress"
	// StateSucceeded means the operation completed and the response is replayable.
	StateSucceeded State = "succeeded"
	// StateFailed means the operation completed with a client error, which is a real
	// outcome and is replayed like any other.
	StateFailed State = "failed"
)

// RecoveryPoint marks the last atomic phase an operation completed.
//
// Each phase commits its own work and advances this in the same database transaction.
// Foreign calls happen strictly between phases, never inside one.
//
// In the MVP the phases collapse into a single transaction, so the machinery looks like
// overkill — and today it is. It exists because the shape the moment a real processor is
// introduced is:
//
//	started        --[DB: reserve funds, pending]--> funds_reserved
//	funds_reserved --[HTTP: charge the processor]--> (no DB write; it is idempotent on our key)
//	               --[DB: capture, persist response]--> finished
//
// and a crash after the processor call but before the capture commits is then
// recoverable: the retry resumes at funds_reserved, re-issues the call with the same key,
// gets the cached response and finishes. Designing the state machine now costs nothing.
type RecoveryPoint string

const (
	PointStarted      RecoveryPoint = "started"
	PointFundsHeld    RecoveryPoint = "funds_reserved"
	PointLedgerPosted RecoveryPoint = "ledger_posted"
	PointFinished     RecoveryPoint = "finished"
)

// Sentinel errors.
var (
	// ErrKeyNotFound means no record exists for that key.
	ErrKeyNotFound = errors.New("idempotency: key not found")

	// ErrFingerprintMismatch means the key was reused with a different request. This is
	// a client bug and must never be treated as a retry: replaying the first response
	// would answer a question the client did not ask.
	ErrFingerprintMismatch = errors.New("idempotency: key reused with a different request")

	// ErrInProgress means another request holds the key and its lease has not expired.
	ErrInProgress = errors.New("idempotency: a request with this key is in progress")

	// ErrKeyRequired means a money-mutating route was called without a key.
	ErrKeyRequired = errors.New("idempotency: an Idempotency-Key header is required")

	// ErrInvalidKey means the supplied key is malformed.
	ErrInvalidKey = errors.New("idempotency: malformed idempotency key")
)

// Record is one stored idempotency key.
type Record struct {
	ID             string
	MerchantID     string
	Key            string
	Method         string
	Path           string
	RequestHash    []byte
	State          State
	RecoveryPoint  RecoveryPoint
	LockExpiresAt  *time.Time
	ResponseStatus int
	ResponseBody   []byte
	ResourceID     string
	RequestID      string
	CreatedAt      time.Time
	ExpiresAt      time.Time

	// completedInTx records that a handler already persisted the response inside its own
	// transaction, atomically with the ledger write. The middleware must then not write
	// it again afterwards — doing so outside that transaction would reintroduce exactly
	// the window this package exists to close.
	completedInTx bool
}

// MarkCompletedInTx notes that the response was persisted inside the caller's
// transaction.
func (r *Record) MarkCompletedInTx() { r.completedInTx = true }

// CompletedInTx reports whether the response was already persisted transactionally.
func (r *Record) CompletedInTx() bool { return r.completedInTx }

// IsTerminal reports whether the record holds a replayable outcome.
func (r *Record) IsTerminal() bool {
	return r.State == StateSucceeded || r.State == StateFailed
}

// LockExpired reports whether the holder's lease has passed, which means it crashed and
// the key may be taken over.
func (r *Record) LockExpired(now time.Time) bool {
	return r.LockExpiresAt == nil || !r.LockExpiresAt.After(now)
}

// Store persists idempotency records.
//
// Declared here and implemented by the postgres package, so the middleware can be tested
// without a database.
//
// Complete must be callable inside the caller's transaction. That is the whole point:
// the response is recorded in the same commit as the ledger entries, so there is no
// window in which money moved but the response was lost.
type Store interface {
	// Acquire attempts to claim the key. It returns the caller's own record and true if
	// it was claimed, or the existing record and false if someone else holds it.
	//
	// Implementations must do this as a single atomic insert-or-read. Checking then
	// inserting has a race in which two concurrent requests both believe they own the
	// key, which is exactly the double-charge this package prevents.
	Acquire(ctx context.Context, rec *Record) (*Record, bool, error)

	// Get returns the record for a key.
	Get(ctx context.Context, merchantID, key string) (*Record, error)

	// Complete records the terminal outcome and the response to replay.
	Complete(ctx context.Context, id string, state State, status int, body []byte, resourceID string) error

	// Advance moves the recovery point and extends the lease, for an operation with
	// more than one phase.
	Advance(ctx context.Context, id string, point RecoveryPoint, leaseUntil time.Time) error

	// TakeOver claims a record whose lease has expired, returning false if another
	// caller got there first.
	TakeOver(ctx context.Context, id string, leaseUntil time.Time) (bool, error)

	// Release removes the lock without recording a response, so a genuine retry can
	// re-execute. Used for 5xx outcomes.
	Release(ctx context.Context, id string) error

	// DeleteExpired removes records past their expiry, in batches. Used by the reaper.
	DeleteExpired(ctx context.Context, before time.Time, limit int) (int64, error)
}

// ValidateKey checks a client-supplied key.
//
// 1 to 255 printable ASCII characters. The bound exists because the key is stored,
// indexed and logged; the character restriction keeps a control character or a newline
// out of a log line, where it could forge a second entry.
func ValidateKey(key string) error {
	if key == "" {
		return ErrKeyRequired
	}
	if len(key) > 255 {
		return ErrInvalidKey
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x20 || key[i] > 0x7E {
			return ErrInvalidKey
		}
	}
	return nil
}
