package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/platform/log"
)

// writeJSON sends a value as JSON with the given status.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Marshalling a response we constructed ourselves failing means a bug in a
		// response type, not a client problem.
		log.From(r.Context()).Error("marshalling response", log.Err(err))
		writeError(w, r, Errorf(http.StatusInternalServerError, TypeAPI, "internal_error",
			"The response could not be serialized."))
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		// The client hung up mid-write. Nothing to do but note it; the work already
		// committed, and a retry with the same idempotency key will replay the response.
		log.From(r.Context()).Debug("writing response body", log.Err(err))
	}
}

// writeError renders an error in the envelope.
//
// The cause is logged with the request id and never serialized. A caller gets the code,
// a safe message and the request id, which is enough to report the failure precisely
// without learning anything about the inside of the service.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	apiErr := toAPIError(err)
	requestID := RequestIDFrom(r.Context())

	logger := log.From(r.Context())
	attrs := []any{
		slog.Int("status", apiErr.Status),
		slog.String("code", apiErr.Code),
	}
	if apiErr.Err != nil {
		attrs = append(attrs, log.Err(apiErr.Err))
	}

	if apiErr.Status >= http.StatusInternalServerError {
		logger.Error("request failed", attrs...)
	} else {
		logger.Info("request rejected", attrs...)
	}

	if apiErr.Status == http.StatusTooManyRequests && w.Header().Get("Retry-After") == "" {
		w.Header().Set("Retry-After", "1")
	}

	writeJSON(w, r, apiErr.Status, ErrorResponse{Error: ErrorBody{
		Type:      apiErr.Type,
		Code:      apiErr.Code,
		Message:   apiErr.Message,
		Param:     apiErr.Param,
		RequestID: requestID,
		DocURL:    docBase + apiErr.Code,
	}})
}

// decodeJSON reads and validates a request body.
//
// Unknown fields are rejected rather than ignored. A caller that misspells "amount" and
// gets a 201 with a zero amount has been failed by the API, and in a payments context
// that failure moves money.
func decodeJSON(r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		media := strings.TrimSpace(strings.Split(ct, ";")[0])
		if !strings.EqualFold(media, "application/json") {
			return Errorf(http.StatusUnsupportedMediaType, TypeInvalidRequest, "unsupported_media_type",
				"Content-Type must be application/json.")
		}
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return errBodyTooLarge.WithCause(err)
		}

		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return errMalformedJSON.
				WithMessage("Field " + typeErr.Field + " has the wrong type.").
				WithParam(typeErr.Field).
				WithCause(err)
		}
		if strings.Contains(err.Error(), "unknown field") {
			return errMalformedJSON.
				WithMessage(strings.TrimPrefix(err.Error(), "json: ")).
				WithCause(err)
		}
		return errMalformedJSON.WithCause(err)
	}

	// Exactly one JSON value, so a body with trailing garbage is rejected rather than
	// silently half-read.
	if dec.More() {
		return errMalformedJSON.
			WithMessage("The request body contained more than one JSON value.")
	}
	return nil
}

// pageFrom builds a pagination request from the query string.
func pageFrom(r *http.Request) (ledger.Page, error) {
	p := ledger.Page{StartingAfter: r.URL.Query().Get("starting_after")}

	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return p, Errorf(http.StatusBadRequest, TypeInvalidRequest, "invalid_limit",
				"limit must be an integer.").WithParam("limit")
		}
		if n < 1 || n > ledger.MaxPageLimit {
			return p, Errorf(http.StatusBadRequest, TypeInvalidRequest, "invalid_limit",
				"limit must be between 1 and "+strconv.Itoa(ledger.MaxPageLimit)+".").WithParam("limit")
		}
		p.Limit = n
	}
	return p.Normalize(), nil
}

// listResponse is the shape of every collection endpoint.
//
// has_more is explicit rather than implied by a non-empty cursor, so a client does not
// have to know that "" means "done".
type listResponse[T any] struct {
	Object     string `json:"object"`
	Data       []T    `json:"data"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func newListResponse[T any](data []T, nextCursor string) listResponse[T] {
	if data == nil {
		data = []T{}
	}
	return listResponse[T]{
		Object:     "list",
		Data:       data,
		HasMore:    nextCursor != "",
		NextCursor: nextCursor,
	}
}
