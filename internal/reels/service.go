package reels

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Service is the reel use cases. The app's handlers, the website and the MCP
// server all call these, so every client gets the same shapes from the same
// code path.
type Service struct {
	reader     ReelReader
	webBaseURL string
	now        func() time.Time
}

func NewService(reader ReelReader, webBaseURL string, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{reader: reader, webBaseURL: webBaseURL, now: now}
}

// ListParams narrow and page the library. After nil starts at the newest save.
type ListParams struct {
	Platforms   []string
	Category    string
	Subcategory string
	SavedDate   string
	After       *Cursor
	Limit       int
	View        View
}

// List answers one page of the user's saves.
func (s *Service) List(ctx context.Context, userID string, params ListParams) (ListResponse, error) {
	// One extra row answers has_more without a second count query.
	records, err := s.reader.List(ctx, userID, ListOptions{
		Platforms:   params.Platforms,
		Category:    params.Category,
		Subcategory: params.Subcategory,
		SavedDate:   params.SavedDate,
		After:       params.After,
		Limit:       params.Limit + 1,
	})
	if err != nil {
		return ListResponse{}, err
	}
	return BuildListResponse(records, params.Limit, s.display(params.View)), nil
}

// Get answers one reel in full. ErrNotFound covers a reel that belongs to
// someone else.
func (s *Service) Get(ctx context.Context, userID string, id uuid.UUID) (Reel, error) {
	record, err := s.reader.Get(ctx, userID, id)
	if err != nil {
		return Reel{}, err
	}
	return BuildReel(record, s.display(ViewDetailed)), nil
}

// Card answers one reel in the list's shape.
func (s *Service) Card(ctx context.Context, userID string, id uuid.UUID, view View) (DisplayReel, error) {
	record, err := s.reader.Get(ctx, userID, id)
	if err != nil {
		return DisplayReel{}, err
	}
	return BuildDisplayReel(record, s.display(view)), nil
}

// Present is the full shape for a record a caller already holds, such as the
// reel a submission found already saved.
func (s *Service) Present(record ReelRecord) Reel {
	return BuildReel(record, s.display(ViewDetailed))
}

// DiscoverParams page the recent saves and optionally choose one saved date.
type DiscoverParams struct {
	After     *Cursor
	Limit     int
	SavedDate string
}

// Discover assembles the Discover tab.
func (s *Service) Discover(ctx context.Context, userID string, params DiscoverParams) (DiscoverResponse, error) {
	// The recent-saves page over-fetches by one to answer has_more, the same
	// way the reel list does.
	recent, err := s.reader.List(ctx, userID, ListOptions{After: params.After, Limit: params.Limit + 1})
	if err != nil {
		return DiscoverResponse{}, fmt.Errorf("reading recent saves: %w", err)
	}

	// The aggregate sections (saved dates, the search prompts) read a capped
	// scan of the library, narrower than a page of full reels.
	scanned, err := s.reader.Library(ctx, userID, DiscoverScanLimit)
	if err != nil {
		return DiscoverResponse{}, fmt.Errorf("scanning the library: %w", err)
	}

	// The chosen date is filtered by the reader, so a date older than the scan
	// still returns its reels.
	var dateReels []ReelRecord
	if params.SavedDate != "" {
		dateReels, err = s.reader.List(ctx, userID, ListOptions{
			SavedDate: params.SavedDate,
			Limit:     DiscoverScanLimit,
		})
		if err != nil {
			return DiscoverResponse{}, fmt.Errorf("reading the selected date: %w", err)
		}
	}

	facets, err := s.reader.Facets(ctx, userID)
	if err != nil {
		return DiscoverResponse{}, fmt.Errorf("reading the category grid: %w", err)
	}

	return BuildDiscoverResponse(userID, recent, scanned, dateReels, facets,
		params.SavedDate, params.Limit, s.display(ViewDetailed)), nil
}

func (s *Service) display(view View) Display {
	return Display{Now: s.now().UTC(), WebBaseURL: s.webBaseURL, View: view}
}
