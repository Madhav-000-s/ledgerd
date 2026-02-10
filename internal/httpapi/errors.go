// Package httpapi is the HTTP surface: routing, authentication, the error envelope and
// keyset pagination.
//
// Handlers translate between JSON and the domain and do nothing else. They never open a
// database transaction, never touch entries, and never decide what an error means — the
// last of those is the job of the single mapping table in this file.
package httpapi

import (
	"errors"
	"net/http"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/money"
)

// ErrorType is the coarse class of a failure, using the vocabulary a payments API
// consumer already expects.
type ErrorType string

const (
	TypeInvalidRequest ErrorType = "invalid_request_error"
	TypeIdempotency    ErrorType = "idempotency_error"
	TypeLedger         ErrorType = "ledger_error"
	TypeRateLimit      ErrorType = "rate_limit_error"
	TypeAuthentication ErrorType = "authentication_error"
	TypeAPI            ErrorType = "api_error"
)

// docBase is prefixed to an error code to produce the doc_url a consumer can follow.
const docBase = "https://github.com/Madhav-000-s/ledgerd/blob/main/docs/errors.md#"

// ErrorBody is the payload of an error response.
type ErrorBody struct {
	Type      ErrorType `json:"type"`
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	Param     string    `json:"param,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
	DocURL    string    `json:"doc_url,omitempty"`
}

// ErrorResponse is the envelope. An error is always nested under a single key, so a
// consumer can tell a failure from a successful response that happens to have an
// "error" field of its own.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// APIError carries everything the envelope needs. Handlers return one when they want to
// say more than a domain sentinel does — usually which parameter was at fault.
type APIError struct {
	Status  int
	Type    ErrorType
	Code    string
	Message string
	Param   string

	// Err is the underlying cause. It is logged, never serialized: an internal error
	// message is exactly the sort of thing that leaks a schema, a file path or a host
	// name to a caller.
	Err error
}

func (e *APIError) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Message + ": " + e.Err.Error()
	}
	return e.Code + ": " + e.Message
}

func (e *APIError) Unwrap() error { return e.Err }

// Errorf builds an APIError.
func Errorf(status int, typ ErrorType, code, message string) *APIError {
	return &APIError{Status: status, Type: typ, Code: code, Message: message}
}

// WithParam names the request field responsible, so a client can highlight it.
func (e *APIError) WithParam(param string) *APIError {
	clone := *e
	clone.Param = param
	return &clone
}

// WithCause attaches the underlying error for logging. It copies rather than mutates, so
// that the package-level sentinels below cannot be modified by a handler.
func (e *APIError) WithCause(err error) *APIError {
	clone := *e
	clone.Err = err
	return &clone
}

// WithMessage replaces the message, for cases where the generic wording is unhelpful.
func (e *APIError) WithMessage(message string) *APIError {
	clone := *e
	clone.Message = message
	return &clone
}

// mapping is one row of the sentinel-to-response table.
type mapping struct {
	sentinel error
	status   int
	typ      ErrorType
	code     string
	param    string
}

// errorTable maps every domain sentinel onto its HTTP representation.
//
// This is the point of the file: the mapping is data in one place rather than a
// scattering of errors.Is branches through the handlers. Adding a domain error means
// adding a row here, and forgetting to means a generic 500 — which is loud — rather than
// a plausible-looking status code invented at a call site.
//
// The first match wins, so more specific sentinels come first.
var errorTable = []mapping{
	// Ledger invariants a caller can fix by sending something different.
	{ledger.ErrUnbalanced, http.StatusBadRequest, TypeInvalidRequest, "transaction_unbalanced", "entries"},
	{ledger.ErrMultiCurrency, http.StatusBadRequest, TypeInvalidRequest, "multiple_currencies", "entries"},
	{ledger.ErrNoEntries, http.StatusBadRequest, TypeInvalidRequest, "no_entries", "entries"},
	{ledger.ErrInvalidDirection, http.StatusBadRequest, TypeInvalidRequest, "invalid_direction", "direction"},
	{ledger.ErrAccountCurrency, http.StatusBadRequest, TypeInvalidRequest, "account_currency_mismatch", "currency"},
	{ledger.ErrInvalidAccountType, http.StatusBadRequest, TypeInvalidRequest, "invalid_account_type", "type"},
	{ledger.ErrInvalidCursor, http.StatusBadRequest, TypeInvalidRequest, "invalid_cursor", "starting_after"},

	// Not-found before the broader ErrInvalidAccount, which would otherwise swallow it.
	{ledger.ErrAccountNotFound, http.StatusNotFound, TypeInvalidRequest, "account_not_found", ""},
	{ledger.ErrTransactionNotFound, http.StatusNotFound, TypeInvalidRequest, "transaction_not_found", ""},
	{ledger.ErrAccountExists, http.StatusConflict, TypeInvalidRequest, "account_exists", "name"},
	{ledger.ErrInvalidAccount, http.StatusBadRequest, TypeInvalidRequest, "invalid_account", ""},

	// Insufficient funds is a ledger_error, not an invalid_request_error: the request
	// was well formed, the books simply do not permit it.
	{ledger.ErrInsufficientFunds, http.StatusUnprocessableEntity, TypeLedger, "insufficient_funds", "amount"},
	{ledger.ErrCaptureExceedsAuthorization, http.StatusUnprocessableEntity, TypeLedger, "capture_exceeds_authorization", "amount"},
	{ledger.ErrAlreadyReversed, http.StatusConflict, TypeLedger, "already_reversed", ""},
	{ledger.ErrNotPending, http.StatusConflict, TypeLedger, "transaction_not_pending", ""},
	{ledger.ErrNotPosted, http.StatusConflict, TypeLedger, "transaction_not_posted", ""},
	{ledger.ErrInvalidAmount, http.StatusBadRequest, TypeInvalidRequest, "invalid_amount", "amount"},

	// Money. An overflow is a caller sending an absurd number, not a server fault, so it
	// is a 400 rather than a 500.
	{money.ErrUnknownCurrency, http.StatusBadRequest, TypeInvalidRequest, "unknown_currency", "currency"},
	{money.ErrCurrencyMismatch, http.StatusBadRequest, TypeInvalidRequest, "currency_mismatch", "currency"},
	{money.ErrInvalidRatios, http.StatusBadRequest, TypeInvalidRequest, "invalid_split", ""},
	{money.ErrOverflow, http.StatusBadRequest, TypeInvalidRequest, "amount_overflow", "amount"},
	{money.ErrInvalidAmount, http.StatusBadRequest, TypeInvalidRequest, "invalid_amount", "amount"},
}

// toAPIError resolves any error into the response to send.
//
// An error with no row in the table becomes a generic 500 whose message says nothing
// about the cause. The cause travels on the returned value so the logger can record it;
// only the request id crosses back to the caller.
func toAPIError(err error) *APIError {
	if err == nil {
		return nil
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}

	for _, m := range errorTable {
		if errors.Is(err, m.sentinel) {
			return &APIError{
				Status:  m.status,
				Type:    m.typ,
				Code:    m.code,
				Message: err.Error(),
				Param:   m.param,
				Err:     err,
			}
		}
	}

	return &APIError{
		Status:  http.StatusInternalServerError,
		Type:    TypeAPI,
		Code:    "internal_error",
		Message: "An unexpected error occurred. The request id identifies this failure in our logs.",
		Err:     err,
	}
}

// Errors raised by the transport itself, where there is no domain sentinel to map from.
var (
	errUnauthorized = Errorf(http.StatusUnauthorized, TypeAuthentication, "unauthorized",
		"No valid API key was provided.")
	errForbidden = Errorf(http.StatusForbidden, TypeAuthentication, "insufficient_permissions",
		"This API key does not have permission to perform that action.")
	errMalformedJSON = Errorf(http.StatusBadRequest, TypeInvalidRequest, "malformed_json",
		"The request body could not be parsed as JSON.")
	errBodyTooLarge = Errorf(http.StatusRequestEntityTooLarge, TypeInvalidRequest, "request_too_large",
		"The request body exceeded the maximum size.")
	errRateLimited = Errorf(http.StatusTooManyRequests, TypeRateLimit, "rate_limit_exceeded",
		"Too many requests. Retry after the interval given in the Retry-After header.")
	errNotFound = Errorf(http.StatusNotFound, TypeInvalidRequest, "not_found",
		"No such endpoint.")
	errMethodNotAllowed = Errorf(http.StatusMethodNotAllowed, TypeInvalidRequest, "method_not_allowed",
		"That method is not supported on this endpoint.")
)
