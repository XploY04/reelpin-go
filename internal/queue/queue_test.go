package queue

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRetryRoutingKeyClimbsTheLadder(t *testing.T) {
	tests := []struct {
		attempt int
		want    string
	}{
		{attempt: 0, want: "reelpin.retry.30s"},
		{attempt: 1, want: "reelpin.retry.30s"},
		{attempt: 2, want: "reelpin.retry.300s"},
		{attempt: 3, want: "reelpin.retry.1800s"},
		// Past the last stage the wait stays at the longest one; deciding to
		// give up belongs to the handler, not to the routing.
		{attempt: 9, want: "reelpin.retry.1800s"},
	}

	for _, tt := range tests {
		if got := RetryRoutingKey(tt.attempt); got != tt.want {
			t.Errorf("RetryRoutingKey(%d) = %s, want %s", tt.attempt, got, tt.want)
		}
	}
}

func TestEveryWorkQueueHasItsOwnDeadLetterQueue(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range WorkQueues {
		dead := DeadLetterQueue(name)
		if dead == name || seen[dead] {
			t.Fatalf("%s does not have a dead letter queue of its own", name)
		}
		seen[dead] = true
	}
}

func TestMessageValidation(t *testing.T) {
	valid := Message{
		EventID:       "11111111-1111-4111-8111-111111111111",
		RunID:         "22222222-2222-4222-8222-222222222222",
		Platform:      "instagram",
		SchemaVersion: SchemaVersion,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a complete message was rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Message)
	}{
		{name: "no event id", mutate: func(m *Message) { m.EventID = "" }},
		{name: "no run id", mutate: func(m *Message) { m.RunID = "" }},
		{name: "no schema version", mutate: func(m *Message) { m.SchemaVersion = 0 }},
		{name: "a newer schema than this worker understands", mutate: func(m *Message) { m.SchemaVersion = SchemaVersion + 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message := valid
			tt.mutate(&message)
			if err := message.Validate(); err == nil {
				t.Fatal("the message was accepted")
			}
		})
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := Decode([]byte("{not json")); err == nil {
		t.Fatal("garbage decoded successfully")
	}
	if _, err := Decode([]byte(`{"event_id":"x"}`)); err == nil {
		t.Fatal("an incomplete message decoded successfully")
	}
}

// A message carries identifiers only. Anything else would put user content on
// the broker, where it is neither needed nor wanted.
func TestMessageCarriesNoContent(t *testing.T) {
	message := Message{
		EventID:       "11111111-1111-4111-8111-111111111111",
		RunID:         "22222222-2222-4222-8222-222222222222",
		Platform:      "instagram",
		SchemaVersion: SchemaVersion,
		Type:          "content.process",
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"url", "token", "user_id", "transcript", "cookie"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("the message envelope carries a %q field", forbidden)
		}
	}
}
