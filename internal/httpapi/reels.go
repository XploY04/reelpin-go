package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/cache"
	"github.com/XploY04/reelpin-go/internal/reels"
)

// filterCacheTTL matches the Python category-filter cache. Reel and job detail
// are deliberately never cached: they must reflect a write immediately.
const filterCacheTTL = 5 * time.Minute

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
	limit, ok := intParam(w, query, "limit", 50, 1, 100)
	if !ok {
		return
	}
	offset, ok := intParam(w, query, "offset", 0, 0, 1<<31-1)
	if !ok {
		return
	}
	savedDate, ok := savedDateParam(w, query)
	if !ok {
		return
	}

	// An integer cursor wins over offset; anything else falls back to it.
	if cursor := strings.TrimSpace(query.Get("cursor")); cursor != "" {
		if parsed, err := strconv.Atoi(cursor); err == nil {
			offset = max(0, parsed)
		}
	}

	// One extra row answers has_more without a second count query.
	records, err := s.deps.Reels.List(r.Context(), requestUserID(r), reels.ListOptions{
		Platforms:   platforms,
		Category:    query.Get("category"),
		Subcategory: query.Get("subcategory"),
		SavedDate:   savedDate,
		Sort:        query.Get("sort"),
		Offset:      offset,
		Limit:       limit + 1,
	})
	if err != nil {
		s.deps.Logger.Error("list reels failed", "error", err)
		internalError(w, "reel_list_failed", "Could not load reels right now.")
		return
	}

	hasMore := len(records) > limit
	if hasMore {
		records = records[:limit]
	}
	totalCount := offset + len(records)
	if hasMore {
		totalCount++
	}

	writeJSON(w, http.StatusOK, reels.BuildListResponse(records, totalCount, limit, offset, s.now()))
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

	userID := requestUserID(r)
	// The tree is rebuilt from one grouped query, so it is cheap to lose and
	// safe to cache: a Redis failure just means loading it again.
	response, err := cache.GetOrLoad(r.Context(), s.deps.Cache, userID, "reel_platform_filters",
		query.Get("platform")+"|"+query.Get("category")+"|"+query.Get("subcategory"),
		filterCacheTTL,
		func(ctx context.Context) (reels.PlatformFiltersResponse, error) {
			rows, err := s.deps.Reels.Facets(ctx, userID)
			if err != nil {
				return reels.PlatformFiltersResponse{}, err
			}
			return reels.BuildPlatformFilters(rows, platforms, query.Get("category"), query.Get("subcategory")), nil
		})
	if err != nil {
		s.deps.Logger.Error("list reel platform filters failed", "error", err)
		internalError(w, "reel_platform_filters_failed", "Could not load filters right now.")
		return
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleCategoryFilters(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	platforms, ok := parsePlatforms(w, query)
	if !ok {
		return
	}

	userID := requestUserID(r)
	response, err := cache.GetOrLoad(r.Context(), s.deps.Cache, userID, "reel_category_filters",
		query.Get("platform")+"|"+query.Get("category")+"|"+query.Get("subcategory"),
		filterCacheTTL,
		func(ctx context.Context) (reels.CategoryFiltersResponse, error) {
			rows, err := s.deps.Reels.Facets(ctx, userID)
			if err != nil {
				return reels.CategoryFiltersResponse{}, err
			}
			return reels.BuildCategoryFilters(userID, rows, platforms, query.Get("category"), query.Get("subcategory")), nil
		})
	if err != nil {
		s.deps.Logger.Error("list reel category filters failed", "error", err)
		internalError(w, "reel_category_filters_failed", "Could not load category filters right now.")
		return
	}

	writeJSON(w, http.StatusOK, response)
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
