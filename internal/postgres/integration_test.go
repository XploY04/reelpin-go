//go:build integration

package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/XploY04/reelpin-go/internal/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	testSchema = "reelpin_go_test"
	userA      = "11111111-1111-4111-8111-111111111111"
	userB      = "99999999-9999-4999-8999-999999999999"
)

// schemaFixture is the smallest slice of the Supabase tables these readers touch.
const schemaFixture = `
CREATE TABLE reels (
    id UUID PRIMARY KEY,
    user_id TEXT NOT NULL,
    url TEXT NOT NULL,
    normalized_url TEXT,
    source_platform TEXT,
    source_content_type TEXT,
    source_content_id TEXT,
    processing_version TEXT,
    ingestion_method TEXT,
    transcript_source TEXT,
    thumbnail_url TEXT,
    title TEXT NOT NULL DEFAULT 'Untitled',
    summary TEXT DEFAULT '',
    transcript TEXT DEFAULT '',
    category TEXT DEFAULT 'Other',
    subcategory TEXT DEFAULT 'Other',
    secondary_categories JSONB DEFAULT '[]'::jsonb,
    key_facts JSONB DEFAULT '[]'::jsonb,
    locations JSONB DEFAULT '[]'::jsonb,
    people_mentioned JSONB DEFAULT '[]'::jsonb,
    actionable_items JSONB DEFAULT '[]'::jsonb,
    events JSONB NOT NULL DEFAULT '[]'::jsonb,
    parse_status TEXT NOT NULL DEFAULT 'parsed',
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE processing_jobs (
    id UUID PRIMARY KEY,
    user_id TEXT NOT NULL,
    url TEXT NOT NULL,
    normalized_url TEXT,
    source_platform TEXT,
    source_content_type TEXT,
    source_content_id TEXT,
    processing_version TEXT,
    ingestion_method TEXT,
    transcript_source TEXT,
    status TEXT NOT NULL DEFAULT 'queued',
    current_step TEXT DEFAULT 'queued',
    progress_percent INTEGER NOT NULL DEFAULT 0,
    failure_code TEXT,
    error_message TEXT,
    result_reel_id UUID,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    next_retry_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    claimed_by TEXT,
    step_durations JSONB NOT NULL DEFAULT '{}'::jsonb,
    collection_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);
`

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parsing TEST_DATABASE_URL: %v", err)
	}
	// Everything happens in a throwaway schema, never in public.
	config.ConnConfig.RuntimeParams["search_path"] = testSchema

	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)

	for _, statement := range []string{
		"DROP SCHEMA IF EXISTS " + testSchema + " CASCADE",
		"CREATE SCHEMA " + testSchema,
		schemaFixture,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("preparing schema: %v", err)
		}
	}
	return pool
}

func seed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	rows := []struct {
		id          string
		userID      string
		platform    any
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
			id: "aaaaaaaa-0000-4000-8000-000000000003", userID: userA, platform: nil,
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
		_, err := pool.Exec(ctx,
			`INSERT INTO reels (id, user_id, url, source_platform, source_content_type, title,
			 summary, transcript, category, subcategory, secondary_categories, key_facts, locations, created_at)
			 VALUES ($1,$2,$3,$4,'reels',$5,'summary','transcript text',$6,$7,'["tag"]','["fact"]',$8::jsonb,$9)`,
			row.id, row.userID, "https://example.com/"+row.id, row.platform, row.title,
			row.category, row.subcategory, row.locations, row.createdAt,
		)
		if err != nil {
			t.Fatalf("seeding reel: %v", err)
		}
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
			`INSERT INTO processing_jobs (id, user_id, url, status, current_step, result_reel_id,
			 step_durations, collection_ids, created_at)
			 VALUES ($1,$2,$3,$4,$4,$5,'{"downloading":1.5}'::jsonb,'["fedcba98-0000-4000-8000-000000000001"]'::jsonb,$6)`,
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

	t.Run("sorting", func(t *testing.T) {
		for sort, wantFirst := range map[string]string{
			"newest": "Zeta link",
			"oldest": "Beta cafes",
			"title":  "Alpha thread",
			"junk":   "Zeta link",
		} {
			records, err := reader.List(ctx, userA, reels.ListOptions{Limit: 50, Sort: sort})
			if err != nil {
				t.Fatalf("List(%s): %v", sort, err)
			}
			if records[0].Title != wantFirst {
				t.Errorf("sort %q first = %q, want %q", sort, records[0].Title, wantFirst)
			}
		}
	})

	t.Run("offset and limit", func(t *testing.T) {
		page, err := reader.List(ctx, userA, reels.ListOptions{Limit: 1, Offset: 1, Sort: "oldest"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page) != 1 || page[0].Title != "Alpha thread" {
			t.Fatalf("page = %+v", page)
		}
	})

	t.Run("platform aliases and the other bucket", func(t *testing.T) {
		cases := map[string]int{
			"x":         1, // stored as twitter
			"instagram": 1,
			"other":     1, // stored as NULL
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
	if all[0].StepDurations["downloading"] != 1.5 {
		t.Errorf("step_durations = %+v", all[0].StepDurations)
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
