// Package notify sends push notifications and runs campaigns.
//
// Two rules shape everything here. A device token is a credential, so it is
// never logged, never returned, and never put in an error. And every send is
// keyed by an event, so a redelivered message or a retried request cannot make
// a phone buzz twice for the same thing.
package notify

import (
	"context"
	"errors"
	"strings"
)

// ErrNotFound is a notification or campaign that is not this caller's to touch.
var ErrNotFound = errors.New("not found")

// ErrNoDeviceTokens means the user has no device registered yet. It is not a
// failure: the app often registers its token seconds after the first share.
var ErrNoDeviceTokens = errors.New("the user has no device tokens")

// MaxTokensPerBatch is the provider's multicast limit.
const MaxTokensPerBatch = 500

// Message is what a device receives. Data carries the routing the app's tap
// handler reads.
type Message struct {
	Title string
	Body  string
	Data  map[string]string
}

// Delivery is what happened to one token.
type Delivery struct {
	Token string
	// MessageID is set when the provider accepted it.
	MessageID string
	// Invalid means the token is dead and should be removed. Retrying it will
	// never work.
	Invalid bool
	// Retryable means the provider was unavailable, not the token wrong.
	Retryable bool
	Err       error
}

// Sender is the provider seam. Everything above it works in deliveries.
type Sender interface {
	Send(ctx context.Context, tokens []string, message Message) ([]Delivery, error)
}

// Notification is one thing to tell one user about.
type Notification struct {
	UserID   string
	Title    string
	Body     string
	Type     string
	Target   string
	TargetID string
	// EventKey makes a send idempotent. Two attempts with the same key produce
	// one notification.
	EventKey string
	Data     map[string]string
}

// EventKey is the default key: one notification per user per thing.
func EventKey(notificationType, userID, targetID string) string {
	return strings.Join([]string{notificationType, strings.TrimSpace(userID), strings.TrimSpace(targetID)}, ":")
}

// ReelReady is the notification a finished save produces.
func ReelReady(userID, reelID, jobID, title, sourcePlatform, sourceContentType string) Notification {
	body := "Reel saved and is ready in ReelPin."
	if cleaned := strings.TrimSpace(title); cleaned != "" {
		body = cleaned + " is ready in ReelPin."
	}

	return Notification{
		UserID:   userID,
		Title:    reelReadyTitle(sourcePlatform, sourceContentType),
		Body:     body,
		Type:     "reel_ready",
		Target:   "reel_detail",
		TargetID: reelID,
		EventKey: EventKey("reel_ready", userID, reelID),
		// These four fields are what the app's tap handler routes on. Changing
		// them changes where a tap lands.
		Data: map[string]string{
			"schema_version": "1",
			"type":           "reel_ready",
			"target":         "reel_detail",
			"reel_id":        reelID,
			"job_id":         jobID,
		},
	}
}

// reelReadyTitle names the kind of thing that was saved, so the notification
// reads like the app rather than like a system message.
func reelReadyTitle(sourcePlatform, sourceContentType string) string {
	switch strings.ToLower(strings.TrimSpace(sourceContentType)) {
	case "reel", "reels":
		return "Reel saved"
	case "post", "carousel":
		return "Post saved"
	case "short":
		return "Short saved"
	case "video":
		return "Video saved"
	case "link", "page":
		return "Link saved"
	}
	if strings.TrimSpace(sourcePlatform) != "" {
		return "Saved from " + strings.TrimSpace(sourcePlatform)
	}
	return "Saved to ReelPin"
}

// CollectionUpdated is the notification a collection change produces.
func CollectionUpdated(userID, collectionID, actorName string, added int) Notification {
	body := "Something new was added."
	switch {
	case actorName != "" && added == 1:
		body = actorName + " added an item."
	case actorName != "":
		body = actorName + " added " + plural(added) + " items."
	case added == 1:
		body = "An item was added."
	default:
		body = plural(added) + " items were added."
	}

	return Notification{
		UserID:   userID,
		Title:    "Collection updated",
		Body:     body,
		Type:     "collection_updated",
		Target:   "collection_detail",
		TargetID: collectionID,
		Data: map[string]string{
			"schema_version": "1",
			"type":           "collection_updated",
			"target":         "collection_detail",
			"collection_id":  collectionID,
		},
	}
}

func plural(count int) string {
	if count < 0 {
		count = 0
	}
	digits := []byte{}
	for value := count; value > 0; value /= 10 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
	}
	if len(digits) == 0 {
		return "0"
	}
	return string(digits)
}
