package mapview

import (
	"context"
	"sort"
	"strconv"

	"github.com/XploY04/reelpin-go/internal/reels"
)

// FolderSummary exists to keep the response shape. The list is always empty:
// folder discovery has been dead in the Python service since a shadowed
// function stopped returning anything, and no shipped client reads it.
type FolderSummary struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Note      *string `json:"note"`
	ReelCount int     `json:"reel_count"`
	UpdatedAt *string `json:"updated_at"`
}

type DiscoverResponse struct {
	Folders              []FolderSummary        `json:"folders"`
	RecentSaves          []reels.DisplayReel    `json:"recent_saves"`
	RecentSavesCount     int                    `json:"recent_saves_count"`
	SavedDates           []string               `json:"saved_dates"`
	SelectedDate         *string                `json:"selected_date"`
	ReelsForSelectedDate []reels.DisplayReel    `json:"reels_for_selected_date"`
	CategoryGrid         []reels.CategoryFilter `json:"category_grid"`
	QuickSearchPrompts   []string               `json:"quick_search_prompts"`
	Pagination           Pagination             `json:"pagination"`
}

type Pagination struct {
	NextCursor *string `json:"next_cursor"`
	NextOffset *int    `json:"next_offset"`
	HasMore    bool    `json:"has_more"`
	TotalCount int     `json:"total_count"`
	Limit      int     `json:"limit"`
	Offset     int     `json:"offset"`
}

// Discover is the home screen: what was saved recently, which days have saves,
// the category grid, and a few searches worth running.
func (s *Service) Discover(ctx context.Context, userID string, offset, limit int, selectedDate string) (DiscoverResponse, error) {
	library, err := s.library(ctx, userID)
	if err != nil {
		return DiscoverResponse{}, err
	}
	deduped := reels.DedupeRecords(library)
	now := s.now().UTC()

	// The library is already ordered newest first, so the page is a slice.
	recent := []reels.DisplayReel{}
	for index := offset; index < len(deduped) && len(recent) < limit; index++ {
		recent = append(recent, reels.BuildDisplayReel(deduped[index], now))
	}

	dateKeys := map[string]bool{}
	forSelectedDate := []reels.DisplayReel{}
	facets := map[string]map[string]int{}

	for _, record := range deduped {
		display := reels.BuildDisplayReel(record, now)
		if display.SavedDateKey != nil {
			dateKeys[*display.SavedDateKey] = true
			if selectedDate != "" && *display.SavedDateKey == selectedDate {
				forSelectedDate = append(forSelectedDate, display)
			}
		}
		category := display.Category
		if facets[category] == nil {
			facets[category] = map[string]int{}
		}
		facets[category][display.SubCategory]++
	}

	savedDates := make([]string, 0, len(dateKeys))
	for key := range dateKeys {
		savedDates = append(savedDates, key)
	}
	// Newest day first: that is the order the date strip is read in.
	sort.Sort(sort.Reverse(sort.StringSlice(savedDates)))

	response := DiscoverResponse{
		// Always empty; see FolderSummary.
		Folders:              []FolderSummary{},
		RecentSaves:          recent,
		RecentSavesCount:     len(deduped),
		SavedDates:           savedDates,
		ReelsForSelectedDate: forSelectedDate,
		CategoryGrid:         categoryGrid(facets),
		QuickSearchPrompts:   reels.BuildQuickSearchPrompts(deduped),
		Pagination: Pagination{
			TotalCount: len(deduped),
			Limit:      limit,
			Offset:     offset,
		},
	}
	if selectedDate != "" {
		response.SelectedDate = &selectedDate
	}

	nextOffset := offset + len(recent)
	if nextOffset < len(deduped) {
		cursor := strconv.Itoa(nextOffset)
		response.Pagination.HasMore = true
		response.Pagination.NextOffset = &nextOffset
		response.Pagination.NextCursor = &cursor
	}
	return response, nil
}

// categoryGrid reuses the filter tree builder, so Discover and the filters
// screen can never disagree about counts or ordering.
func categoryGrid(facets map[string]map[string]int) []reels.CategoryFilter {
	rows := []reels.FacetRow{}
	for category, subcategories := range facets {
		for subcategory, count := range subcategories {
			categoryName, subcategoryName := category, subcategory
			rows = append(rows, reels.FacetRow{
				Category:    &categoryName,
				Subcategory: &subcategoryName,
				Count:       count,
			})
		}
	}
	return reels.BuildCategoryFilters("", rows, nil, "", "").Categories
}
