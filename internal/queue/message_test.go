package queue

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validMessage() Message {
	return Message{
		EventID:       "11111111-1111-4111-8111-111111111111",
		SchemaVersion: SchemaVersion,
		EventType:     EventProcessLight,
		RunID:         "22222222-2222-4222-8222-222222222222",
		CreatedAt:     time.Now().UTC(),
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	body, err := json.Marshal(validMessage())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.EventID != validMessage().EventID {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestDecodeRejectsWhatAConsumerCannotTrust(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Message)
	}{
		{"no event id", func(m *Message) { m.EventID = "" }},
		{"no run id", func(m *Message) { m.RunID = "" }},
		{"no event type", func(m *Message) { m.EventType = "" }},
		{"no schema version", func(m *Message) { m.SchemaVersion = 0 }},
		{"future schema version", func(m *Message) { m.SchemaVersion = SchemaVersion + 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message := validMessage()
			tt.mutate(&message)
			body, _ := json.Marshal(message)
			if _, err := Decode(body); err == nil {
				t.Fatal("decoded a message a consumer cannot act on safely")
			}
		})
	}
}

func TestANotificationNeedsNoRun(t *testing.T) {
	message := validMessage()
	message.EventType = EventNotification
	message.RunID = ""
	if err := message.Validate(); err != nil {
		t.Fatalf("a notification event was rejected: %v", err)
	}
}

func TestDecodeBoundsItsInput(t *testing.T) {
	if _, err := Decode([]byte(strings.Repeat("x", MaxMessageBytes+1))); err == nil {
		t.Fatal("an oversized body was decoded")
	}
}

func TestRetryRoutingKeysStayInTheirClass(t *testing.T) {
	for _, queue := range WorkQueues {
		for attempt := 0; attempt <= 5; attempt++ {
			key := RetryRoutingKey(queue, attempt)
			if !strings.HasPrefix(key, queue+".retry.") {
				t.Fatalf("attempt %d of %s routes to %q: a shared retry queue would return one class's messages to the other", attempt, queue, key)
			}
		}
	}
	if RetryRoutingKey(QueueLight, 1) == RetryRoutingKey(QueueLight, 2) {
		t.Error("the first and second attempts share a delay")
	}
}
