//go:build integration

package backfill

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// legacySchema is the shape the Python service left behind, trimmed to what the
// backfill reads.
const legacySchema = `
CREATE TABLE public.reels (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
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
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE public.processing_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    environment TEXT NOT NULL DEFAULT 'test',
    url TEXT NOT NULL,
    normalized_url TEXT,
    status TEXT NOT NULL DEFAULT 'queued',
    failure_code TEXT,
    result_reel_id UUID,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    collection_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ DEFAULT now(),
    completed_at TIMESTAMPTZ
);
CREATE TABLE public.processing_cache (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_platform TEXT NOT NULL,
    source_content_id TEXT NOT NULL,
    source_content_type TEXT DEFAULT '',
    normalized_url TEXT NOT NULL,
    processing_version TEXT DEFAULT '',
    ingestion_method TEXT DEFAULT '',
    transcript_source TEXT DEFAULT '',
    transcript TEXT DEFAULT '',
    caption TEXT DEFAULT '',
    thumbnail_url TEXT,
    extracted_data JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ DEFAULT now(),
    updated_at TIMESTAMPTZ DEFAULT now(),
    UNIQUE (source_platform, source_content_id)
);
`

func testDatabase(t *testing.T) *pgxpool.Pool {
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
		t.Fatalf("parsing url: %v", err)
	}
	parsed.Path = "/" + name
	databaseURL := parsed.String()

	pool, err := pgxpool.New(ctx, databaseURL)
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

	if _, err := pool.Exec(ctx, legacySchema); err != nil {
		t.Fatalf("creating legacy tables: %v", err)
	}
	if _, err := migrations.Up(ctx, databaseURL); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return pool
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// seed writes the shapes that matter: two users saving the same reel, a save
// with a cache entry, a generic link, an unusable URL, and a terminal job.
func seed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	statements := []string{
		`INSERT INTO public.reels (id, user_id, url, source_platform, source_content_type, source_content_id, title, summary, transcript, category, key_facts, locations)
		 VALUES ('aaaaaaaa-0000-4000-8000-000000000001', 'user-a', 'https://www.instagram.com/reel/SHARED1/?igsh=x', 'instagram', 'reel', 'SHARED1',
		         'Shared reel', 'Summary A', 'transcript from user a', 'food', '["fact"]'::jsonb, '[{"name":"Cafe","latitude":1.0,"longitude":2.0}]'::jsonb)`,

		`INSERT INTO public.reels (id, user_id, url, source_platform, source_content_type, source_content_id, title, summary, transcript, category)
		 VALUES ('aaaaaaaa-0000-4000-8000-000000000002', 'user-b', 'https://instagram.com/reel/SHARED1/', 'instagram', 'reel', 'SHARED1',
		         'Shared reel', 'Summary B', 'transcript from user b', 'travel')`,

		`INSERT INTO public.reels (id, user_id, url, source_platform, source_content_type, source_content_id, title, transcript)
		 VALUES ('aaaaaaaa-0000-4000-8000-000000000003', 'user-a', 'https://www.instagram.com/reel/CACHED1/', 'instagram', 'reel', 'CACHED1',
		         'Cached reel', 'thin transcript')`,

		`INSERT INTO public.reels (id, user_id, url, title)
		 VALUES ('aaaaaaaa-0000-4000-8000-000000000004', 'user-a', 'https://someblog.com/Article?utm_source=x', 'A link')`,

		`INSERT INTO public.reels (id, user_id, url, title)
		 VALUES ('aaaaaaaa-0000-4000-8000-000000000005', 'user-a', 'not a url at all', 'Broken')`,

		`INSERT INTO public.processing_cache (source_platform, source_content_id, source_content_type, normalized_url, transcript, caption, thumbnail_url, extracted_data)
		 VALUES ('instagram', 'CACHED1', 'reel', 'https://www.instagram.com/reel/CACHED1/', 'full transcript from the cache', 'a caption',
		         'https://cdn.example.com/thumb.jpg', '{"title":"Cached","key_facts":["cached fact"]}'::jsonb)`,

		`INSERT INTO public.processing_jobs (id, user_id, url, status, result_reel_id, completed_at)
		 VALUES ('cccccccc-0000-4000-8000-000000000001', 'user-a', 'https://www.instagram.com/reel/SHARED1/', 'completed',
		         'aaaaaaaa-0000-4000-8000-000000000001', now())`,

		`INSERT INTO public.processing_jobs (id, user_id, url, status)
		 VALUES ('cccccccc-0000-4000-8000-000000000002', 'user-a', 'https://www.instagram.com/reel/SHARED1/', 'queued')`,

		`INSERT INTO public.processing_jobs (id, user_id, url, status, failure_code, completed_at)
		 VALUES ('cccccccc-0000-4000-8000-000000000003', 'user-a', 'nonsense', 'failed', 'internal_error', now())`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
}

