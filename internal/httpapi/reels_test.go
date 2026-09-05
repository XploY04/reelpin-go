package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/reels"
)

func listServer(reader *fakeReels) Deps {
	deps := testDeps(&fakePinger{})
	deps.Reels = reader
	return deps
}

func decodeList(t *testing.T, rec *httptest.ResponseRecorder) reels.ListResponse {
	t.Helper()
	var body reels.ListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not a list response: %v", rec.Body.String(), err)
	}
	return body
}

func manyReels(count int) []reels.ReelRecord {
	records := make([]reels.ReelRecord, 0, count)
	for i := 0; i < count; i++ {
		record := sampleReel(testReelID, testUserID)
		record.ID = string(rune('a'+i)) + "-id"
		records = append(records, record)
	}
	return records
}

func TestListReelsPagination(t *testing.T) {
	tests := []struct {
		name           string
		query          string
		stored         int
		wantCount      int
		wantHasMore    bool
		wantOffset     int
		wantTotal      int
		wantNextCursor string
	}{
		{name: "defaults", query: "", stored: 3, wantCount: 3, wantTotal: 3},
		{
			name: "more rows than the limit", query: "?limit=2", stored: 5,
			wantCount: 2, wantHasMore: true, wantTotal: 3, wantNextCursor: "2",
		},
		{
			name: "offset carries into the next cursor", query: "?limit=2&offset=4", stored: 5,
			wantCount: 2, wantHasMore: true, wantOffset: 4, wantTotal: 7, wantNextCursor: "6",
		},
		{
			name: "cursor overrides offset", query: "?limit=2&offset=4&cursor=10", stored: 5,
			wantCount: 2, wantHasMore: true, wantOffset: 10, wantTotal: 13, wantNextCursor: "12",
		},
		{
			name: "invalid cursor falls back to offset", query: "?limit=2&offset=4&cursor=abc", stored: 5,
			wantCount: 2, wantHasMore: true, wantOffset: 4, wantTotal: 7, wantNextCursor: "6",
		},
		{
			name: "negative cursor clamps to zero", query: "?limit=2&offset=4&cursor=-3", stored: 5,
			wantCount: 2, wantHasMore: true, wantOffset: 0, wantTotal: 3, wantNextCursor: "2",
		},
		{name: "empty library", query: "", stored: 0, wantCount: 0, wantTotal: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &fakeReels{records: manyReels(tt.stored)}
			rec := serve(listServer(reader), "GET", "/api/v1/reels"+tt.query, "Bearer good.token")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
			}

			body := decodeList(t, rec)
			if len(body.Reels) != tt.wantCount {
				t.Errorf("reels = %d, want %d", len(body.Reels), tt.wantCount)
			}
			if body.HasMore != tt.wantHasMore {
				t.Errorf("has_more = %v, want %v", body.HasMore, tt.wantHasMore)
			}
			if body.Offset != tt.wantOffset {
				t.Errorf("offset = %d, want %d", body.Offset, tt.wantOffset)
			}
			if body.TotalCount != tt.wantTotal {
				t.Errorf("total_count = %d, want %d", body.TotalCount, tt.wantTotal)
			}
			if tt.wantNextCursor == "" {
				if body.NextCursor != nil {
					t.Errorf("next_cursor = %q, want null", *body.NextCursor)
				}
			} else if body.NextCursor == nil || *body.NextCursor != tt.wantNextCursor {
				t.Errorf("next_cursor = %v, want %q", body.NextCursor, tt.wantNextCursor)
			}
			// The reader is always asked for one row beyond the page.
			if reader.lastOptions.Limit != body.Limit+1 {
				t.Errorf("reader limit = %d, want %d", reader.lastOptions.Limit, body.Limit+1)
			}
			if reader.lastOptions.Offset != tt.wantOffset {
				t.Errorf("reader offset = %d, want %d", reader.lastOptions.Offset, tt.wantOffset)
			}
		})
	}
}

