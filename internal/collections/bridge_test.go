package collections

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestBridgeSnapshotsRequireMatchingActorAndRequestedIDs(t *testing.T) {
	user := "11111111-1111-4111-8111-111111111111"
	id := "22222222-2222-4222-8222-222222222222"
	for _, body := range []string{
		`{"id":"` + id + `","user_id":"another-user","url":"https://example.com"}`,
		`{"id":"invalid","user_id":"` + user + `","url":"https://example.com"}`,
		`{"id":"` + id + `","user_id":"` + user + `","url":""}`,
		`{"id":"` + id + `","user_id":"` + user + `","url":"https://example.com","locations":{}}`,
	} {
		if _, err := ParseSnapshots([]json.RawMessage{json.RawMessage(body)}, user, []string{id}); !errors.Is(err, ErrBridgeInput) {
			t.Fatalf("body=%s err=%v", body, err)
		}
	}
	body := json.RawMessage(`{"id":"` + id + `","user_id":"` + user + `","url":"https://example.com","locations":null,"extra_source_field":true}`)
	if _, err := ParseSnapshots([]json.RawMessage{body, body}, user, []string{id}); !errors.Is(err, ErrBridgeInput) {
		t.Fatalf("duplicate snapshot accepted: %v", err)
	}
	snapshots, err := ParseSnapshots([]json.RawMessage{body}, user, []string{id})
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("valid legacy snapshot rejected: %v", err)
	}
}
