//go:build integration

package migrations

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testDatabase gives each test its own database so migrations run against an
// empty server, exactly as a fresh deployment would.
func testDatabase(t *testing.T) (*pgxpool.Pool, string) {
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

	name := "reelpin_migrate_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	if len(name) > 60 {
		name = name[:60]
	}
	for _, statement := range []string{
		`DROP DATABASE IF EXISTS ` + name,
		`CREATE DATABASE ` + name,
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatalf("preparing %s: %v", name, err)
		}
	}

	config, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatalf("parsing url: %v", err)
	}
	config.ConnConfig.Database = name

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connecting to %s: %v", name, err)
	}
	databaseURL := withDatabase(t, adminURL, name)
	t.Cleanup(func() {
		pool.Close()

		cleanup, err := pgxpool.New(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	return pool, databaseURL
}

// withDatabase points a connection string at another database on the same server.
func withDatabase(t *testing.T, rawURL, database string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing TEST_DATABASE_URL: %v", err)
	}
	parsed.Path = "/" + database
	return parsed.String()
}

func TestMigrationsApplyToAnEmptyDatabase(t *testing.T) {
	pool, databaseURL := testDatabase(t)
	ctx := context.Background()

	if _, err := Up(ctx, databaseURL); err != nil {
		t.Fatalf("Up: %v", err)
	}

	wantTables := []string{
		"contents", "content_versions", "content_locations", "content_chunks",
		"processing_runs", "processing_stage_results", "outbox_events", "provider_cooldowns",
	}
	for _, table := range wantTables {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname='reelpin' AND tablename=$1)`,
			table,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("checking %s: %v", table, err)
		}
		if !exists {
			t.Errorf("reelpin.%s was not created", table)
		}
	}

	var postgis bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname='postgis')`,
	).Scan(&postgis); err != nil {
		t.Fatalf("checking postgis: %v", err)
	}
	if !postgis {
		t.Error("postgis was not enabled")
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	pool, databaseURL := testDatabase(t)
	ctx := context.Background()

	if _, err := Up(ctx, databaseURL); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	if _, err := Up(ctx, databaseURL); err != nil {
		t.Fatalf("second Up: %v", err)
	}

	// Re-running the SQL itself must also be safe, not just goose's version
	// bookkeeping.
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, migration := range loaded {
		if _, err := pool.Exec(ctx, migration.Up); err != nil {
			t.Fatalf("reapplying %s: %v", migration.Name, err)
		}
	}
}

func TestGlobalConstraints(t *testing.T) {
	pool, databaseURL := testDatabase(t)
	ctx := context.Background()
	if _, err := Up(ctx, databaseURL); err != nil {
		t.Fatalf("Up: %v", err)
	}

	insertContent := func(platform, contentType, contentID, urlHash, scope string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO reelpin.contents
			 (source_platform, source_content_type, source_content_id, normalized_url,
			  normalized_url_hash, access_scope_hash)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			platform, contentType, nullable(contentID), "https://example.com/"+urlHash, urlHash, scope,
		)
		return err
	}

	t.Run("a public identity is unique per access scope", func(t *testing.T) {
		if err := insertContent("instagram", "reel", "C8abc", "hash-1", "public"); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		if err := insertContent("instagram", "reel", "C8abc", "hash-2", "public"); err == nil {
			t.Fatal("the same public identity was inserted twice")
		}
		// A different access scope is different content.
		if err := insertContent("instagram", "reel", "C8abc", "hash-3", "cookie-slot-a"); err != nil {
			t.Fatalf("scoped insert: %v", err)
		}
	})

	t.Run("a generic link is unique by normalized url hash", func(t *testing.T) {
		if err := insertContent("someblog.com", "link", "", "generic-1", "public"); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		if err := insertContent("someblog.com", "link", "", "generic-1", "public"); err == nil {
			t.Fatal("the same generic link was inserted twice")
		}
	})

	t.Run("one live run per content and processor version", func(t *testing.T) {
		var contentID string
		if err := pool.QueryRow(ctx,
			`SELECT id FROM reelpin.contents WHERE normalized_url_hash = 'hash-1'`,
		).Scan(&contentID); err != nil {
			t.Fatalf("reading content: %v", err)
		}

		insertRun := func(status string) error {
			_, err := pool.Exec(ctx,
				`INSERT INTO reelpin.processing_runs (content_id, processor_version, platform, status)
				 VALUES ($1,'v1','instagram',$2)`,
				contentID, status,
			)
			return err
		}

		if err := insertRun("queued"); err != nil {
			t.Fatalf("first run: %v", err)
		}
		for _, status := range []string{"queued", "processing", "retry_scheduled"} {
			if err := insertRun(status); err == nil {
				t.Fatalf("a second %s run was allowed", status)
			}
		}
		// A finished run does not block the next one.
		if _, err := pool.Exec(ctx,
			`UPDATE reelpin.processing_runs SET status='completed' WHERE content_id=$1`, contentID,
		); err != nil {
			t.Fatalf("completing the run: %v", err)
		}
		if err := insertRun("queued"); err != nil {
			t.Fatalf("run after completion: %v", err)
		}

		if err := insertRun("nonsense"); err == nil {
			t.Fatal("an unknown status was accepted")
		}
	})

	t.Run("outbox event ids are unique", func(t *testing.T) {
		const id = "11111111-1111-4111-8111-111111111111"
		insert := func() error {
			_, err := pool.Exec(ctx,
				`INSERT INTO reelpin.outbox_events (event_id, event_type, routing_key, payload)
				 VALUES ($1,'content.ready','reelpin.jobs.instagram','{}'::jsonb)`, id)
			return err
		}
		if err := insert(); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		if err := insert(); err == nil {
			t.Fatal("the same event id was inserted twice")
		}
	})
}