func TestListReelsFiltersReachTheReader(t *testing.T) {
	reader := &fakeReels{records: manyReels(1)}
	rec := serve(listServer(reader),
		"GET",
		"/api/v1/reels?platform=twitter,instagram&category=Food&subcategory=Cafes&saved_date=2026-09-01&sort=title",
		"Bearer good.token",
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	options := reader.lastOptions
	if len(options.Platforms) != 2 || options.Platforms[0] != "x" || options.Platforms[1] != "instagram" {
		t.Errorf("platforms = %v, want [x instagram]", options.Platforms)
	}
	if options.Category != "Food" || options.Subcategory != "Cafes" {
		t.Errorf("category/subcategory = %q/%q", options.Category, options.Subcategory)
	}
	if options.SavedDate != "2026-09-01" {
		t.Errorf("saved_date = %q", options.SavedDate)
	}
	if options.Sort != "title" {
		t.Errorf("sort = %q", options.Sort)
	}
}

func TestListReelsRejectsBadParameters(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantCode   string
	}{
		{name: "unknown platform", query: "?platform=myspace", wantStatus: http.StatusBadRequest, wantCode: "invalid_platform"},
		{name: "limit too high", query: "?limit=101", wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_error"},
		{name: "limit too low", query: "?limit=0", wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_error"},
		{name: "limit not a number", query: "?limit=many", wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_error"},
		{name: "negative offset", query: "?offset=-1", wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_error"},
		{name: "malformed saved_date", query: "?saved_date=01-09-2026", wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(testDeps(&fakePinger{}), "GET", "/api/v1/reels"+tt.query, "Bearer good.token")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			body := decodeError(t, rec)
			if body.ErrorCode != tt.wantCode {
				t.Errorf("error_code = %q, want %q", body.ErrorCode, tt.wantCode)
			}
			if tt.wantCode == "invalid_platform" && len(body.Allowed) == 0 {
				t.Error("allowed values missing from the invalid_platform body")
			}
		})
	}
}

func TestListReelsFailureIsOpaque(t *testing.T) {
	deps := listServer(&fakeReels{err: errFake})
	rec := serve(deps, "GET", "/api/v1/reels", "Bearer good.token")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := decodeError(t, rec)
	if body.ErrorCode != "reel_list_failed" {
		t.Errorf("error_code = %q, want reel_list_failed", body.ErrorCode)
	}
	if body.Detail == errFake.Error() {
		t.Error("detail leaks the reader error")
	}
}

func TestListReelsDisplayFormatting(t *testing.T) {
	reader := &fakeReels{records: []reels.ReelRecord{sampleReel(testReelID, testUserID)}}
	rec := serve(listServer(reader), "GET", "/api/v1/reels", "Bearer good.token")

	body := decodeList(t, rec)
	if len(body.Reels) != 1 {
		t.Fatalf("reels = %d, want 1", len(body.Reels))
	}
	reel := body.Reels[0]

	if reel.RelativeDate != "2 days ago" {
		t.Errorf("relative_date = %q, want \"2 days ago\"", reel.RelativeDate)
	}
	if reel.DisplayDate != "Sep 2, 2026" {
		t.Errorf("display_date = %q, want \"Sep 2, 2026\"", reel.DisplayDate)
	}
	if reel.SavedDateKey == nil || *reel.SavedDateKey != "2026-09-02" {
		t.Errorf("saved_date_key = %v, want 2026-09-02", reel.SavedDateKey)
	}
	if reel.CategoryLabel != "Food And Drink" || reel.SubCategoryLabel != "Cafes" {
		t.Errorf("labels = %q/%q", reel.CategoryLabel, reel.SubCategoryLabel)
	}
	if reel.ContentType != "reel" {
		t.Errorf("content_type = %q, want reel", reel.ContentType)
	}
	if !reel.HasMapLocations || reel.MapLocationCount != 1 {
		t.Errorf("map locations = %v/%d, want true/1", reel.HasMapLocations, reel.MapLocationCount)
	}
	location := reel.MappableLocations[0]
	if location.MarkerID != testReelID+":0" {
		t.Errorf("marker_id = %q", location.MarkerID)
	}
	if location.LocationDisplayLabel != "Artjuna Cafe" {
		t.Errorf("location label = %q", location.LocationDisplayLabel)
	}
	if location.GoogleMapsURL == nil || *location.GoogleMapsURL != "https://www.google.com/maps/search/?api=1&query=Artjuna%20Cafe" {
		t.Errorf("google_maps_url = %v", location.GoogleMapsURL)
	}
}

func TestGetReel(t *testing.T) {
	stored := sampleReel(testReelID, testUserID)
	reader := &fakeReels{byID: map[string]reels.ReelRecord{testReelID: stored}}

	tests := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{name: "own reel", path: "/api/v1/reels/" + testReelID, wantStatus: http.StatusOK},
		{name: "malformed id", path: "/api/v1/reels/not-a-uuid", wantStatus: http.StatusNotFound},
		{name: "missing id", path: "/api/v1/reels/55555555-5555-4555-8555-555555555555", wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(listServer(reader), "GET", tt.path, "Bearer good.token")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus == http.StatusNotFound {
				if code := decodeError(t, rec).ErrorCode; code != "reel_not_found" {
					t.Errorf("error_code = %q, want reel_not_found", code)
				}
				return
			}

			var body ReelResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not a reel: %v", err)
			}
			if body.Transcript != stored.Transcript {
				t.Errorf("transcript = %q, want %q", body.Transcript, stored.Transcript)
			}
			if len(body.Locations) != 2 {
				t.Errorf("locations = %d, want 2 (raw, not mappable)", len(body.Locations))
			}
		})
	}
}

func TestGetReelOwnedByAnotherUserIs404(t *testing.T) {
	reader := &fakeReels{byID: map[string]reels.ReelRecord{
		testReelID: sampleReel(testReelID, otherUserID),
	}}

	rec := serve(listServer(reader), "GET", "/api/v1/reels/"+testReelID, "Bearer good.token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec).ErrorCode; code != "reel_not_found" {
		t.Errorf("error_code = %q, want reel_not_found", code)
	}
}

func TestListReelsDoesNotReturnTranscripts(t *testing.T) {
	reader := &fakeReels{records: []reels.ReelRecord{sampleReel(testReelID, testUserID)}}
	rec := serve(listServer(reader), "GET", "/api/v1/reels", "Bearer good.token")

	if body := rec.Body.String(); strings.Contains(body, "spoken words") {
		t.Fatalf("list response carries a transcript: %s", body)
	}
}
