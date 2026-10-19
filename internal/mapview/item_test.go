package mapview

import (
	"errors"
	"testing"
)

func TestParseItemID(t *testing.T) {
	const id = "22222222-2222-4222-8222-222222222222"

	manual, err := ParseItemID("manual:" + id)
	if err != nil || !manual.Manual || manual.ID != id {
		t.Fatalf("manual = %+v, %v", manual, err)
	}
	reel, err := ParseItemID("reel:" + id + ":3")
	if err != nil || reel.Manual || reel.ID != id || reel.Ordinal != 3 {
		t.Fatalf("reel = %+v, %v", reel, err)
	}

	for _, bad := range []string{
		"", "manual:", "manual:nope", "reel:" + id, "reel:" + id + ":x",
		"reel:" + id + ":-1", "reel:nope:0", "pin:" + id, id, "manual:" + id + ":0",
	} {
		if _, err := ParseItemID(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q: err = %v, want ErrNotFound", bad, err)
		}
	}
}
