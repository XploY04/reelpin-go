package queue

import (
	"encoding/json"
	"fmt"
)

// SchemaVersion is the shape of Message. Bump it when a field changes meaning,
// never when one is added.
const SchemaVersion = 1

// Message is everything a consumer is given. It carries identifiers only: no
// URLs, no tokens, no user text. The worker loads what it needs from the
// database using RunID.
type Message struct {
	EventID       string `json:"event_id"`
	RunID         string `json:"run_id"`
	Platform      string `json:"platform"`
	Attempt       int    `json:"attempt"`
	SchemaVersion int    `json:"schema_version"`
	Type          string `json:"type"`
}

func (m Message) Validate() error {
	switch {
	case m.EventID == "":
		return fmt.Errorf("message has no event id")
	case m.RunID == "":
		return fmt.Errorf("message has no run id")
	case m.SchemaVersion == 0:
		return fmt.Errorf("message has no schema version")
	case m.SchemaVersion > SchemaVersion:
		return fmt.Errorf("message schema version %d is newer than this worker understands", m.SchemaVersion)
	}
	return nil
}

func Decode(body []byte) (Message, error) {
	var message Message
	if err := json.Unmarshal(body, &message); err != nil {
		return Message{}, fmt.Errorf("decoding message: %w", err)
	}
	if err := message.Validate(); err != nil {
		return Message{}, err
	}
	return message, nil
}
