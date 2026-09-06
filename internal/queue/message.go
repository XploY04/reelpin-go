package queue

import (
	"encoding/json"
	"fmt"
	"time"
)

// SchemaVersion is the shape of Message. Bump it when a field changes meaning,
// never when one is added; a consumer rejects anything newer than it
// understands rather than guessing.
const SchemaVersion = 1

// MaxMessageBytes bounds what a consumer will decode. An envelope is
// identifiers only, so anything near this size is not one of ours.
const MaxMessageBytes = 4096

// Message is everything a consumer is given: identifiers and nothing else. No
// URL, token, cookie, prompt or provider response ever rides the broker; the
// worker loads state from PostgreSQL by RunID. That rule is what makes broker
// loss recoverable and broker contents uninteresting to an attacker.
type Message struct {
	EventID       string `json:"event_id"`
	SchemaVersion int    `json:"schema_version"`
	EventType     string `json:"event_type"`
	RunID         string `json:"run_id"`
	// DispatchGeneration is the lease generation the dispatcher saw when it
	// published. A worker whose lease claim produces a lower generation is
	// holding a stale delivery.
	DispatchGeneration int64     `json:"dispatch_generation"`
	CreatedAt          time.Time `json:"created_at"`
	// TraceContext carries W3C traceparent/tracestate so a trace can follow
	// work across the broker. Opaque here; the metrics task consumes it.
	TraceContext map[string]string `json:"trace_context,omitempty"`
}

func (m Message) Validate() error {
	switch {
	case m.EventID == "":
		return fmt.Errorf("message has no event id")
	case m.RunID == "" && m.EventType != EventNotification:
		return fmt.Errorf("message has no run id")
	case m.EventType == "":
		return fmt.Errorf("message has no event type")
	case m.SchemaVersion == 0:
		return fmt.Errorf("message has no schema version")
	case m.SchemaVersion > SchemaVersion:
		return fmt.Errorf("message schema version %d is newer than this worker understands", m.SchemaVersion)
	}
	return nil
}

// Event types this topology routes. The type decides the queue class together
// with the routing key the outbox row carries.
const (
	EventProcessMedia = "run.process.media"
	EventProcessLight = "run.process.light"
	EventNotification = "notification.send"
)

func Decode(body []byte) (Message, error) {
	if len(body) > MaxMessageBytes {
		return Message{}, fmt.Errorf("message is %d bytes, cap is %d: not one of ours", len(body), MaxMessageBytes)
	}
	var message Message
	if err := json.Unmarshal(body, &message); err != nil {
		return Message{}, fmt.Errorf("decoding message: %w", err)
	}
	if err := message.Validate(); err != nil {
		return Message{}, err
	}
	return message, nil
}
