//go:build integration

package backfill

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"

	sharedReelA = "aaaaaaaa-0000-4000-8000-000000000001"
	sharedReelB = "aaaaaaaa-0000-4000-8000-000000000002"
	cachedReel  = "aaaaaaaa-0000-4000-8000-000000000003"
	linkReel    = "aaaaaaaa-0000-4000-8000-000000000004"
	brokenReel  = "aaaaaaaa-0000-4000-8000-000000000005"

	completedJob = "cccccccc-0000-4000-8000-000000000001"
	queuedJob    = "cccccccc-0000-4000-8000-000000000002"
	brokenJob    = "cccccccc-0000-4000-8000-000000000003"
)

// legacySchema is the shape the Python service left behind, trimmed to what the
// backfill reads. Supabase owns these tables in every real deployment, so no
// migration in this repo creates them.
const legacySchema = `
CREATE TABLE public.reels (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    url TEXT NOT NULL,
    normalized_url TEXT,
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
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE public.processing_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    url TEXT NOT NULL,
    normalized_url TEXT,
    status TEXT NOT NULL DEFAULT 'queued',
    failure_code TEXT,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    created_at TIMESTAMPTZ DEFAULT now(),
    completed_at TIMESTAMPTZ
);
CREATE TABLE public.processing_cache (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_platform TEXT NOT NULL,
    source_content_id TEXT NOT NULL,
    normalized_url TEXT NOT NULL,
    transcript TEXT DEFAULT '',
    caption TEXT DEFAULT '',
    thumbnail_url TEXT,
    extracted_data JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ DEFAULT now(),
    UNIQUE (source_platform, source_content_id)
);
`

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer admin.Close()

	name := "reelpin_backfill_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	if len(name) > 60 {
		name = name[:60]
	}
	for _, statement := range []string{
		`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`,
		`CREATE DATABASE ` + name,
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatalf("preparing %s: %v", name, err)
		}
	}

	parsed, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name

	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connecting to %s: %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanup, err := pgxpool.New(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})

	if _, err := pool.Exec(ctx, `
		CREATE SCHEMA auth;
		CREATE TABLE auth.users (id UUID PRIMARY KEY, email TEXT, created_at TIMESTAMPTZ DEFAULT now())`); err != nil {
		t.Fatalf("creating auth.users: %v", err)
	}
	if _, err := pool.Exec(ctx, legacySchema); err != nil {
		t.Fatalf("creating the legacy tables: %v", err)
	}
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO auth.users (id) VALUES ($1), ($2)`, userA, userB); err != nil {
		t.Fatalf("seeding users: %v", err)
	}
	return pool
}

// seed writes the shapes that matter: two users saving the same reel, a save
// the cache has better data for, a generic link, an unusable URL, and one job
// of each outcome.
func seed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO public.reels (id, user_id, url, title, summary, transcript, category, key_facts, locations)
		  VALUES ($1, $2, 'https://www.instagram.com/reel/SHARED1/?igsh=x', 'Shared reel', 'Summary A',
		          'transcript from user a', 'food', '["fact"]'::jsonb,
		          '[{"name":"Cafe","city":"Pune"}]'::jsonb)`, []any{sharedReelA, userA}},

		{`INSERT INTO public.reels (id, user_id, url, title, summary, transcript, category)
		  VALUES ($1, $2, 'https://instagram.com/reel/SHARED1/', 'Shared reel', 'Summary B',
		          'transcript from user b', 'travel')`, []any{sharedReelB, userB}},

		{`INSERT INTO public.reels (id, user_id, url, title, transcript)
		  VALUES ($1, $2, 'https://www.instagram.com/reel/CACHED1/', 'Cached reel', 'thin transcript')`,
			[]any{cachedReel, userA}},

		{`INSERT INTO public.reels (id, user_id, url, title)
		  VALUES ($1, $2, 'https://someblog.com/Article?utm_source=x', 'A link')`, []any{linkReel, userA}},

		{`INSERT INTO public.reels (id, user_id, url, title)
		  VALUES ($1, $2, 'not a url at all', 'Broken')`, []any{brokenReel, userA}},

		{`INSERT INTO public.processing_cache
		      (source_platform, source_content_id, normalized_url, transcript, caption, thumbnail_url, extracted_data)
		  VALUES ('instagram', 'CACHED1', 'https://www.instagram.com/reel/CACHED1/',
		          'full transcript from the cache', 'a caption', 'https://cdn.example.com/thumb.jpg',
		          '{"title":"Cached","key_facts":["cached fact"]}'::jsonb)`, nil},

		{`INSERT INTO public.processing_jobs (id, user_id, url, status, completed_at)
		  VALUES ($1, $2, 'https://www.instagram.com/reel/SHARED1/', 'completed', now())`,
			[]any{completedJob, userA}},

		{`INSERT INTO public.processing_jobs (id, user_id, url, status)
		  VALUES ($1, $2, 'https://www.instagram.com/reel/SHARED1/', 'queued')`, []any{queuedJob, userA}},

		{`INSERT INTO public.processing_jobs (id, user_id, url, status, failure_code, completed_at)
		  VALUES ($1, $2, 'nonsense', 'failed', 'internal_error', now())`, []any{brokenJob, userA}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
}

