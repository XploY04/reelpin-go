//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "99999999-9999-4999-8999-999999999999"
)

// testPool is a migrated database shaped like Neon, with users A and B. The
// migrations are the only schema: no Python table exists for a reader to
// resolve by accident.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := rawPool(t)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO auth.users (id) VALUES ($1), ($2)`, userA, userB); err != nil {
		t.Fatalf("seeding users: %v", err)
	}
	return pool
}

// canonicalReel is one user's save of its own content, written the way the
// pipeline persists it: the category and the extraction lists ride the
// version's raw_extraction, the tags and key facts its typed arrays, the
// thumbnail its media.
type canonicalReel struct {
	id          string
	userID      string
	platform    string
	contentType string
	sourceID    string
	category    string
	subcategory string
	title       string
	summary     string
	transcript  string
	tags        []string
	keyFacts    []string
	extraction  string
	thumbnail   string
	savedAt     time.Time
}

func seedCanonicalReel(t *testing.T, pool *pgxpool.Pool, row canonicalReel) {
	t.Helper()
	ctx := context.Background()
	if row.contentType == "" {
		row.contentType = "reel"
	}
	if row.extraction == "" {
		row.extraction = `{}`
	}
	var contentID, versionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id,
			 normalized_url, normalized_url_hash, access_scope_hash)
		VALUES ($1, $2, $3, 'https://example.com/'||$3, $3, 'public')
		RETURNING id::text`, row.platform, row.contentType, row.sourceID).Scan(&contentID); err != nil {
		t.Fatalf("seeding content: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, prompt_version, schema_version, model_version,
			 title, summary, transcript, tags, key_facts, raw_extraction, media)
		VALUES ($1, 'v1', 'p1', 's1', 'm1', $2, $3, $4, $5, $6,
		        jsonb_build_object('category', $7::text, 'subcategory', $8::text,
		                           'extraction', $9::jsonb),
		        jsonb_build_object('thumbnail_url', $10::text))
		RETURNING id::text`,
		contentID, row.title, row.summary, row.transcript, row.tags, row.keyFacts,
		row.category, row.subcategory, row.extraction, row.thumbnail).Scan(&versionID); err != nil {
		t.Fatalf("seeding a version: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.contents SET current_version_id = $2 WHERE id = $1`, contentID, versionID); err != nil {
		t.Fatalf("pointing at the version: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.user_saves (id, user_id, content_id, saved_at) VALUES ($1, $2, $3, $4)`,
		row.id, row.userID, contentID, row.savedAt); err != nil {
		t.Fatalf("seeding a save: %v", err)
	}
}

func seed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	rows := []struct {
		id          string
		userID      string
		platform    string
		category    string
		subcategory string
		title       string
		createdAt   time.Time
		locations   string
	}{
		{
			id: "aaaaaaaa-0000-4000-8000-000000000001", userID: userA, platform: "instagram",
			category: "food", subcategory: "cafes", title: "Beta cafes",
			createdAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
			locations: `[{"name":"Artjuna","city":"Anjuna","latitude":15.58,"longitude":73.74},{"name":"No pin"}]`,
		},
		{
			id: "aaaaaaaa-0000-4000-8000-000000000002", userID: userA, platform: "twitter",
			category: "tech", subcategory: "ai", title: "Alpha thread",
			createdAt: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC),
			locations: `[]`,
		},
		{
			id: "aaaaaaaa-0000-4000-8000-000000000003", userID: userA, platform: "web",
			category: "misc", subcategory: "links", title: "Zeta link",
			createdAt: time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC),
			locations: `[]`,
		},
		{
			id: "bbbbbbbb-0000-4000-8000-000000000001", userID: userB, platform: "instagram",
			category: "food", subcategory: "cafes", title: "Someone else's reel",
			createdAt: time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC),
			locations: `[{"name":"Elsewhere","latitude":1.0,"longitude":2.0}]`,
		},
	}

	for _, row := range rows {
		seedCanonicalReel(t, pool, canonicalReel{
			id: row.id, userID: row.userID, platform: row.platform, sourceID: row.id,
			category: row.category, subcategory: row.subcategory,
			title: row.title, summary: "summary", transcript: "transcript text",
			tags: []string{"tag"}, keyFacts: []string{"fact"},
			extraction: `{"locations":` + row.locations + `}`,
			savedAt:    row.createdAt,
		})
	}

	jobRows := []struct {
		id      string
		userID  string
		status  string
		reelID  any
		created time.Time
	}{
		{id: "cccccccc-0000-4000-8000-000000000001", userID: userA, status: "queued", created: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)},
		{id: "cccccccc-0000-4000-8000-000000000002", userID: userA, status: "completed", reelID: "aaaaaaaa-0000-4000-8000-000000000001", created: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)},
		{id: "dddddddd-0000-4000-8000-000000000001", userID: userB, status: "processing", created: time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)},
	}
	for _, row := range jobRows {
		_, err := pool.Exec(ctx,
			`INSERT INTO reelpin.processing_jobs (id, user_id, url, normalized_url, status, current_step,
			 user_save_id, collection_ids, created_at)
			 VALUES ($1,$2,$3,$3,$4,$4,$5,'{fedcba98-0000-4000-8000-000000000001}',$6)`,
			row.id, row.userID, "https://example.com/job", row.status, row.reelID, row.created,
		)
		if err != nil {
			t.Fatalf("seeding job: %v", err)
		}
	}
}

