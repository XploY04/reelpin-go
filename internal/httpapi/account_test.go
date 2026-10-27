package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/XploY04/reelpin-go/internal/reels"
)

func TestLibraryStats(t *testing.T) {
	stats := reels.LibraryStats{
		TotalReels: 7, TotalPinnedLocations: 3, TotalTags: 5,
		TotalCategories: 2, TotalSubcategories: 4,
	}
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{stats: stats}

	rec := serve(deps, "GET", "/api/v2/account/library-stats", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body reels.LibraryStats
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not library stats: %v", err)
	}
	if body != stats {
		t.Errorf("stats = %+v, want %+v", body, stats)
	}
}

func TestLibraryStatsFailure(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{err: errFake}

	rec := serve(deps, "GET", "/api/v2/account/library-stats", "Bearer good.token")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "library_stats_failed" {
		t.Errorf("error_code = %q, want library_stats_failed", code)
	}
}

func TestEmptyLibraryStats(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{}

	rec := serve(deps, "GET", "/api/v2/account/library-stats", "Bearer good.token")
	var body reels.LibraryStats
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not library stats: %v", err)
	}
	if body != (reels.LibraryStats{}) {
		t.Errorf("stats = %+v, want zeros", body)
	}
}