func TestConcurrentInsertsPickOneWinner(t *testing.T) {
	pool, databaseURL := testDatabase(t)
	ctx := context.Background()
	if _, err := Up(ctx, databaseURL); err != nil {
		t.Fatalf("Up: %v", err)
	}

	const workers = 8
	results := make(chan error, workers)
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		go func() {
			<-start
			_, err := pool.Exec(ctx,
				`INSERT INTO reelpin.contents
				 (source_platform, source_content_type, source_content_id, normalized_url, normalized_url_hash)
				 VALUES ('instagram','reel','RACE','https://example.com/race','race-hash')`)
			results <- err
		}()
	}
	close(start)

	succeeded := 0
	for i := 0; i < workers; i++ {
		if err := <-results; err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d concurrent inserts succeeded, want exactly 1", succeeded)
	}
}

func TestExistingTablesStillAcceptOldInserts(t *testing.T) {
	pool, databaseURL := testDatabase(t)
	ctx := context.Background()

	// A production-shaped database already has these tables when the migration
	// runs, so create them first and then migrate.
	if _, err := pool.Exec(ctx, legacyTables); err != nil {
		t.Fatalf("creating legacy tables: %v", err)
	}
	if _, err := Up(ctx, databaseURL); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO public.reels (id, user_id, url, title) VALUES (gen_random_uuid(),'user-1','https://example.com/a','A reel')`,
	); err != nil {
		t.Fatalf("legacy reel insert: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.processing_jobs (id, user_id, url) VALUES (gen_random_uuid(),'user-1','https://example.com/a')`,
	); err != nil {
		t.Fatalf("legacy job insert: %v", err)
	}

	var contentVersionID, runID *string
	if err := pool.QueryRow(ctx, `SELECT content_version_id FROM public.reels`).Scan(&contentVersionID); err != nil {
		t.Fatalf("reading the new column: %v", err)
	}
	if contentVersionID != nil {
		t.Error("content_version_id should stay null until the backfill links it")
	}
	if err := pool.QueryRow(ctx, `SELECT processing_run_id FROM public.processing_jobs`).Scan(&runID); err != nil {
		t.Fatalf("reading the new column: %v", err)
	}
	if runID != nil {
		t.Error("processing_run_id should stay null until the backfill links it")
	}

	// Deleting shared global content must not take a user's save with it.
	var versionID string
	if err := pool.QueryRow(ctx, `
		WITH content AS (
			INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id, normalized_url, normalized_url_hash)
			VALUES ('instagram','reel','LINKED','https://example.com/linked','linked-hash')
			RETURNING id
		)
		INSERT INTO reelpin.content_versions (content_id, processor_version, extraction_schema_version)
		SELECT id, 'v1', 'v1' FROM content
		RETURNING id`).Scan(&versionID); err != nil {
		t.Fatalf("creating content: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE public.reels SET content_version_id = $1`, versionID); err != nil {
		t.Fatalf("linking the reel: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM reelpin.content_versions WHERE id = $1`, versionID); err != nil {
		t.Fatalf("deleting the content version: %v", err)
	}

	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.reels`).Scan(&remaining); err != nil {
		t.Fatalf("counting reels: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("reels = %d, want the save to survive its content version", remaining)
	}
}

func TestHotPathsUseIndexes(t *testing.T) {
	pool, databaseURL := testDatabase(t)
	ctx := context.Background()
	if _, err := Up(ctx, databaseURL); err != nil {
		t.Fatalf("Up: %v", err)
	}

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			name: "source lookup",
			query: `EXPLAIN SELECT id FROM reelpin.contents
			        WHERE source_platform='instagram' AND source_content_type='reel'
			          AND source_content_id='C8abc' AND access_scope_hash='public'`,
			want: "contents_public_identity_key",
		},
		{
			name: "available outbox rows",
			query: `EXPLAIN SELECT event_id FROM reelpin.outbox_events
			        WHERE published_at IS NULL AND available_at <= now()
			        ORDER BY available_at, event_id LIMIT 100`,
			want: "outbox_events_pending_idx",
		},
		{
			name: "active run for a content",
			query: `EXPLAIN SELECT id FROM reelpin.processing_runs
			        WHERE content_id='11111111-1111-4111-8111-111111111111' AND processor_version='v1'
			          AND status IN ('queued','processing','retry_scheduled')`,
			want: "processing_runs_active_key",
		},
		{
			name: "expired leases",
			query: `EXPLAIN SELECT id FROM reelpin.processing_runs
			        WHERE status='processing' AND lease_expires_at < now()`,
			want: "processing_runs_lease_idx",
		},
	}

	// The planner only prefers an index once it believes there are rows.
	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.contents (source_platform, source_content_type, source_content_id, normalized_url, normalized_url_hash)
		SELECT 'instagram','reel','id-'||generation, 'https://example.com/'||generation, 'hash-'||generation
		FROM generate_series(1, 2000) AS generation`); err != nil {
		t.Fatalf("seeding contents: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.outbox_events (event_id, event_type, routing_key, payload, available_at)
		SELECT gen_random_uuid(), 'content.ready', 'reelpin.jobs.instagram', '{}'::jsonb, now() - (generation || ' seconds')::interval
		FROM generate_series(1, 2000) AS generation`); err != nil {
		t.Fatalf("seeding outbox: %v", err)
	}
	// Most runs are finished; only a handful hold an expired lease, which is
	// what makes the partial index worth using.
	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.processing_runs (content_id, processor_version, platform, status, lease_expires_at)
		SELECT id, 'v1', 'instagram', 'completed', NULL FROM reelpin.contents`); err != nil {
		t.Fatalf("seeding finished runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE reelpin.processing_runs SET status='processing', lease_expires_at = now() - interval '1 minute'
		WHERE id IN (SELECT id FROM reelpin.processing_runs LIMIT 5)`); err != nil {
		t.Fatalf("expiring a few leases: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE reelpin.contents, reelpin.outbox_events, reelpin.processing_runs`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := pool.Query(ctx, tt.query)
			if err != nil {
				t.Fatalf("explain: %v", err)
			}
			defer rows.Close()

			var plan strings.Builder
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatalf("scanning plan: %v", err)
				}
				plan.WriteString(line)
				plan.WriteString("\n")
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("reading plan: %v", err)
			}
			if !strings.Contains(plan.String(), tt.want) {
				t.Errorf("plan does not use %s:\n%s", tt.want, plan.String())
			}
		})
	}
}

