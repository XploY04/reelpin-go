//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const fullReelID = "22222222-2222-4222-8222-222222222222"

// fullExtraction is the raw_extraction "extraction" object exactly as the
// pipeline marshals ai.Extraction, which is also what the legacy mirror wrote
// into the public.reels JSONB columns.
const fullExtraction = `{
	"title": "Best cafes in Goa", "summary": "Three cafes worth the ride.",
	"content_domain": "food", "content_format": "list",
	"topical_tags": ["cafe", "goa"], "key_facts": ["Opens at 8am"],
	"locations": [
		{"name": "Artjuna Cafe", "neighborhood": "", "city": "Anjuna", "state": "", "country": "",
		 "latitude": 15.5834, "longitude": 73.7407},
		{"name": "No coordinates", "neighborhood": "", "city": "", "state": "", "country": ""}
	],
	"people_mentioned": ["Rhea"], "actionable_items": ["Go early"],
	"events": [{"name": "Market", "date": "2026-09-05", "time": ""}]
}`

func ptr[T any](value T) *T { return &value }

// legacyEquivalent is the record public.reels produced for the same save when
// the pipeline mirrored it there: the reader contract the canonical projection
// has to keep.
func legacyEquivalent(savedAt time.Time) reels.ReelRecord {
	empty := ""
	return reels.ReelRecord{
		ID:                fullReelID,
		UserID:            userA,
		URL:               "https://www.instagram.com/reel/abc/?igsh=shared",
		NormalizedURL:     ptr("https://example.com/abc"),
		SourcePlatform:    ptr("instagram"),
		SourceContentType: ptr("reel"),
		SourceContentID:   ptr("abc"),
		ProcessingVersion: ptr("v1"),
		ThumbnailURL:      ptr("https://cdn.example.com/abc.jpg"),
		Title:             "Best cafes in Goa",
		Summary:           "Three cafes worth the ride.",
		Transcript:        "spoken words",
		Category:          "food_and_drink",
		Subcategory:       "cafes",
		SecondaryCategories: []string{
			"cafe", "goa",
		},
		KeyFacts: []string{"Opens at 8am"},
		Locations: []reels.Location{
			{Name: "Artjuna Cafe", Neighborhood: &empty, City: ptr("Anjuna"), State: &empty, Country: &empty,
				Latitude: ptr(15.5834), Longitude: ptr(73.7407)},
			{Name: "No coordinates", Neighborhood: &empty, City: &empty, State: &empty, Country: &empty},
		},
		PeopleMentioned: []string{"Rhea"},
		ActionableItems: []string{"Go early"},
		Events:          []reels.Event{{Name: "Market", Date: "2026-09-05"}},
		ParseStatus:     ptr("parsed"),
		CreatedAt:       &savedAt,
	}
}

