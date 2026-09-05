package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/reels"
)

// ReelResponse is the full single-reel body, transcript included.
type ReelResponse struct {
	ID                  string           `json:"id"`
	UserID              string           `json:"user_id"`
	URL                 string           `json:"url"`
	ThumbnailURL        *string          `json:"thumbnail_url"`
	NormalizedURL       *string          `json:"normalized_url"`
	SourcePlatform      *string          `json:"source_platform"`
	SourceContentType   *string          `json:"source_content_type"`
	SourceContentID     *string          `json:"source_content_id"`
	ProcessingVersion   *string          `json:"processing_version"`
	IngestionMethod     *string          `json:"ingestion_method"`
	TranscriptSource    *string          `json:"transcript_source"`
	Title               string           `json:"title"`
	Summary             string           `json:"summary"`
	ContentType         string           `json:"content_type"`
	Transcript          string           `json:"transcript"`
	Category            string           `json:"category"`
	Subcategory         string           `json:"subcategory"`
	SecondaryCategories []string         `json:"secondary_categories"`
	KeyFacts            []string         `json:"key_facts"`
	Locations           []reels.Location `json:"locations"`
	PeopleMentioned     []string         `json:"people_mentioned"`
	ActionableItems     []string         `json:"actionable_items"`
	CreatedAt           *string          `json:"created_at"`
}

func (s *Server) handleListReels(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	platforms, ok := parsePlatforms(w, query)
	if !ok {
		return
	}
	limit, ok := intParam(w, query, "limit", reels.DefaultPageSize, 1, reels.MaxPageSize)
	if !ok {
		return
	}
	savedDate, ok := savedDateParam(w, query)
	if !ok {
		return
	}

	var after *reels.Cursor
	if raw := strings.TrimSpace(query.Get("cursor")); raw != "" {
		cursor, err := reels.DecodeCursor(raw)
		if err != nil {
			// A cursor this service did not issue cannot be honoured, and
			// guessing a position would silently skip or repeat saves.
			validationError(w, "cursor", "is not a cursor this API issued")
			return
		}
		after = &cursor
	}

	// One extra row answers has_more without a second count query.
	records, err := s.deps.Reels.List(r.Context(), requestUserID(r), reels.ListOptions{
		Platforms:   platforms,
		Category:    query.Get("category"),
		Subcategory: query.Get("subcategory"),
		SavedDate:   savedDate,
		After:       after,
		Limit:       limit + 1,
	})
	if err != nil {
		s.deps.Logger.Error("list reels failed", "error", err)
		internalError(w, "reel_list_failed", "Could not load reels right now.")
		return
	}

	writeJSON(w, http.StatusOK, reels.BuildListResponse(records, limit, s.now()))
}

func (s *Server) handleGetReel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "reel_id", "reel_not_found")
	if !ok {
		return
	}

	record, err := s.deps.Reels.Get(r.Context(), requestUserID(r), id)
	if errors.Is(err, reels.ErrNotFound) {
		notFoundError(w, "reel_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("get reel failed", "error", err)
		internalError(w, "reel_lookup_failed", "Could not load the reel right now.")
		return
	}

	writeJSON(w, http.StatusOK, buildReelResponse(record))
}

func (s *Server) handlePlatformFilters(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	platforms, ok := parsePlatforms(w, query)
	if !ok {
		return
	}

	rows, err := s.deps.Reels.Facets(r.Context(), requestUserID(r))
	if err != nil {
		s.deps.Logger.Error("list reel platform filters failed", "error", err)
		internalError(w, "reel_platform_filters_failed", "Could not load filters right now.")
		return
	}

	writeJSON(w, http.StatusOK, reels.BuildPlatformFilters(
		rows, platforms, query.Get("category"), query.Get("subcategory"),
	))
}

func (s *Server) handleCategoryFilters(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	platforms, ok := parsePlatforms(w, query)
	if !ok {
		return
	}

	userID := requestUserID(r)
	rows, err := s.deps.Reels.Facets(r.Context(), userID)
	if err != nil {
		s.deps.Logger.Error("list reel category filters failed", "error", err)
		internalError(w, "reel_category_filters_failed", "Could not load category filters right now.")
		return
	}

	writeJSON(w, http.StatusOK, reels.BuildCategoryFilters(
		userID, rows, platforms, query.Get("category"), query.Get("subcategory"),
	))
}

func buildReelResponse(record reels.ReelRecord) ReelResponse {
	category := record.Category
	if strings.TrimSpace(category) == "" {
		category = "Other"
	}
	subcategory := record.Subcategory
	if strings.TrimSpace(subcategory) == "" {
		subcategory = "Other"
	}

	locations := record.Locations
	if locations == nil {
		locations = []reels.Location{}
	}

	return ReelResponse{
		ID:                  record.ID,
		UserID:              record.UserID,
		URL:                 record.URL,
		ThumbnailURL:        cleanOptionalText(record.ThumbnailURL),
		NormalizedURL:       record.NormalizedURL,
		SourcePlatform:      record.SourcePlatform,
		SourceContentType:   record.SourceContentType,
		SourceContentID:     record.SourceContentID,
		ProcessingVersion:   record.ProcessingVersion,
		IngestionMethod:     record.IngestionMethod,
		TranscriptSource:    record.TranscriptSource,
		Title:               record.Title,
		Summary:             record.Summary,
		ContentType:         reels.ShareCardContentType(record),
		Transcript:          record.Transcript,
		Category:            category,
		Subcategory:         subcategory,
		SecondaryCategories: nonNilStrings(record.SecondaryCategories),
		KeyFacts:            nonNilStrings(record.KeyFacts),
		Locations:           locations,
		PeopleMentioned:     nonNilStrings(record.PeopleMentioned),
		ActionableItems:     nonNilStrings(record.ActionableItems),
		CreatedAt:           isoTimestamp(record.CreatedAt),
	}
}

func cleanOptionalText(value *string) *string {
	if value == nil {
		return nil
	}
	cleaned := strings.TrimSpace(*value)
	if cleaned == "" {
		return nil
	}
	return &cleaned
}

func isoTimestamp(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
