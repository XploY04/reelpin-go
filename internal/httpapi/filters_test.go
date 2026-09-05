package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/reels"
)

func facetFixture() []reels.FacetRow {
	return []reels.FacetRow{
		{SourcePlatform: stringPtr("instagram"), Category: stringPtr("food"), Subcategory: stringPtr("cafes"), Count: 3},
		{SourcePlatform: stringPtr("instagram"), Category: stringPtr("food"), Subcategory: stringPtr("bakeries"), Count: 3},
		{SourcePlatform: stringPtr("twitter"), Category: stringPtr("tech"), Subcategory: stringPtr("ai"), Count: 4},
		{SourcePlatform: stringPtr("mystery"), Category: stringPtr("misc"), Subcategory: nil, Count: 1},
		{SourcePlatform: nil, Category: nil, Subcategory: nil, Count: 2},
		{SourcePlatform: stringPtr("youtube"), Category: stringPtr("food"), Subcategory: stringPtr("cafes"), Count: 0},
	}
}

func TestPlatformFiltersTree(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{facets: facetFixture()}

	rec := serve(deps, "GET", "/api/v1/reels/filters?platform=x&category=tech", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var body reels.PlatformFiltersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not a filter tree: %v", err)
	}

	if body.TotalCount != 13 {
		t.Errorf("total_count = %d, want 13", body.TotalCount)
	}
	// twitter folds into x (4), other collects the unknown and the null (3),
	// instagram has 6, so instagram leads.
	wantOrder := []struct {
		platform string
		count    int
	}{
		{"instagram", 6},
		{"x", 4},
		{"other", 3},
	}
	if len(body.Platforms) != len(wantOrder) {
		t.Fatalf("platforms = %d, want %d", len(body.Platforms), len(wantOrder))
	}
	for i, want := range wantOrder {
		if body.Platforms[i].Platform != want.platform || body.Platforms[i].Count != want.count {
			t.Errorf("platform %d = %s/%d, want %s/%d", i,
				body.Platforms[i].Platform, body.Platforms[i].Count, want.platform, want.count)
		}
	}
	if body.TopPlatform == nil || *body.TopPlatform != "instagram" {
		t.Errorf("top_platform = %v, want instagram", body.TopPlatform)
	}
	// The query never prunes the tree, it only counts the selection.
	if body.SelectedPreviewCount != 4 {
		t.Errorf("selected_preview_count = %d, want 4", body.SelectedPreviewCount)
	}
	if label := body.Platforms[1].Label; label != "X" {
		t.Errorf("x label = %q, want X", label)
	}

	// Equal counts fall back to the name, and a blank subcategory reads as Other.
	instagram := body.Platforms[0]
	if len(instagram.Categories) != 1 || len(instagram.Categories[0].Subcategories) != 2 {
		t.Fatalf("instagram tree = %+v", instagram.Categories)
	}
	if instagram.Categories[0].Subcategories[0].Name != "bakeries" {
		t.Errorf("tied subcategories are not name-ordered: %+v", instagram.Categories[0].Subcategories)
	}
}

func TestCategoryFiltersPrunedByPlatform(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{facets: facetFixture()}

	rec := serve(deps, "GET", "/api/v1/reels/category-filters?platform=instagram&category=food", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var body reels.CategoryFiltersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not a category tree: %v", err)
	}

	if body.UserID != testUserID {
		t.Errorf("user_id = %q, want %q", body.UserID, testUserID)
	}
	if body.TotalCount != 6 {
		t.Errorf("total_count = %d, want 6 (instagram only)", body.TotalCount)
	}
	if body.TotalCategories != 1 || body.Categories[0].Category != "food" {
		t.Fatalf("categories = %+v", body.Categories)
	}
	if body.Categories[0].Label != "Food" {
		t.Errorf("label = %q, want Food", body.Categories[0].Label)
	}
	if body.TopCategory.Category == nil || *body.TopCategory.Category != "food" || body.TopCategory.Count != 6 {
		t.Errorf("top_category = %+v", body.TopCategory)
	}
	if body.SelectedPreviewCount == nil || *body.SelectedPreviewCount != 6 {
		t.Errorf("selected_preview_count = %v, want 6", body.SelectedPreviewCount)
	}
}

func TestCategoryFiltersWithoutSelection(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{facets: facetFixture()}

	rec := serve(deps, "GET", "/api/v1/reels/category-filters", "Bearer good.token")
	var body reels.CategoryFiltersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not a category tree: %v", err)
	}
	if body.SelectedPreviewCount != nil {
		t.Errorf("selected_preview_count = %v, want null", *body.SelectedPreviewCount)
	}
	if body.TotalCount != 13 {
		t.Errorf("total_count = %d, want 13", body.TotalCount)
	}
}

func TestEmptyLibraryFilters(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{}

	for _, path := range []string{"/api/v1/reels/filters", "/api/v1/reels/category-filters"} {
		rec := serve(deps, "GET", path, "Bearer good.token")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"total_count":0`) {
			t.Errorf("%s body = %s, want total_count 0", path, rec.Body.String())
		}
	}
}

func TestFilterFailuresAreOpaque(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{err: errFake}

	for path, wantCode := range map[string]string{
		"/api/v1/reels/filters":          "reel_platform_filters_failed",
		"/api/v1/reels/category-filters": "reel_category_filters_failed",
	} {
		rec := serve(deps, "GET", path, "Bearer good.token")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s status = %d, want 500", path, rec.Code)
		}
		if code := decodeError(t, rec).ErrorCode; code != wantCode {
			t.Errorf("%s error_code = %q, want %q", path, code, wantCode)
		}
	}
}
