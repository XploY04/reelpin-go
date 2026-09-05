//go:build integration

package migrations

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDatabase gives each test its own database so migrations run against an
// empty server, exactly as a fresh deployment would. Cleanup drops it WITH
// (FORCE), so a connection left open by a failure cannot hang the package.
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
		`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`,
		`CREATE DATABASE ` + name,
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatalf("preparing %s: %v", name, err)
		}
	}

	databaseURL := withDatabase(t, adminURL, name)
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

	// Supabase owns auth.users in every real deployment, so migrations assume
	// it. The integration shape mirrors production's identity table.
	if _, err := pool.Exec(ctx, `
		CREATE SCHEMA auth;
		CREATE TABLE auth.users (
			id         UUID PRIMARY KEY,
			email      TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatalf("creating the production-shaped auth schema: %v", err)
	}

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

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

func migrate(t *testing.T, databaseURL string) {
	t.Helper()
	if _, err := Up(context.Background(), databaseURL); err != nil {
		t.Fatalf("Up: %v", err)
	}
}

func seedUsers(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO auth.users (id) VALUES ($1), ($2)`, userA, userB); err != nil {
		t.Fatalf("seeding users: %v", err)
	}
}

// seedContent creates one content with one version and returns both ids.
func seedContent(t *testing.T, pool *pgxpool.Pool, sourceID string) (string, string) {
	t.Helper()
	ctx := context.Background()

	var contentID, versionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, normalized_url, normalized_url_hash,
			 source_content_id, access_scope_hash)
		VALUES ('instagram', 'reel', 'https://www.instagram.com/reel/'||$1||'/', $1, $1, 'public')
		RETURNING id::text`, sourceID,
	).Scan(&contentID); err != nil {
		t.Fatalf("seeding content: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, prompt_version, schema_version, model_version, title)
		VALUES ($1, 'v1', 'p1', 's1', 'm1', 'A title')
		RETURNING id::text`, contentID,
	).Scan(&versionID); err != nil {
		t.Fatalf("seeding a version: %v", err)
	}
	return contentID, versionID
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestMigrationsApplyToAnEmptyDatabaseAndRepeatCleanly(t *testing.T) {
	pool, databaseURL := testDatabase(t)
	ctx := context.Background()

	applied, err := Up(ctx, databaseURL)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(applied) != 3 {
		t.Fatalf("applied %d migrations, want 3: %v", len(applied), applied)
	}

	wantTables := []string{
		"contents", "content_versions", "user_saves",
		"processing_runs", "processing_jobs", "processing_stage_results",
		"outbox_events", "idempotency_keys", "account_deletion_requests",
		"categories", "category_aliases", "category_proposals", "taxonomy_runs",
	}
	for _, table := range wantTables {
		var exists bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM information_schema.tables
			               WHERE table_schema = 'reelpin' AND table_name = $1)`, table,
		).Scan(&exists); err != nil {
			t.Fatalf("checking %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table reelpin.%s is missing", table)
		}
	}

	// Running again changes nothing.
	again, err := Up(ctx, databaseURL)
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second run applied %v, want nothing", again)
	}

	for _, line := range mustStatus(t, databaseURL) {
		if !strings.HasPrefix(line, "applied ") {
			t.Errorf("status line %q, want applied", line)
		}
	}
}

func mustStatus(t *testing.T, databaseURL string) []string {
	t.Helper()
	lines, err := Status(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return lines
}

func TestSchemaHasNoEnvironmentColumn(t *testing.T) {
	pool, databaseURL := testDatabase(t)
	migrate(t, databaseURL)

	var count int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'reelpin' AND column_name = 'environment'`,
	).Scan(&count); err != nil {
		t.Fatalf("querying columns: %v", err)
	}
	if count != 0 {
		t.Fatalf("%d environment columns exist; separate infrastructure replaced them", count)
	}
}

