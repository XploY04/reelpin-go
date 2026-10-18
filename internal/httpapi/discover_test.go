package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/XploY04/reelpin-go/internal/reels"
)

func decodeDiscover(t *testing.T, body []byte) reels.DiscoverResponse {
	t.Helper()
	var response reels.DiscoverResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("body is not a discover response: %v", err)
	}
	return response
}

func discoverReader() *fakeReels {
	record := sampleReel(testReelID, testUserID)
	return &fakeReels{
		records: []reels.ReelRecord{record},
		facets: []reels.FacetRow{
			{Category: stringPtr("food_and_drink"), Subcategory: stringPtr("cafes"), Count: 1},
		},
	}
}

func TestDiscover(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = discoverReader()

	rec := serve(deps, "GET", "/api/v2/discover", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	response := decodeDiscover(t, rec.Body.Bytes())
	if len(response.RecentSaves) != 1 {
		t.Errorf("recent_saves = %d, want 1", len(response.RecentSaves))
	}
	if response.RecentSavesCount != 1 {
		t.Errorf("recent_saves_count = %d, want 1", response.RecentSavesCount)
	}
	// sampleReel is saved two days before the fixed clock: 2026-09-02.
	if len(response.SavedDates) != 1 || response.SavedDates[0] != "2026-09-02" {
		t.Errorf("saved_dates = %v, want [2026-09-02]", response.SavedDates)
	}
	if response.SelectedDate != nil {
		t.Errorf("selected_date = %v, want nil with no filter", response.SelectedDate)
	}
	if len(response.CategoryGrid) != 1 || response.CategoryGrid[0].Category != "food_and_drink" {
		t.Errorf("category_grid = %+v, want one food_and_drink entry", response.CategoryGrid)
	}
	if len(response.QuickSearchPrompts) == 0 {
		t.Errorf("quick_search_prompts is empty")
	}
	if response.Pagination.HasMore {
		t.Errorf("has_more = true with a single save")
	}
	if response.Pagination.Limit != discoverDefaultPageSize {
		t.Errorf("limit = %d, want %d", response.Pagination.Limit, discoverDefaultPageSize)
	}
}

func TestDiscoverSelectedDate(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = discoverReader()

	rec := serve(deps, "GET", "/api/v2/discover?saved_date=2026-09-02", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	response := decodeDiscover(t, rec.Body.Bytes())
	if response.SelectedDate == nil || *response.SelectedDate != "2026-09-02" {
		t.Fatalf("selected_date = %v, want 2026-09-02", response.SelectedDate)
	}
	if len(response.ReelsForSelectedDate) != 1 {
		t.Errorf("reels_for_selected_date = %d, want 1", len(response.ReelsForSelectedDate))
	}
}

func TestDiscoverRejectsBadDate(t *testing.T) {
	rec := serve(testDeps(&fakePinger{}), "GET", "/api/v2/discover?saved_date=09-2026", "Bearer good.token")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "validation_error" {
		t.Errorf("error code = %q, want validation_error", code)
	}
}

func TestDiscoverRejectsBadCursor(t *testing.T) {
	rec := serve(testDeps(&fakePinger{}), "GET", "/api/v2/discover?cursor=25", "Bearer good.token")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestDiscoverFailure(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{err: errFake}

	rec := serve(deps, "GET", "/api/v2/discover", "Bearer good.token")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "discover_failed" {
		t.Errorf("error code = %q, want discover_failed", code)
	}
}

func TestDiscoverRequiresAuth(t *testing.T) {
	rec := serve(testDeps(&fakePinger{}), "GET", "/api/v2/discover", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