func TestReelsListIsolationAndFilters(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	reader := NewReels(pool)
	ctx := context.Background()

	t.Run("only the caller's rows", func(t *testing.T) {
		records, err := reader.List(ctx, userA, reels.ListOptions{Limit: 50})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(records) != 3 {
			t.Fatalf("records = %d, want 3", len(records))
		}
		for _, record := range records {
			if record.UserID != userA {
				t.Fatalf("List returned a row owned by %q", record.UserID)
			}
			if record.Transcript != "" {
				t.Error("the list query must not select transcript")
			}
		}
	})

	t.Run("jsonb decodes into typed slices", func(t *testing.T) {
		records, _ := reader.List(ctx, userA, reels.ListOptions{Limit: 50, Category: "food"})
		if len(records) != 1 {
			t.Fatalf("records = %d, want 1", len(records))
		}
		record := records[0]
		if len(record.Locations) != 2 || record.Locations[0].Name != "Artjuna" {
			t.Fatalf("locations = %+v", record.Locations)
		}
		if record.Locations[0].Latitude == nil || *record.Locations[0].Latitude != 15.58 {
			t.Errorf("latitude = %v", record.Locations[0].Latitude)
		}
		if len(record.KeyFacts) != 1 || record.KeyFacts[0] != "fact" {
			t.Errorf("key_facts = %v", record.KeyFacts)
		}
		if len(reels.BuildMappableLocations(record)) != 1 {
			t.Error("only the located entry is mappable")
		}
	})

	t.Run("one order, newest saved first", func(t *testing.T) {
		records, err := reader.List(ctx, userA, reels.ListOptions{Limit: 50})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if records[0].Title != "Zeta link" {
			t.Errorf("first = %q, want the newest save", records[0].Title)
		}
		for index := 1; index < len(records); index++ {
			previous, current := records[index-1].CreatedAt, records[index].CreatedAt
			if previous != nil && current != nil && current.After(*previous) {
				t.Fatalf("rows are not newest-first at index %d", index)
			}
		}
	})

	t.Run("a cursor resumes exactly where the page ended", func(t *testing.T) {
		first, err := reader.List(ctx, userA, reels.ListOptions{Limit: 1})
		if err != nil {
			t.Fatalf("first page: %v", err)
		}
		cursor, ok := reels.CursorFor(first[0])
		if !ok {
			t.Fatal("the first row has no resumable position")
		}

		rest, err := reader.List(ctx, userA, reels.ListOptions{Limit: 50, After: &cursor})
		if err != nil {
			t.Fatalf("second page: %v", err)
		}
		for _, record := range rest {
			if record.ID == first[0].ID {
				t.Fatal("the cursor repeated the row it resumes after")
			}
		}
		if len(rest) == 0 {
			t.Fatal("the cursor skipped every remaining row")
		}
	})

	t.Run("platform aliases and the other bucket", func(t *testing.T) {
		cases := map[string]int{
			"x":         1, // stored as twitter
			"instagram": 1,
			"other":     1, // stored as web
		}
		for platform, want := range cases {
			records, err := reader.List(ctx, userA, reels.ListOptions{Limit: 50, Platforms: []string{platform}})
			if err != nil {
				t.Fatalf("List(%s): %v", platform, err)
			}
			if len(records) != want {
				t.Errorf("platform %q = %d rows, want %d", platform, len(records), want)
			}
		}

		both, err := reader.List(ctx, userA, reels.ListOptions{Limit: 50, Platforms: []string{"x", "other"}})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(both) != 2 {
			t.Errorf("x+other = %d rows, want 2", len(both))
		}
	})

	t.Run("saved date", func(t *testing.T) {
		records, err := reader.List(ctx, userA, reels.ListOptions{Limit: 50, SavedDate: "2026-09-02"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(records) != 1 || records[0].Title != "Alpha thread" {
			t.Errorf("saved_date rows = %+v", records)
		}
	})
}

func TestReelGetIsUserScoped(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	reader := NewReels(pool)
	ctx := context.Background()

	own := uuid.UUID{}
	own, err := uuid.Parse("aaaaaaaa-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	record, err := reader.Get(ctx, userA, own)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if record.Transcript != "transcript text" {
		t.Errorf("transcript = %q, want the detail column", record.Transcript)
	}

	foreign, _ := uuid.Parse("bbbbbbbb-0000-4000-8000-000000000001")
	if _, err := reader.Get(ctx, userA, foreign); err != reels.ErrNotFound {
		t.Fatalf("cross-user Get error = %v, want ErrNotFound", err)
	}

	missing, _ := uuid.Parse("eeeeeeee-0000-4000-8000-000000000009")
	if _, err := reader.Get(ctx, userA, missing); err != reels.ErrNotFound {
		t.Fatalf("missing Get error = %v, want ErrNotFound", err)
	}
}

func TestFacetsAndStats(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	reader := NewReels(pool)
	ctx := context.Background()

	facets, err := reader.Facets(ctx, userA)
	if err != nil {
		t.Fatalf("Facets: %v", err)
	}
	if len(facets) != 3 {
		t.Fatalf("facets = %d, want 3", len(facets))
	}
	total := 0
	for _, row := range facets {
		total += row.Count
	}
	if total != 3 {
		t.Errorf("facet counts total %d, want 3", total)
	}

	tree := reels.BuildPlatformFilters(facets, nil, "", "")
	if tree.TotalCount != 3 || len(tree.Platforms) != 3 {
		t.Errorf("platform tree = %+v", tree)
	}

	stats, err := reader.Stats(ctx, userA)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	want := reels.LibraryStats{
		TotalReels: 3, TotalPinnedLocations: 1, TotalTags: 1,
		TotalCategories: 3, TotalSubcategories: 3,
	}
	if stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
}

func TestJobsAreUserScoped(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	reader := NewJobs(pool)
	ctx := context.Background()

	all, err := reader.List(ctx, userA, false, 20)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("jobs = %d, want 2", len(all))
	}
	if all[0].CreatedAt.Before(*all[1].CreatedAt) {
		t.Error("jobs are not newest first")
	}
	if len(all[0].CollectionIDs) != 1 {
		t.Errorf("collection_ids = %+v, want one id", all[0].CollectionIDs)
	}

	active, err := reader.List(ctx, userA, true, 20)
	if err != nil {
		t.Fatalf("List(active): %v", err)
	}
	if len(active) != 1 || active[0].Status != jobs.StatusQueued {
		t.Errorf("active jobs = %+v", active)
	}

	foreign, _ := uuid.Parse("dddddddd-0000-4000-8000-000000000001")
	if _, err := reader.Get(ctx, userA, foreign); err != jobs.ErrNotFound {
		t.Fatalf("cross-user Get error = %v, want ErrNotFound", err)
	}
}

func TestLibraryIsUserScopedNewestFirstAndCapped(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	reader := NewReels(pool)
	ctx := context.Background()

	records, err := reader.Library(ctx, userA, 2)
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want the limit of 2", len(records))
	}
	if records[0].ID != "aaaaaaaa-0000-4000-8000-000000000003" || records[1].ID != "aaaaaaaa-0000-4000-8000-000000000002" {
		t.Errorf("order = %s, %s, want newest first", records[0].ID, records[1].ID)
	}
	if records[1].Category != "tech" || len(records[1].SecondaryCategories) != 1 || records[1].CreatedAt == nil {
		t.Errorf("projection lost a column: %+v", records[1])
	}

	all, err := reader.Library(ctx, userA, 50)
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("records = %d, want only user A's 3", len(all))
	}
}