func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var total int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&total); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return total
}

func TestDryRunIsRepeatableAndWritesNothing(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	ctx := context.Background()

	first, err := New(pool, quiet()).Run(ctx, Options{})
	if err != nil {
		t.Fatalf("first dry run: %v", err)
	}
	second, err := New(pool, quiet()).Run(ctx, Options{})
	if err != nil {
		t.Fatalf("second dry run: %v", err)
	}
	if first != second {
		t.Fatalf("dry runs disagree:\nfirst  %+v\nsecond %+v", first, second)
	}

	if first.ReelsScanned != 5 {
		t.Errorf("reels_scanned = %d, want 5", first.ReelsScanned)
	}
	if first.InvalidURLs != 1 {
		t.Errorf("invalid_urls = %d, want 1", first.InvalidURLs)
	}
	if first.CacheHits != 1 {
		t.Errorf("cache_hits = %d, want 1", first.CacheHits)
	}
	// Two users share one reel, so three contents: the shared reel, the cached
	// reel, and the generic link.
	if first.UniqueContent != 3 {
		t.Errorf("unique_content = %d, want 3", first.UniqueContent)
	}

	for _, table := range []string{
		"reelpin.contents", "reelpin.content_versions", "reelpin.user_saves",
		"reelpin.processing_jobs", "reelpin.processing_runs",
		"reelpin.backfill_audit", "reelpin.backfill_progress",
	} {
		if total := count(t, pool, table); total != 0 {
			t.Errorf("%s has %d rows after a dry run, want 0", table, total)
		}
	}
}