func TestDryRunIsRepeatableAndWritesNothing(t *testing.T) {
	pool := testDatabase(t)
	seed(t, pool)
	ctx := context.Background()

	first, err := New(pool, quietLogger()).Run(ctx, Options{})
	if err != nil {
		t.Fatalf("first dry run: %v", err)
	}
	second, err := New(pool, quietLogger()).Run(ctx, Options{})
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

	for _, table := range []string{"reelpin.contents", "reelpin.content_versions", "reelpin.backfill_audit"} {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("counting %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s has %d rows after a dry run, want 0", table, count)
		}
	}

	var linked int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM public.reels WHERE content_version_id IS NOT NULL`,
	).Scan(&linked); err != nil {
		t.Fatalf("counting links: %v", err)
	}
	if linked != 0 {
		t.Errorf("%d reels were linked by a dry run", linked)
	}
}

func TestExecuteIsIdempotentAndDeduplicates(t *testing.T) {
	pool := testDatabase(t)
	seed(t, pool)
	ctx := context.Background()

	before := reelSnapshot(t, pool)

	first, err := New(pool, quietLogger()).Run(ctx, Options{Execute: true})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if first.ReelsLinked != 4 {
		t.Errorf("reels_linked = %d, want 4 (the fifth url is unusable)", first.ReelsLinked)
	}
	// Two users saved the same reel, so three contents: the shared reel, the
	// cached reel, and the generic link.
	if first.UniqueContent != 3 {
		t.Errorf("unique_content = %d, want 3", first.UniqueContent)
	}
	if first.CacheHits != 1 {
		t.Errorf("cache_hits = %d, want 1", first.CacheHits)
	}

	var contents, versions int
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.contents`).Scan(&contents)
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.content_versions`).Scan(&versions)
	if contents != 3 || versions != 3 {
		t.Fatalf("contents=%d versions=%d, want 3 and 3", contents, versions)
	}

	// Both saves of the same reel point at one content version.
	var shared int
	if err := pool.QueryRow(ctx, `
		SELECT count(DISTINCT content_version_id) FROM public.reels
		WHERE id IN ('aaaaaaaa-0000-4000-8000-000000000001','aaaaaaaa-0000-4000-8000-000000000002')`,
	).Scan(&shared); err != nil {
		t.Fatalf("checking the shared link: %v", err)
	}
	if shared != 1 {
		t.Fatalf("two saves of one reel point at %d content versions, want 1", shared)
	}

	// The cache supplied the transcript the reel row did not have.
	var transcript, caption string
	if err := pool.QueryRow(ctx, `
		SELECT v.transcript, v.caption FROM reelpin.content_versions v
		JOIN public.reels r ON r.content_version_id = v.id
		WHERE r.id = 'aaaaaaaa-0000-4000-8000-000000000003'`,
	).Scan(&transcript, &caption); err != nil {
		t.Fatalf("reading the cached version: %v", err)
	}
	if transcript != "full transcript from the cache" || caption != "a caption" {
		t.Errorf("cache was not preferred: transcript=%q caption=%q", transcript, caption)
	}

	// Nothing a user sees changed.
	if after := reelSnapshot(t, pool); after != before {
		t.Errorf("user-visible reel columns changed:\nbefore %s\nafter  %s", before, after)
	}

	// A finished run leaves its cursor at the end, so a rerun would scan
	// nothing and prove nothing. Clearing the cursor is what an operator does
	// to re-verify, and it is the real idempotency test.
	if _, err := pool.Exec(ctx, `DELETE FROM reelpin.backfill_progress`); err != nil {
		t.Fatalf("clearing the cursor: %v", err)
	}

	second, err := New(pool, quietLogger()).Run(ctx, Options{Execute: true})
	if err != nil {
		t.Fatalf("second execute: %v", err)
	}
	if second.ReelsScanned != 5 {
		t.Errorf("the rerun scanned %d rows, want all 5", second.ReelsScanned)
	}
	if second.ReelsLinked != 0 || second.UniqueContent != 0 || second.ContentVersions != 0 {
		t.Errorf("the second run created work: %+v", second)
	}
	if second.ReelsAlreadyLinked != 4 {
		t.Errorf("reels_already_linked = %d, want 4", second.ReelsAlreadyLinked)
	}

	var contentsAfter, versionsAfter int
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.contents`).Scan(&contentsAfter)
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.content_versions`).Scan(&versionsAfter)
	if contentsAfter != contents || versionsAfter != versions {
		t.Errorf("the second run changed row counts: contents %d->%d versions %d->%d",
			contents, contentsAfter, versions, versionsAfter)
	}
}

func TestJobsLinkOnlyWhenUnambiguous(t *testing.T) {
	pool := testDatabase(t)
	seed(t, pool)
	ctx := context.Background()

	report, err := New(pool, quietLogger()).Run(ctx, Options{Execute: true})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if report.JobsScanned != 3 {
		t.Errorf("jobs_scanned = %d, want 3", report.JobsScanned)
	}
	if report.JobsLinked != 1 {
		t.Errorf("jobs_linked = %d, want 1 (only the terminal, resolvable job)", report.JobsLinked)
	}
	if report.JobsUncertain != 2 {
		t.Errorf("jobs_uncertain = %d, want 2", report.JobsUncertain)
	}

	var status, stage string
	if err := pool.QueryRow(ctx, `
		SELECT r.status, r.stage FROM reelpin.processing_runs r
		JOIN public.processing_jobs j ON j.processing_run_id = r.id
		WHERE j.id = 'cccccccc-0000-4000-8000-000000000001'`,
	).Scan(&status, &stage); err != nil {
		t.Fatalf("reading the reconstructed run: %v", err)
	}
	if status != "completed" {
		t.Errorf("reconstructed run status = %q, want completed", status)
	}

	// A reconstructed run must never look like work waiting to be done.
	var active int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM reelpin.processing_runs WHERE status IN ('queued','processing','retry_scheduled')`,
	).Scan(&active); err != nil {
		t.Fatalf("counting active runs: %v", err)
	}
	if active != 0 {
		t.Fatalf("%d reconstructed runs look active", active)
	}

	// The still-queued job stays unlinked and is reported, not guessed at.
	var linked *string
	if err := pool.QueryRow(ctx,
		`SELECT processing_run_id::text FROM public.processing_jobs WHERE id = 'cccccccc-0000-4000-8000-000000000002'`,
	).Scan(&linked); err != nil {
		t.Fatalf("reading the active job: %v", err)
	}
	if linked != nil {
		t.Error("an active job was linked to a reconstructed run")
	}

	var skipped int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM reelpin.backfill_audit WHERE action LIKE 'skipped%'`,
	).Scan(&skipped); err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	if skipped != 3 {
		t.Errorf("audit records %d skips, want 3 (one reel, two jobs)", skipped)
	}
}

func TestResumeContinuesFromTheCursor(t *testing.T) {
	pool := testDatabase(t)
	seed(t, pool)
	ctx := context.Background()

	partial, err := New(pool, quietLogger()).Run(ctx, Options{Execute: true, BatchSize: 2, MaxRows: 2})
	if err != nil {
		t.Fatalf("partial run: %v", err)
	}
	if partial.ReelsScanned != 2 {
		t.Fatalf("reels_scanned = %d, want 2", partial.ReelsScanned)
	}

	var cursor string
	if err := pool.QueryRow(ctx,
		`SELECT last_source_id::text FROM reelpin.backfill_progress
		 WHERE backfill_version = $1 AND source_table = 'reels'`, Version,
	).Scan(&cursor); err != nil {
		t.Fatalf("reading the cursor: %v", err)
	}
	if cursor != "aaaaaaaa-0000-4000-8000-000000000002" {
		t.Fatalf("cursor = %q, want the second reel", cursor)
	}

	rest, err := New(pool, quietLogger()).Run(ctx, Options{Execute: true, BatchSize: 2})
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if rest.ReelsScanned != 3 {
		t.Errorf("resumed run scanned %d, want the remaining 3", rest.ReelsScanned)
	}

	var linked int
	pool.QueryRow(ctx, `SELECT count(*) FROM public.reels WHERE content_version_id IS NOT NULL`).Scan(&linked)
	if linked != 4 {
		t.Errorf("%d reels linked after resuming, want 4", linked)
	}
}

func TestOnlyOneBackfillRunsAtATime(t *testing.T) {
	pool := testDatabase(t)
	seed(t, pool)
	ctx := context.Background()

	held, release, err := lockForTest(ctx, pool)
	if err != nil {
		t.Fatalf("taking the lock: %v", err)
	}
	if !held {
		t.Fatal("the lock was not free")
	}
	defer release()

	if _, err := New(pool, quietLogger()).Run(ctx, Options{Execute: true}); err == nil {
		t.Fatal("a second backfill ran while one held the lock")
	}
}

func lockForTest(ctx context.Context, pool *pgxpool.Pool) (bool, func(), error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, nil, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, advisoryLockKey).Scan(&acquired); err != nil {
		conn.Release()
		return false, nil, err
	}
	return acquired, func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
		conn.Release()
	}, nil
}

// reelSnapshot is every column a user can see, so the test can prove the
// backfill only ever wrote the link column.
func reelSnapshot(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT id, user_id, url, title, summary, transcript, category, subcategory,
		       key_facts, locations, created_at
		FROM public.reels ORDER BY id`)
	if err != nil {
		t.Fatalf("snapshotting reels: %v", err)
	}
	defer rows.Close()

	var snapshot strings.Builder
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("snapshotting reels: %v", err)
		}
		for _, value := range values {
			snapshot.WriteString(strings.TrimSpace(sprint(value)))
			snapshot.WriteByte('|')
		}
		snapshot.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshotting reels: %v", err)
	}
	return snapshot.String()
}

func sprint(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	case []byte:
		return string(typed)
	default:
		return strings.Join(strings.Fields(fmt.Sprintf("%v", typed)), " ")
	}
}
