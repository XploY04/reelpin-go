package reels

import (
	"testing"
	"time"
)

var discoverNow = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func discoverRecord(id, category, subcategory string, daysAgo int) ReelRecord {
	created := discoverNow.AddDate(0, 0, -daysAgo)
	return ReelRecord{
		ID:          id,
		UserID:      "user-1",
		URL:         "https://example.com/" + id,
		Title:       "Reel " + id,
		Category:    category,
		Subcategory: subcategory,
		CreatedAt:   &created,
	}
}

func TestBuildDiscoverResponseSections(t *testing.T) {
	records := []ReelRecord{
		discoverRecord("a", "food_and_drink", "cafes", 0),
		discoverRecord("b", "food_and_drink", "cafes", 1),
		discoverRecord("c", "travel", "beaches", 1),
	}
	facets := []FacetRow{
		{Category: strPtr("food_and_drink"), Subcategory: strPtr("cafes"), Count: 2},
		{Category: strPtr("travel"), Subcategory: strPtr("beaches"), Count: 1},
	}

	response := BuildDiscoverResponse("user-1", records, records, facets, "", 25, discoverNow)

	if response.RecentSavesCount != 3 {
		t.Errorf("recent_saves_count = %d, want 3", response.RecentSavesCount)
	}
	if len(response.RecentSaves) != 3 {
		t.Errorf("recent_saves = %d, want 3", len(response.RecentSaves))
	}
	// Two distinct days, newest first.
	wantDates := []string{"2026-09-04", "2026-09-03"}
	if len(response.SavedDates) != 2 || response.SavedDates[0] != wantDates[0] || response.SavedDates[1] != wantDates[1] {
		t.Errorf("saved_dates = %v, want %v", response.SavedDates, wantDates)
	}
	if len(response.CategoryGrid) != 2 {
		t.Errorf("category_grid = %d categories, want 2", len(response.CategoryGrid))
	}
	if response.SelectedDate != nil {
		t.Errorf("selected_date = %v, want nil", response.SelectedDate)
	}
}

func TestBuildDiscoverResponseSelectedDate(t *testing.T) {
	records := []ReelRecord{
		discoverRecord("a", "food_and_drink", "cafes", 0),
		discoverRecord("b", "travel", "beaches", 1),
	}

	response := BuildDiscoverResponse("user-1", records, records, nil, "2026-09-03", 25, discoverNow)

	if response.SelectedDate == nil || *response.SelectedDate != "2026-09-03" {
		t.Fatalf("selected_date = %v, want 2026-09-03", response.SelectedDate)
	}
	if len(response.ReelsForSelectedDate) != 1 || response.ReelsForSelectedDate[0].ID != "b" {
		t.Errorf("reels_for_selected_date = %+v, want only reel b", response.ReelsForSelectedDate)
	}
}

func TestBuildDiscoverResponsePaginates(t *testing.T) {
	// Three records with a page limit of two: the over-fetched third answers
	// has_more and yields a cursor.
	records := []ReelRecord{
		discoverRecord("a", "food", "x", 0),
		discoverRecord("b", "food", "x", 1),
		discoverRecord("c", "food", "x", 2),
	}

	response := BuildDiscoverResponse("user-1", records, records, nil, "", 2, discoverNow)

	if !response.Pagination.HasMore {
		t.Error("has_more = false with an over-fetched page")
	}
	if response.Pagination.NextCursor == nil {
		t.Error("next_cursor is nil when there is more")
	}
	if len(response.RecentSaves) != 2 {
		t.Errorf("recent_saves = %d, want the page trimmed to 2", len(response.RecentSaves))
	}
}

func TestQuickSearchPromptsPreferSpecificAndDropGeneric(t *testing.T) {
	records := []ReelRecord{
		discoverRecord("a", "food_and_drink", "cafes", 0),
		discoverRecord("b", "food_and_drink", "cafes", 1),
		discoverRecord("c", "Other", "Other", 2),
	}

	prompts := buildQuickSearchPrompts(DedupeRecords(records))

	if len(prompts) == 0 {
		t.Fatal("no prompts produced")
	}
	// Subcategory outranks category, so "Cafes" leads.
	if prompts[0] != "Cafes" {
		t.Errorf("first prompt = %q, want Cafes", prompts[0])
	}
	for _, prompt := range prompts {
		if prompt == "Other" {
			t.Errorf("a generic 'Other' label leaked into prompts: %v", prompts)
		}
	}
}

func TestQuickSearchPromptsFallBackWhenEmpty(t *testing.T) {
	prompts := buildQuickSearchPrompts(nil)
	if len(prompts) != maxQuickSearchPrompts {
		t.Fatalf("fallback prompts = %d, want %d", len(prompts), maxQuickSearchPrompts)
	}
	if prompts[0] != "Travel" {
		t.Errorf("fallback[0] = %q, want Travel", prompts[0])
	}
}

func strPtr(value string) *string { return &value }