func TestExecuteIsIdempotentAndDeduplicates(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	ctx := context.Background()

	legacyBefore := legacySnapshot(t, pool)

	first, err := New(pool, quiet()).Run(ctx, Options{Execute: true})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if first.SavesCreated != 4 {
		t.Errorf("saves_created = %d, want 4 (the fifth url is unusable)", first.SavesCreated)
	}
	if first.UniqueContent != 3 {
		t.Errorf("unique_content = %d, want 3", first.UniqueContent)
	}
	if first.CacheHits != 1 {
		t.Errorf("cache_hits = %d, want 1", first.CacheHits)
	}

	contents, versions := count(t, pool, "reelpin.contents"), count(t, pool, "reelpin.content_versions")
	if contents != 3 || versions != 3 {
		t.Fatalf("contents=%d versions=%d, want 3 and 3", contents, versions)
	}

	// The legacy reel id is the save id, so the app's existing reel ids still
	// resolve after the cutover.
	for _, id := range []string{sharedReelA, sharedReelB, cachedReel, linkReel} {
		var found bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM reelpin.user_saves WHERE id = $1)`, id).Scan(&found); err != nil {
			t.Fatalf("checking the save for %s: %v", id, err)
		}
		if !found {
			t.Errorf("legacy reel %s has no save under its own id", id)
		}
	}

	// Both saves of the same public reel point at one content.
	var shared int
	if err := pool.QueryRow(ctx, `
		SELECT count(DISTINCT content_id) FROM reelpin.user_saves WHERE id IN ($1, $2)`,
		sharedReelA, sharedReelB).Scan(&shared); err != nil {
		t.Fatalf("checking the shared content: %v", err)
	}
	if shared != 1 {
		t.Fatalf("two saves of one reel point at %d contents, want 1", shared)
	}

	// A generic link is fenced to the user who saved it, exactly as a fresh
	// submission of the same link would be.
	var scope string
	if err := pool.QueryRow(ctx, `
		SELECT c.access_scope_hash FROM reelpin.contents c
		JOIN reelpin.user_saves s ON s.content_id = c.id
		WHERE s.id = $1`, linkReel).Scan(&scope); err != nil {
		t.Fatalf("reading the link's scope: %v", err)
	}
	if scope == "public" {
		t.Error("a generic link was deduplicated globally")
	}

	// The cache supplied the transcript, caption and thumbnail the reel row
	// did not have.
	var transcript, caption, thumbnail string
	if err := pool.QueryRow(ctx, `
		SELECT v.transcript, v.caption, v.media->>'thumbnail_url'
		FROM reelpin.content_versions v
		JOIN reelpin.contents c ON c.current_version_id = v.id
		JOIN reelpin.user_saves s ON s.content_id = c.id
		WHERE s.id = $1`, cachedReel).Scan(&transcript, &caption, &thumbnail); err != nil {
		t.Fatalf("reading the cached version: %v", err)
	}
	if transcript != "full transcript from the cache" || caption != "a caption" {
		t.Errorf("the cache was not preferred: transcript=%q caption=%q", transcript, caption)
	}
	if thumbnail != "https://cdn.example.com/thumb.jpg" {
		t.Errorf("thumbnail = %q, want the cached one", thumbnail)
	}

	// The extraction lands where the pipeline puts it, so the same readers work.
	var category, fact string
	if err := pool.QueryRow(ctx, `
		SELECT v.raw_extraction->>'category', v.key_facts[1]
		FROM reelpin.content_versions v
		JOIN reelpin.contents c ON c.current_version_id = v.id
		JOIN reelpin.user_saves s ON s.content_id = c.id
		WHERE s.id = $1`, sharedReelA).Scan(&category, &fact); err != nil {
		t.Fatalf("reading the extraction: %v", err)
	}
	if category != "food" || fact != "fact" {
		t.Errorf("category=%q key_facts[1]=%q, want food and fact", category, fact)
	}

	if after := legacySnapshot(t, pool); after != legacyBefore {
		t.Errorf("the legacy tables changed:\nbefore %s\nafter  %s", legacyBefore, after)
	}

	// A finished run leaves its cursor at the end, so a rerun would scan
	// nothing and prove nothing. Clearing the cursor is what an operator does
	// to re-verify, and it is the real idempotency test.
	if _, err := pool.Exec(ctx, `DELETE FROM reelpin.backfill_progress`); err != nil {
		t.Fatalf("clearing the cursor: %v", err)
	}

	second, err := New(pool, quiet()).Run(ctx, Options{Execute: true})
	if err != nil {
		t.Fatalf("second execute: %v", err)
	}
	if second.ReelsScanned != 5 {
		t.Errorf("the rerun scanned %d rows, want all 5", second.ReelsScanned)
	}
	if second.SavesAlreadyThere != 4 {
		t.Errorf("saves_already_there = %d, want 4", second.SavesAlreadyThere)
	}
	if second.SavesCreated != 0 || second.UniqueContent != 0 || second.ContentVersions != 0 {
		t.Errorf("the second run created work: %+v", second)
	}
	if got := count(t, pool, "reelpin.contents"); got != contents {
		t.Errorf("the second run changed contents: %d -> %d", contents, got)
	}
	if got := count(t, pool, "reelpin.content_versions"); got != versions {
		t.Errorf("the second run changed versions: %d -> %d", versions, got)
	}
}

func TestJobsCopyOnlyWhenUnambiguous(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	ctx := context.Background()

	report, err := New(pool, quiet()).Run(ctx, Options{Execute: true})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if report.JobsScanned != 3 {
		t.Errorf("jobs_scanned = %d, want 3", report.JobsScanned)
	}
	if report.JobsCreated != 1 {
		t.Errorf("jobs_created = %d, want 1 (only the terminal, resolvable job)", report.JobsCreated)
	}
	if report.JobsUncertain != 2 {
		t.Errorf("jobs_uncertain = %d, want 2", report.JobsUncertain)
	}

	var status, stage, jobStatus string
	var saveID string
	if err := pool.QueryRow(ctx, `
		SELECT r.status, r.stage, j.status, j.user_save_id::text
		FROM reelpin.processing_jobs j
		JOIN reelpin.processing_runs r ON r.id = j.run_id
		WHERE j.id = $1`, completedJob).Scan(&status, &stage, &jobStatus, &saveID); err != nil {
		t.Fatalf("reading the copied job: %v", err)
	}
	if status != "completed" || stage != "persist" {
		t.Errorf("reconstructed run = %q/%q, want completed/persist", status, stage)
	}
	if jobStatus != "completed" {
		t.Errorf("job status = %q, want completed", jobStatus)
	}
	if saveID != sharedReelA {
		t.Errorf("the job points at save %s, want the user's own save %s", saveID, sharedReelA)
	}

	// A reconstructed run must never look like work waiting to be done.
	var active int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM reelpin.processing_runs
		WHERE status IN ('queued', 'processing', 'retry_scheduled')`).Scan(&active); err != nil {
		t.Fatalf("counting active runs: %v", err)
	}
	if active != 0 {
		t.Fatalf("%d reconstructed runs look active", active)
	}

	// The still-queued job stays behind and is reported, not guessed at.
	for _, id := range []string{queuedJob, brokenJob} {
		var found bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM reelpin.processing_jobs WHERE id = $1)`, id).Scan(&found); err != nil {
			t.Fatalf("checking job %s: %v", id, err)
		}
		if found {
			t.Errorf("job %s was copied even though it is ambiguous", id)
		}
	}

	var skipped int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM reelpin.backfill_audit WHERE action LIKE 'skipped%'`).Scan(&skipped); err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	if skipped != 3 {
		t.Errorf("the audit records %d skips, want 3 (one reel, two jobs)", skipped)
	}
}

