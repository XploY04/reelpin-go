package reels

import (
	"sort"
	"strings"
	"time"
)

// DiscoverPagination pages the recent-saves list forward with the same cursor
// the reel list uses. total_count is the deduped size of the scanned set, so the
// client can show how far through the library it is.
type DiscoverPagination struct {
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
	Limit      int     `json:"limit"`
	TotalCount int     `json:"total_count"`
}

// DiscoverResponse is the Discover tab: the newest saves, the dates with saves,
// the reels on a chosen date, the category grid, and a few search prompts. It
// carries no folders: that feature is retired.
type DiscoverResponse struct {
	RecentSaves          []DisplayReel      `json:"recent_saves"`
	RecentSavesCount     int                `json:"recent_saves_count"`
	SavedDates           []string           `json:"saved_dates"`
	SelectedDate         *string            `json:"selected_date"`
	ReelsForSelectedDate []DisplayReel      `json:"reels_for_selected_date"`
	CategoryGrid         []CategoryFilter   `json:"category_grid"`
	QuickSearchPrompts   []string           `json:"quick_search_prompts"`
	Pagination           DiscoverPagination `json:"pagination"`
}

// DiscoverScanLimit is how many of a user's saves the aggregate sections read.
// It matches the Python display query, which caps at 5000 before aggregating,
// so recent_saves_count and total_count stop at this number.
const DiscoverScanLimit = 5000

// BuildDiscoverResponse assembles the tab from one over-fetched page of recent
// saves, a capped scan of the library for the aggregates, the facet rows for the
// category grid, and the reels on selectedDate as the reader filtered them.
// recentPage is fetched with limit+1 so the extra row answers has_more, exactly
// like the reel list.
func BuildDiscoverResponse(userID string, recentPage, scanned, dateReels []ReelRecord, facets []FacetRow, selectedDate string, limit int, now time.Time) DiscoverResponse {
	hasMore := len(recentPage) > limit
	page := recentPage
	if hasMore {
		page = page[:limit]
	}
	recent := displayReels(DedupeRecords(page), now)

	library := DedupeRecords(scanned)

	var selected *string
	reelsForDate := []DisplayReel{}
	if trimmed := strings.TrimSpace(selectedDate); trimmed != "" {
		selected = &trimmed
		reelsForDate = displayReels(DedupeRecords(dateReels), now)
	}

	pagination := DiscoverPagination{HasMore: hasMore, Limit: limit, TotalCount: len(library)}
	if hasMore && len(page) > 0 {
		if cursor, ok := CursorFor(page[len(page)-1]); ok {
			encoded := cursor.Encode()
			pagination.NextCursor = &encoded
		} else {
			pagination.HasMore = false
		}
	}

	return DiscoverResponse{
		RecentSaves:          recent,
		RecentSavesCount:     len(library),
		SavedDates:           distinctSavedDates(library),
		SelectedDate:         selected,
		ReelsForSelectedDate: reelsForDate,
		CategoryGrid:         BuildCategoryFilters(userID, facets, nil, "", "").Categories,
		QuickSearchPrompts:   buildQuickSearchPrompts(library),
		Pagination:           pagination,
	}
}

func displayReels(records []ReelRecord, now time.Time) []DisplayReel {
	display := make([]DisplayReel, 0, len(records))
	for _, record := range records {
		display = append(display, BuildDisplayReel(record, now))
	}
	return display
}

// distinctSavedDates lists the saved-date keys newest first. The records arrive
// newest first, so the sort only guards against an unordered caller.
func distinctSavedDates(records []ReelRecord) []string {
	seen := map[string]bool{}
	dates := []string{}
	for _, record := range records {
		key := SavedDateKey(record.CreatedAt)
		if key == nil || *key == "" || seen[*key] {
			continue
		}
		seen[*key] = true
		dates = append(dates, *key)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	return dates
}

const maxQuickSearchPrompts = 5

var fallbackQuickSearchPrompts = []string{"Travel", "Food", "Fitness", "Movies", "Coffee"}

// promptStopWords are the generic labels that make a useless prompt.
var promptStopWords = map[string]bool{
	"other": true, "misc": true, "miscellaneous": true, "general": true,
	"uncategorized": true, "none": true, "unknown": true,
}

// buildQuickSearchPrompts ranks prompts by kind, in the order a specific one
// beats a broad one: subcategory, category, location, source, tag, then person.
// Within a kind the most frequent wins. Generic labels are dropped, and the
// list is topped up from a fixed fallback so it is never empty.
//
// This is a deliberate simplification of the Python ranking (one per kind then a
// global fill, composite "Category in Location" and "Platform ContentType"
// prompts, keyword mining, weighted scores). It was not ported for v2.
func buildQuickSearchPrompts(records []ReelRecord) []string {
	subcategory := map[string]int{}
	category := map[string]int{}
	location := map[string]int{}
	source := map[string]int{}
	tag := map[string]int{}
	person := map[string]int{}

	for _, record := range records {
		if label := promptLabel(record.Subcategory); label != "" {
			subcategory[label]++
		}
		if label := promptLabel(record.Category); label != "" {
			category[label]++
		}
		for _, loc := range record.Locations {
			if label := promptLabel(loc.Name); label != "" {
				location[label]++
			}
		}
		if record.SourcePlatform != nil {
			if label := promptLabel(PlatformLabel(RecordPlatform(record.SourcePlatform))); label != "" {
				source[label]++
			}
		}
		for _, secondary := range record.SecondaryCategories {
			if label := promptLabel(secondary); label != "" {
				tag[label]++
			}
		}
		for _, name := range record.PeopleMentioned {
			if label := promptRaw(name); label != "" {
				person[label]++
			}
		}
	}

	prompts := []string{}
	seen := map[string]bool{}
	for _, counts := range []map[string]int{subcategory, category, location, source, tag, person} {
		for _, name := range sortedByCountThenName(counts, true) {
			key := strings.ToLower(name)
			if seen[key] {
				continue
			}
			seen[key] = true
			prompts = append(prompts, name)
			if len(prompts) >= maxQuickSearchPrompts {
				return prompts
			}
		}
	}

	for _, fallback := range fallbackQuickSearchPrompts {
		key := strings.ToLower(fallback)
		if seen[key] {
			continue
		}
		seen[key] = true
		prompts = append(prompts, fallback)
		if len(prompts) >= maxQuickSearchPrompts {
			break
		}
	}
	return prompts
}

func promptLabel(value string) string {
	cleaned := strings.TrimSpace(value)
	if cleaned == "" || promptStopWords[strings.ToLower(cleaned)] {
		return ""
	}
	label := Labelize(cleaned)
	if promptStopWords[strings.ToLower(label)] {
		return ""
	}
	return label
}

func promptRaw(value string) string {
	cleaned := strings.TrimSpace(value)
	if cleaned == "" || promptStopWords[strings.ToLower(cleaned)] {
		return ""
	}
	return cleaned
}