func TestConstraintsRejectWhatTheyMust(t *testing.T) {
	pool, databaseURL := testDatabase(t)
	migrate(t, databaseURL)
	seedUsers(t, pool)
	ctx := context.Background()

	contentID, _ := seedContent(t, pool, "SHARED1")

	if _, err := pool.Exec(ctx,
		`INSERT INTO reelpin.user_saves (user_id, content_id) VALUES ($1, $2)`, userA, contentID); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO reelpin.user_saves (user_id, content_id) VALUES ($1, $2)`, userA, contentID); sqlState(err) != "23505" {
		t.Errorf("duplicate save err = %v, want unique violation", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO reelpin.processing_runs (content_id, processor_version) VALUES ($1, 'v1')`, contentID); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO reelpin.processing_runs (content_id, processor_version) VALUES ($1, 'v1')`, contentID); sqlState(err) != "23505" {
		t.Errorf("duplicate active run err = %v, want unique violation", err)
	}
	// A completed run does not block a new one.
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.processing_runs SET status = 'completed' WHERE content_id = $1`, contentID); err != nil {
		t.Fatalf("completing the run: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO reelpin.processing_runs (content_id, processor_version) VALUES ($1, 'v1')`, contentID); err != nil {
		t.Errorf("a new run after completion should be allowed: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.processing_jobs (user_id, url, normalized_url, status)
		VALUES ($1, 'https://x', 'https://x', 'sideways')`, userA); sqlState(err) != "23514" {
		t.Errorf("invalid job status err = %v, want check violation", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.processing_jobs (user_id, url, normalized_url)
		VALUES ('99999999-9999-4999-8999-999999999999', 'https://x', 'https://x')`); sqlState(err) != "23503" {
		t.Errorf("unknown user err = %v, want foreign key violation", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, normalized_url, normalized_url_hash, access_scope_hash)
		VALUES ('instagram', 'hologram', 'https://x', 'h1', 'public')`); sqlState(err) != "23514" {
		t.Errorf("invalid content type err = %v, want check violation", err)
	}
}

func TestCurrentVersionMustBelongToTheContent(t *testing.T) {
	pool, databaseURL := testDatabase(t)
	migrate(t, databaseURL)
	ctx := context.Background()

	contentA, versionA := seedContent(t, pool, "AAA")
	_, versionB := seedContent(t, pool, "BBB")

	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.contents SET current_version_id = $1 WHERE id = $2`, versionB, contentA); sqlState(err) != "23503" {
		t.Errorf("pointing at another content's version err = %v, want foreign key violation", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.contents SET current_version_id = $1 WHERE id = $2`, versionA, contentA); err != nil {
		t.Errorf("pointing at its own version failed: %v", err)
	}
}

func TestContentVersionsAreImmutable(t *testing.T) {
	pool, databaseURL := testDatabase(t)
	migrate(t, databaseURL)
	ctx := context.Background()

	_, versionID := seedContent(t, pool, "IMMUT1")

	// Even the table owner cannot edit one: the trigger is the backstop for a
	// session that outranks the grants.
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.content_versions SET title = 'edited' WHERE id = $1`, versionID); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Errorf("owner update err = %v, want the immutability trigger", err)
	}
}

// asRole runs one function on a connection switched to another role, and
// releases the connection before any assertion can fail the test.
func asRole(t *testing.T, pool *pgxpool.Pool, role string, run func(conn *pgxpool.Conn) error) error {
	t.Helper()
	ctx := context.Background()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring a connection: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SET ROLE "+role); err != nil {
		t.Fatalf("SET ROLE %s: %v", role, err)
	}
	defer conn.Exec(ctx, "RESET ROLE")

	return run(conn)
}

func TestApplicationRoleCannotTouchVersionsOrOtherSchemas(t *testing.T) {
	pool, databaseURL := testDatabase(t)
	migrate(t, databaseURL)
	ctx := context.Background()

	_, versionID := seedContent(t, pool, "ROLE1")

	err := asRole(t, pool, "reelpin_app", func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, `UPDATE reelpin.content_versions SET title = 'edited' WHERE id = $1`, versionID)
		return err
	})
	if sqlState(err) != "42501" {
		t.Errorf("app update err = %v, want permission denied", err)
	}

	err = asRole(t, pool, "reelpin_app", func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, `DELETE FROM reelpin.content_versions WHERE id = $1`, versionID)
		return err
	})
	if sqlState(err) != "42501" {
		t.Errorf("app delete err = %v, want permission denied", err)
	}

	// The application role sees its own schema and nothing else's tables.
	err = asRole(t, pool, "reelpin_app", func(conn *pgxpool.Conn) error {
		var count int
		return conn.QueryRow(ctx, `SELECT count(*) FROM public.schema_migrations`).Scan(&count)
	})
	if sqlState(err) != "42501" {
		t.Errorf("reading another schema err = %v, want permission denied", err)
	}

	err = asRole(t, pool, "reelpin_app", func(conn *pgxpool.Conn) error {
		var count int
		return conn.QueryRow(ctx, `SELECT count(*) FROM reelpin.contents`).Scan(&count)
	})
	if err != nil {
		t.Errorf("the app role cannot read its own tables: %v", err)
	}

	// The maintenance role is the one allowed to delete a version, for the
	// audited global purge.
	err = asRole(t, pool, "reelpin_maintenance", func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, `DELETE FROM reelpin.content_versions WHERE id = $1`, versionID)
		return err
	})
	if err != nil {
		t.Errorf("maintenance delete failed: %v", err)
	}
}

func TestDownUnwindsADisposableDatabase(t *testing.T) {
	pool, databaseURL := testDatabase(t)
	migrate(t, databaseURL)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := Down(ctx, databaseURL); err != nil {
			t.Fatalf("Down %d: %v", i+1, err)
		}
	}

	var tables int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables WHERE table_schema = 'reelpin'`,
	).Scan(&tables); err != nil {
		t.Fatalf("counting tables: %v", err)
	}
	if tables != 0 {
		t.Fatalf("%d tables survived a full rollback", tables)
	}
}
