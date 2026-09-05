package reels

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidCursor is returned for anything that is not a cursor this service
// issued. Callers turn it into a validation error; they never guess at what the
// caller meant.
var ErrInvalidCursor = errors.New("invalid cursor")

// Cursor is the position of the last row a page returned. The list is ordered
// by saved time descending and then id descending, so those two values are
// exactly what the next page needs to resume, with no offset to drift when
// rows are added or removed between pages.
//
// It is opaque on purpose: it is base64 of a private JSON shape, the client
// never constructs one, and its fields can change without a contract change.
type Cursor struct {
	SavedAt time.Time `json:"s"`
	ID      string    `json:"i"`
}

// Encode renders a cursor for the wire. The encoding is URL-safe because a
// cursor travels as a query parameter.
func (c Cursor) Encode() string {
	encoded, err := json.Marshal(c)
	if err != nil {
		// The struct is two known fields; a failure here is not reachable.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

// DecodeCursor rejects anything it did not produce. A caller passing an offset,
// a row id, or a truncated string gets one error rather than a page from an
// unpredictable position.
func DecodeCursor(raw string) (Cursor, error) {
	if raw == "" {
		return Cursor{}, ErrInvalidCursor
	}

	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: not base64", ErrInvalidCursor)
	}

	var cursor Cursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return Cursor{}, fmt.Errorf("%w: not a cursor", ErrInvalidCursor)
	}
	if cursor.ID == "" || cursor.SavedAt.IsZero() {
		return Cursor{}, fmt.Errorf("%w: incomplete", ErrInvalidCursor)
	}
	return cursor, nil
}

// CursorFor builds the cursor that resumes after this record.
func CursorFor(record ReelRecord) (Cursor, bool) {
	if record.CreatedAt == nil {
		// Without a saved time there is no stable position to resume from.
		return Cursor{}, false
	}
	return Cursor{SavedAt: *record.CreatedAt, ID: record.ID}, true
}
