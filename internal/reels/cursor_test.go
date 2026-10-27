package reels

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestACursorSurvivesTheRoundTrip(t *testing.T) {
	saved := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)
	cursor := Cursor{SavedAt: saved, ID: "11111111-1111-4111-8111-111111111111"}

	decoded, err := DecodeCursor(cursor.Encode())
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if !decoded.SavedAt.Equal(saved) || decoded.ID != cursor.ID {
		t.Fatalf("decoded = %+v, want %+v", decoded, cursor)
	}
}

func TestACursorIsOpaqueAndURLSafe(t *testing.T) {
	cursor := Cursor{SavedAt: time.Now().UTC(), ID: "22222222-2222-4222-8222-222222222222"}
	encoded := cursor.Encode()

	// A client that can read the id out of it will start depending on it.
	if strings.Contains(encoded, cursor.ID) {
		t.Error("the id is readable in the cursor")
	}
	for _, unsafe := range []string{"+", "/", "=", "?", "&", " "} {
		if strings.Contains(encoded, unsafe) {
			t.Errorf("cursor contains %q, which needs escaping in a query string", unsafe)
		}
	}
}

func TestOnlyOurOwnCursorsAreAccepted(t *testing.T) {
	// The shapes a client might guess at, and one truncated real cursor.
	real := Cursor{SavedAt: time.Now().UTC(), ID: "33333333-3333-4333-8333-333333333333"}.Encode()

	for _, raw := range []string{
		"", "0", "25", "not base64!", "eyJ9", real[:len(real)/2],
		"33333333-3333-4333-8333-333333333333",
	} {
		if _, err := DecodeCursor(raw); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("DecodeCursor(%q) err = %v, want ErrInvalidCursor", raw, err)
		}
	}
}

func TestACursorNeedsBothHalves(t *testing.T) {
	// Either half alone cannot resume a keyset scan.
	for _, cursor := range []Cursor{
		{SavedAt: time.Now().UTC()},
		{ID: "44444444-4444-4444-8444-444444444444"},
	} {
		if _, err := DecodeCursor(cursor.Encode()); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("a half cursor %+v was accepted", cursor)
		}
	}
}

func TestCursorForNeedsASavedTime(t *testing.T) {
	saved := time.Now().UTC()
	if _, ok := CursorFor(ReelRecord{ID: "x", CreatedAt: &saved}); !ok {
		t.Error("a record with a saved time produced no cursor")
	}
	if _, ok := CursorFor(ReelRecord{ID: "x"}); ok {
		t.Error("a record with no saved time produced a cursor to resume from")
	}
}