func TestACanonicalSaveReadsAsTheLegacyRowDid(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	savedAt := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	seedCanonicalReel(t, pool, canonicalReel{
		id: fullReelID, userID: userA, platform: "instagram", sourceID: "abc",
		category: "food_and_drink", subcategory: "cafes",
		title: "Best cafes in Goa", summary: "Three cafes worth the ride.", transcript: "spoken words",
		tags: []string{"cafe", "goa"}, keyFacts: []string{"Opens at 8am"},
		extraction: fullExtraction, thumbnail: "https://cdn.example.com/abc.jpg",
		savedAt: savedAt,
	})
	// The link the user submitted survives only on their job; the earliest one
	// is the one the legacy mirror kept.
	for _, job := range []struct {
		url     string
		created time.Time
	}{
		{"https://www.instagram.com/reel/abc/?igsh=shared", savedAt.Add(-time.Minute)},
		{"https://www.instagram.com/reel/abc/?again", savedAt.Add(time.Hour)},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO reelpin.processing_jobs (user_id, user_save_id, url, normalized_url, status, created_at)
			VALUES ($1, $2, $3, 'https://example.com/abc', 'completed', $4)`,
			userA, fullReelID, job.url, job.created); err != nil {
			t.Fatalf("seeding a job: %v", err)
		}
	}

	reader := NewReels(pool)
	want := legacyEquivalent(savedAt)

	got, err := reader.Get(ctx, userA, uuid.MustParse(fullReelID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertRecord(t, "get", got, want)

	listed, err := reader.List(ctx, userA, reels.ListOptions{Limit: 25})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed = %d, want 1", len(listed))
	}
	wantListed := want
	wantListed.Transcript = ""
	assertRecord(t, "list", listed[0], wantListed)

	library, err := reader.Library(ctx, userA, 10)
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	wantLibrary := reels.ReelRecord{
		ID: fullReelID, SourcePlatform: want.SourcePlatform, Category: want.Category,
		Subcategory: want.Subcategory, SecondaryCategories: want.SecondaryCategories,
		Locations: want.Locations, PeopleMentioned: want.PeopleMentioned, CreatedAt: want.CreatedAt,
	}
	if len(library) != 1 {
		t.Fatalf("library = %d, want 1", len(library))
	}
	assertRecord(t, "library", library[0], wantLibrary)

	facets, err := reader.Facets(ctx, userA)
	if err != nil {
		t.Fatalf("Facets: %v", err)
	}
	wantFacets := []reels.FacetRow{{
		SourcePlatform: ptr("instagram"), Category: ptr("food_and_drink"), Subcategory: ptr("cafes"), Count: 1,
	}}
	if !reflect.DeepEqual(facets, wantFacets) {
		t.Errorf("facets = %+v, want %+v", facets, wantFacets)
	}

	stats, err := reader.Stats(ctx, userA)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	wantStats := reels.LibraryStats{
		TotalReels: 1, TotalCategories: 1, TotalSubcategories: 1, TotalTags: 2, TotalPinnedLocations: 1,
	}
	if stats != wantStats {
		t.Errorf("stats = %+v, want %+v", stats, wantStats)
	}

	// The detail response keeps the contract's exact key set.
	display := reels.Display{Now: savedAt.AddDate(0, 0, 2), WebBaseURL: "https://reelpin.in", View: reels.ViewDetailed}
	detail := jsonObject(t, reels.BuildReel(got, display))
	fixture := readFixture(t, "reel_detail.json")
	if gotKeys, wantKeys := slices.Sorted(maps.Keys(detail)), slices.Sorted(maps.Keys(fixture)); !slices.Equal(gotKeys, wantKeys) {
		t.Errorf("detail keys = %v, want the fixture's %v", gotKeys, wantKeys)
	}
}

// TestASaveWithNoVersionStillLists keeps every save visible: a content whose
// version was removed lists untitled, as loadSavedReel answers it, rather than
// vanishing from the library and its counts.
func TestASaveWithNoVersionStillLists(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		WITH content AS (
			INSERT INTO reelpin.contents
				(source_platform, source_content_type, source_content_id,
				 normalized_url, normalized_url_hash, access_scope_hash)
			VALUES ('instagram', 'reel', 'BARE', 'https://example.com/BARE', 'BARE', 'public')
			RETURNING id
		)
		INSERT INTO reelpin.user_saves (id, user_id, content_id)
		SELECT $1, $2, id FROM content`, fullReelID, userA); err != nil {
		t.Fatalf("seeding a bare save: %v", err)
	}

	reader := NewReels(pool)
	listed, err := reader.List(ctx, userA, reels.ListOptions{Limit: 25})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != fullReelID || listed[0].Title != "" {
		t.Fatalf("listed = %+v, want the one untitled save", listed)
	}
	if listed[0].URL != "https://example.com/BARE" {
		t.Errorf("url = %q, want the normalized URL when no job is left", listed[0].URL)
	}
	if listed[0].ParseStatus != nil {
		t.Errorf("parse_status = %v, want null", *listed[0].ParseStatus)
	}
	stats, err := reader.Stats(ctx, userA)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalReels != 1 {
		t.Errorf("total_reels = %d, want 1", stats.TotalReels)
	}
}

