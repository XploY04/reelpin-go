// Package reels holds the saved-reel domain types, the read port the API needs
// from storage, and the display shapes the Flutter client already expects.
package reels

import (
	"context"
	"errors"
	"time"

	"github.com/XploY04/reelpin-go/internal/uuid"
)

// ErrNotFound covers a missing row and a row owned by another user. Callers
// must not tell the two apart.
var ErrNotFound = errors.New("reel not found")

type Location struct {
	Name         string   `json:"name"`
	Address      *string  `json:"address"`
	Neighborhood *string  `json:"neighborhood"`
	City         *string  `json:"city"`
	State        *string  `json:"state"`
	Country      *string  `json:"country"`
	Latitude     *float64 `json:"latitude"`
	Longitude    *float64 `json:"longitude"`
}

type Event struct {
	Name string `json:"name"`
	Date string `json:"date"`
	Time string `json:"time"`
}

type ReelRecord struct {
	ID                  string
	UserID              string
	URL                 string
	NormalizedURL       *string
	SourcePlatform      *string
	SourceContentType   *string
	SourceContentID     *string
	ProcessingVersion   *string
	IngestionMethod     *string
	TranscriptSource    *string
	ThumbnailURL        *string
	Title               string
	Summary             string
	Transcript          string
	Category            string
	Subcategory         string
	SecondaryCategories []string
	KeyFacts            []string
	Locations           []Location
	PeopleMentioned     []string
	ActionableItems     []string
	Events              []Event
	ParseStatus         *string
	CreatedAt           *time.Time
}

type ListOptions struct {
	Platforms   []string
	Category    string
	Subcategory string
	SavedDate   string
	Sort        string
	Offset      int
	Limit       int
}

type FacetRow struct {
	SourcePlatform *string
	Category       *string
	Subcategory    *string
	Count          int
}

type LibraryStats struct {
	TotalReels           int `json:"total_reels"`
	TotalPinnedLocations int `json:"total_pinned_locations"`
	TotalTags            int `json:"total_tags"`
	TotalCategories      int `json:"total_categories"`
	TotalSubcategories   int `json:"total_subcategories"`
}

type ReelReader interface {
	List(context.Context, string, ListOptions) ([]ReelRecord, error)
	Get(context.Context, string, uuid.UUID) (ReelRecord, error)
	Facets(context.Context, string) ([]FacetRow, error)
	Stats(context.Context, string) (LibraryStats, error)
}