func TestResumeContinuesFromTheCursor(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	ctx := context.Background()

	partial, err := New(pool, quiet()).Run(ctx, Options{Execute: true, BatchSize: 2, MaxRows: 2})
	if err != nil {
		t.Fatalf("partial run: %v", err)
	}
	if partial.ReelsScanned != 2 {
		t.Fatalf("reels_scanned = %d, want 2", partial.ReelsScanned)
	}
	if partial.JobsScanned != 0 {
		t.Errorf("jobs_scanned = %d, want 0: the reel pass never finished", partial.JobsScanned)
	}

	var cursor string
	if err := pool.QueryRow(ctx, `
		SELECT last_source_id::text FROM reelpin.backfill_progress
		WHERE backfill_version = $1 AND source_table = 'reels'`, Version).Scan(&cursor); err != nil {
		t.Fatalf("reading the cursor: %v", err)
	}
	if cursor != sharedReelB {
		t.Fatalf("cursor = %q, want the second reel", cursor)
	}

	rest, err := New(pool, quiet()).Run(ctx, Options{Execute: true, BatchSize: 2})
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if rest.ReelsScanned != 3 {
		t.Errorf("the resumed run scanned %d, want the remaining 3", rest.ReelsScanned)
	}
	if total := count(t, pool, "reelpin.user_saves"); total != 4 {
		t.Errorf("%d saves after resuming, want 4", total)
	}
}

func TestABlocklistedSourceIsSkippedNotFatal(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.source_blocklist
			(source_platform, source_content_type, source_content_id, reason, blocked_by)
		VALUES ('instagram', 'reel', 'SHARED1', 'privacy request', 'operator')`); err != nil {
		t.Fatalf("blocklisting the source: %v", err)
	}

	report, err := New(pool, quiet()).Run(ctx, Options{Execute: true})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if report.Blocklisted != 2 {
		t.Errorf("blocklisted = %d, want 2 (both saves of the blocked reel)", report.Blocklisted)
	}
	// The rest of the table still went through.
	if report.SavesCreated != 2 {
		t.Errorf("saves_created = %d, want the 2 rows that are not blocked", report.SavesCreated)
	}

	var action string
	if err := pool.QueryRow(ctx, `
		SELECT action FROM reelpin.backfill_audit WHERE source_id = $1`, sharedReelA).Scan(&action); err != nil {
		t.Fatalf("reading the audit row: %v", err)
	}
	if action != "skipped_blocklisted" {
		t.Errorf("action = %q, want skipped_blocklisted", action)
	}
}

func TestOnlyOneBackfillRunsAtATime(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring a connection: %v", err)
	}
	defer conn.Release()

	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, advisoryLockKey).Scan(&acquired); err != nil {
		t.Fatalf("taking the lock: %v", err)
	}
	if !acquired {
		t.Fatal("the lock was not free")
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)

	if _, err := New(pool, quiet()).Run(ctx, Options{Execute: true}); err == nil {
		t.Fatal("a second backfill ran while one held the lock")
	}
}

// legacySnapshot is every legacy column the backfill reads, so a test can prove
// the legacy tables are only ever a source.
func legacySnapshot(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var snapshot string
	if err := pool.QueryRow(context.Background(), `
		SELECT coalesce(string_agg(line, E'\n' ORDER BY line), '') FROM (
			SELECT r::text AS line FROM public.reels r
			UNION ALL
			SELECT j::text FROM public.processing_jobs j
			UNION ALL
			SELECT c::text FROM public.processing_cache c
		) rows`).Scan(&snapshot); err != nil {
		t.Fatalf("snapshotting the legacy tables: %v", err)
	}
	return snapshot
}