// TestNoReelReaderCrossesUsers seeds user A only and reads as user B through
// every reader.
func TestNoReelReaderCrossesUsers(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM reelpin.user_saves WHERE user_id = $1`, userB); err != nil {
		t.Fatal(err)
	}
	reader := NewReels(pool)

	listed, err := reader.List(ctx, userB, reels.ListOptions{Limit: 50})
	if err != nil || len(listed) != 0 {
		t.Errorf("List = %d rows, %v; want none", len(listed), err)
	}
	if _, err := reader.Get(ctx, userB, uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001")); err != reels.ErrNotFound {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
	facets, err := reader.Facets(ctx, userB)
	if err != nil || len(facets) != 0 {
		t.Errorf("Facets = %+v, %v; want none", facets, err)
	}
	library, err := reader.Library(ctx, userB, 50)
	if err != nil || len(library) != 0 {
		t.Errorf("Library = %d rows, %v; want none", len(library), err)
	}
	stats, err := reader.Stats(ctx, userB)
	if err != nil || stats != (reels.LibraryStats{}) {
		t.Errorf("Stats = %+v, %v; want zero", stats, err)
	}
}

// recordingQuerier remembers the last statement a reader sent, so a test can
// EXPLAIN the exact SQL rather than a copy of it.
type recordingQuerier struct {
	Querier
	sql  string
	args []any
}

func (r *recordingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.sql, r.args = sql, args
	return r.Querier.Query(ctx, sql, args...)
}

// TestTheReelListUsesTheSaveIndex is the hot path: one user's newest saves,
// first page and a resumed page, must come off user_saves_list_idx rather than
// a scan of every save.
func TestTheReelListUsesTheSaveIndex(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO auth.users (id)
		SELECT ('00000000-0000-4000-8000-' || lpad(u::text, 12, '0'))::uuid
		FROM generate_series(1, 40) u`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		WITH content AS (
			INSERT INTO reelpin.contents
				(source_platform, source_content_type, source_content_id,
				 normalized_url, normalized_url_hash, access_scope_hash)
			SELECT 'instagram', 'reel', 'BULK' || n, 'https://example.com/' || n, 'BULK' || n, 'public'
			FROM generate_series(1, 4000) n
			RETURNING id, source_content_id
		)
		INSERT INTO reelpin.user_saves (user_id, content_id, saved_at)
		SELECT ('00000000-0000-4000-8000-' || lpad((substr(source_content_id, 5)::int % 40 + 1)::text, 12, '0'))::uuid,
		       id, now() - (substr(source_content_id, 5)::int || ' minutes')::interval
		FROM content`); err != nil {
		t.Fatalf("seeding saves: %v", err)
	}
	for _, table := range []string{"reelpin.user_saves", "reelpin.contents", "reelpin.content_versions", "reelpin.processing_jobs"} {
		if _, err := pool.Exec(ctx, `ANALYZE `+table); err != nil {
			t.Fatal(err)
		}
	}

	user := "00000000-0000-4000-8000-000000000007"
	recorder := &recordingQuerier{Querier: pool}
	reader := NewReels(recorder)
	first, err := reader.List(ctx, user, reels.ListOptions{Limit: 25})
	if err != nil || len(first) != 25 {
		t.Fatalf("List = %d rows, %v", len(first), err)
	}
	assertUsesIndex(t, pool, recorder, "user_saves_list_idx")

	cursor, _ := reels.CursorFor(first[len(first)-1])
	if _, err := reader.List(ctx, user, reels.ListOptions{Limit: 25, After: &cursor}); err != nil {
		t.Fatalf("resumed List: %v", err)
	}
	assertUsesIndex(t, pool, recorder, "user_saves_list_idx")
}

func assertUsesIndex(t *testing.T, pool Querier, recorder *recordingQuerier, index string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), "EXPLAIN "+recorder.sql, recorder.args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if !strings.Contains(plan.String(), index) {
		t.Fatalf("the reel list does not use %s:\n%s", index, plan.String())
	}
}

func assertRecord(t *testing.T, name string, got, want reels.ReelRecord) {
	t.Helper()
	if got.CreatedAt == nil || want.CreatedAt == nil || !got.CreatedAt.Equal(*want.CreatedAt) {
		t.Errorf("%s created_at = %v, want %v", name, got.CreatedAt, want.CreatedAt)
	}
	got.CreatedAt, want.CreatedAt = nil, nil
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", " ")
		wantJSON, _ := json.MarshalIndent(want, "", " ")
		t.Errorf("%s record differs from the legacy row:\n got %s\nwant %s", name, gotJSON, wantJSON)
	}
}

func readFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../api/fixtures/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Response map[string]any `json:"response"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.Response
}
