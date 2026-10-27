//go:build integration

package main

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testDatabaseURL(t *testing.T) (string, *pgxpool.Pool) {
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

	name := "reelpin_rebuild_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return parsed.String(), pool
}

func TestRebuildQueueRecreatesUnfinishedWorkOnce(t *testing.T) {
	databaseURL, pool := testDatabaseURL(t)
	ctx := context.Background()

	// Two unfinished runs: one queued whose event was already "published" (and
	// then lost with the broker), one mid-lease whose worker died with it. And
	// one completed run that must not come back.
	seed := func(sourceID, status string) string {
		var runID string
		if err := pool.QueryRow(ctx, `
			WITH content AS (
				INSERT INTO reelpin.contents
					(source_platform, source_content_type, source_content_id,
					 normalized_url, normalized_url_hash, access_scope_hash)
				VALUES ('instagram', 'reel', $1,
				        'https://www.instagram.com/reel/'||$1||'/', $1, 'public')
				RETURNING id
			)
			INSERT INTO reelpin.processing_runs (content_id, processor_version, status)
			SELECT id, 'v1', $2 FROM content
			RETURNING id::text`, sourceID, status).Scan(&runID); err != nil {
			t.Fatalf("seeding %s: %v", sourceID, err)
		}
		return runID
	}
	queued := seed("QUEUED1", "queued")
	leased := seed("LEASED1", "processing")
	seed("DONE1", "completed")

	// The queued run's original event was published before the broker died.
	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.outbox_events (event_id, event_type, routing_key, schema_version, payload, published_at)
		VALUES ('99999999-9999-4999-8999-999999999999', 'run.process.media', 'reelpin.processing.media', 1,
		        jsonb_build_object('run_id', $1::text, 'dispatch_generation', 0), now())`, queued); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{DatabaseURL: databaseURL}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if err := runRebuildQueue(ctx, logger, cfg, []string{"--broker-empty"}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	var resumes int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM reelpin.outbox_events
		WHERE event_type = 'run.resume' AND published_at IS NULL`).Scan(&resumes); err != nil {
		t.Fatal(err)
	}
	if resumes != 2 {
		t.Fatalf("resume events = %d, want one per unfinished run", resumes)
	}

	// The media run resumes on the media queue, taken from its own history.
	var mediaRouting string
	if err := pool.QueryRow(ctx, `
		SELECT routing_key FROM reelpin.outbox_events
		WHERE event_type = 'run.resume' AND payload->>'run_id' = $1`, queued).Scan(&mediaRouting); err != nil {
		t.Fatal(err)
	}
	if mediaRouting != "reelpin.processing.media" {
		t.Errorf("the media run resumes on %q", mediaRouting)
	}
	var leasedRouting string
	if err := pool.QueryRow(ctx, `
		SELECT routing_key FROM reelpin.outbox_events
		WHERE event_type = 'run.resume' AND payload->>'run_id' = $1`, leased).Scan(&leasedRouting); err != nil {
		t.Fatal(err)
	}
	if leasedRouting != "reelpin.processing.light" {
		t.Errorf("a run with no history resumes on %q, want the light fallback", leasedRouting)
	}

	// Run it again: deterministic event ids mean nothing new appears.
	if err := runRebuildQueue(ctx, logger, cfg, []string{"--broker-empty"}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM reelpin.outbox_events WHERE event_type = 'run.resume'`).Scan(&resumes); err != nil {
		t.Fatal(err)
	}
	if resumes != 2 {
		t.Fatalf("resume events after a second rebuild = %d, want still 2", resumes)
	}
}

func TestRebuildQueueRefusesWithoutTheAssertion(t *testing.T) {
	cfg := config.Config{DatabaseURL: "postgres://localhost:1/never-reached"}
	err := runRebuildQueue(context.Background(),
		slog.New(slog.NewJSONHandler(io.Discard, nil)), cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "--broker-empty") {
		t.Fatalf("err = %v, want a refusal naming --broker-empty: against a live broker this duplicates every delivery", err)
	}
}