func TestMigrationStatusAndDown(t *testing.T) {
	pool, databaseURL := testDatabase(t)
	ctx := context.Background()
	if _, err := Up(ctx, databaseURL); err != nil {
		t.Fatalf("Up: %v", err)
	}

	lines, err := Status(ctx, databaseURL)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if lines[0] != "applied 00001_global_content.sql" {
		t.Fatalf("status = %v, want the migration reported as applied", lines)
	}
	if len(lines) == 0 {
		t.Fatal("status returned nothing")
	}

	// Down takes one migration at a time, newest first, so the loop also proves
	// every down section works.
	rolledBack := 0
	for {
		name, err := Down(ctx, databaseURL)
		if err != nil {
			t.Fatalf("Down: %v", err)
		}
		if name == "" {
			break
		}
		rolledBack++
	}
	if rolledBack != len(lines) {
		t.Fatalf("rolled back %d migrations, want %d", rolledBack, len(lines))
	}
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname='reelpin')`,
	).Scan(&exists); err != nil {
		t.Fatalf("checking schema: %v", err)
	}
	if exists {
		t.Error("the schema survived a rollback on a disposable database")
	}
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// legacyTables is the slice of the current production schema these migrations
// have to expand without breaking.
const legacyTables = `
CREATE TABLE public.reels (
    id UUID PRIMARY KEY,
    user_id TEXT NOT NULL,
    url TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT 'Untitled',
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE public.processing_jobs (
    id UUID PRIMARY KEY,
    user_id TEXT NOT NULL,
    url TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued',
    collection_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ DEFAULT now()
);
`

// The reelpin schema shares its name with the database role, so an unqualified
// version table would land in the "$user" schema on any connection made after
// the migration ran, and every later connection would see an empty one.
func TestVersionTableSurvivesTheUserSchema(t *testing.T) {
	pool, databaseURL := testDatabase(t)
	ctx := context.Background()

	if _, err := Up(ctx, databaseURL); err != nil {
		t.Fatalf("Up: %v", err)
	}

	var schemas int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_tables WHERE tablename = 'schema_migrations'`,
	).Scan(&schemas); err != nil {
		t.Fatalf("counting version tables: %v", err)
	}
	if schemas != 1 {
		t.Fatalf("%d version tables exist, want exactly one", schemas)
	}

	// A second run must be a no-op, not a replay.
	applied, err := Up(ctx, databaseURL)
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("second Up applied %v, want nothing", applied)
	}

	status, err := Status(ctx, databaseURL)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, line := range status {
		if strings.HasPrefix(line, "pending") {
			t.Errorf("status reports %q after a successful migration", line)
		}
	}
}
