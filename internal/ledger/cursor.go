package ledger

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cursor is a position in a keyset-paginated list: the (created_at, id) pair of the
// last row returned.
//
// Both halves are needed. The timestamp alone is not unique — a ledger can write many
// entries in the same microsecond — and the id alone would not survive a future switch
// to a time-ordered partition scan. Together they are a total order.
//
// OFFSET is never used. It degrades linearly with depth, and worse, it silently skips
// or duplicates rows when concurrent inserts land between pages, which is exactly the
// behavior a ledger view must not have.
type Cursor struct {
	CreatedAt time.Time
	ID        int64
}

// String encodes the cursor as an opaque token. It is base64 so that callers treat it
// as opaque rather than parsing it and coming to depend on the format.
func (c Cursor) String() string {
	raw := strconv.FormatInt(c.CreatedAt.UTC().UnixNano(), 10) + ":" + strconv.FormatInt(c.ID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseCursor decodes a token produced by Cursor.String.
func ParseCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, fmt.Errorf("%w: empty cursor", ErrInvalidCursor)
	}

	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: %q is not a valid token", ErrInvalidCursor, s)
	}

	nanos, idPart, ok := strings.Cut(string(raw), ":")
	if !ok {
		return Cursor{}, fmt.Errorf("%w: %q has no separator", ErrInvalidCursor, s)
	}

	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: %q has a bad timestamp", ErrInvalidCursor, s)
	}
	entryID, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: %q has a bad id", ErrInvalidCursor, s)
	}

	return Cursor{CreatedAt: time.Unix(0, n).UTC(), ID: entryID}, nil
}

// CursorFor returns the cursor pointing at e.
func CursorFor(e Entry) Cursor {
	return Cursor{CreatedAt: e.CreatedAt, ID: e.ID}
}
