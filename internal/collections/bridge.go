package collections

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/google/uuid"
)

var ErrBridgeInput = errors.New("invalid collection bridge input")
var ErrBridgeDeleted = errors.New("collection actor was deleted")

type Snapshot struct {
	ID                  string          `json:"id"`
	UserID              string          `json:"user_id"`
	URL                 string          `json:"url"`
	NormalizedURL       string          `json:"normalized_url"`
	SourcePlatform      string          `json:"source_platform"`
	SourceContentType   string          `json:"source_content_type"`
	SourceContentID     *string         `json:"source_content_id"`
	Title               string          `json:"title"`
	Summary             string          `json:"summary"`
	Caption             string          `json:"caption"`
	Transcript          string          `json:"transcript"`
	Category            string          `json:"category"`
	Subcategory         string          `json:"subcategory"`
	SecondaryCategories []string        `json:"secondary_categories"`
	KeyFacts            []string        `json:"key_facts"`
	Locations           json.RawMessage `json:"locations"`
	PeopleMentioned     json.RawMessage `json:"people_mentioned"`
	ActionableItems     json.RawMessage `json:"actionable_items"`
	Events              json.RawMessage `json:"events"`
	ThumbnailURL        string          `json:"thumbnail_url"`
	ParseStatus         string          `json:"parse_status"`
	CreatedAt           *time.Time      `json:"created_at"`
}

type BridgeStore interface {
	Store
	AcceptBridgeInvite(context.Context, string, string) (Collection, bool, error)
	EnsureBridgeUser(context.Context, string) error
	CreateImported(context.Context, string, string, string, []string, []Snapshot) (Collection, int, error)
	AddImported(context.Context, string, string, []string, []Snapshot) (Collection, int, error)
	FileImported(context.Context, string, []string, []string, []Snapshot) ([]string, error)
	EditableTargets(context.Context, string, []string) ([]string, error)
	VisibleReel(context.Context, string, string) (reels.ReelRecord, error)
	DeleteImported(context.Context, string, string) error
	DeleteBridgeAccount(context.Context, string) error
}

func ParseSnapshots(raw []json.RawMessage, userID string, ids []string) ([]Snapshot, error) {
	if len(raw) > MaxSaveIDsPerRequest || len(ids) > MaxSaveIDsPerRequest {
		return nil, ErrBridgeInput
	}
	requested := map[string]bool{}
	for _, id := range ids {
		if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil {
			return nil, ErrBridgeInput
		}
		requested[id] = true
	}
	snapshots := make([]Snapshot, 0, len(raw))
	seen := map[string]bool{}
	for _, body := range raw {
		var item Snapshot
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, ErrBridgeInput
		}
		if parsed, err := uuid.Parse(item.ID); err != nil || parsed == uuid.Nil {
			return nil, ErrBridgeInput
		}
		if item.UserID != userID || !requested[item.ID] || seen[item.ID] || strings.TrimSpace(item.URL) == "" {
			return nil, ErrBridgeInput
		}
		for _, list := range []json.RawMessage{item.Locations, item.PeopleMentioned, item.ActionableItems, item.Events} {
			if len(list) > 0 && string(list) != "null" {
				var values []json.RawMessage
				if json.Unmarshal(list, &values) != nil {
					return nil, ErrBridgeInput
				}
			}
		}
		seen[item.ID] = true
		snapshots = append(snapshots, item)
	}
	return snapshots, nil
}
