//go:build integration

package postgres

import (
	"context"
	"testing"
)

func TestMigrationRequiresDrainedCanonicalWork(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := CheckMigrationIdle(ctx, pool, false); err != nil {
		t.Fatalf("empty work tables: %v", err)
	}
	for _, statement := range []string{
		`INSERT INTO reelpin.processing_jobs (user_id, url, normalized_url, status) VALUES ('11111111-1111-4111-8111-111111111111', 'https://example.com', 'https://example.com', 'queued')`,
		`WITH c AS (INSERT INTO reelpin.contents (source_platform, source_content_type, normalized_url, normalized_url_hash, access_scope_hash)
		 VALUES ('other', 'page', 'https://example.com', 'example', 'public') RETURNING id)
		 INSERT INTO reelpin.processing_runs (content_id, processor_version, status) SELECT id, 'v1', 'retry_scheduled' FROM c`,
		`INSERT INTO reelpin.outbox_events (event_id, event_type, routing_key, schema_version, payload)
		 VALUES (gen_random_uuid(), 'run.resume', 'reelpin.processing.light', 1, '{}')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
		if err := CheckMigrationIdle(ctx, pool, false); err == nil {
			t.Fatal("unfinished canonical work allowed migration")
		}
		if _, err := pool.Exec(ctx, `DELETE FROM reelpin.outbox_events; DELETE FROM reelpin.processing_jobs; DELETE FROM reelpin.processing_runs`); err != nil {
			t.Fatal(err)
		}
		if err := CheckMigrationIdle(ctx, pool, false); err != nil {
			t.Fatalf("drained work: %v", err)
		}
	}
	if err := CheckMigrationIdle(ctx, pool, true); err == nil {
		t.Fatal("an empty broker allowed bootstrap of an initialized database")
	}
	if _, err := pool.Exec(ctx, `DROP TABLE reelpin.outbox_events`); err != nil {
		t.Fatal(err)
	}
	if err := CheckMigrationIdle(ctx, pool, false); err == nil {
		t.Fatal("partial work schema allowed migration")
	}
	if _, err := pool.Exec(ctx, `DROP SCHEMA reelpin CASCADE; DROP SCHEMA auth CASCADE; DROP TABLE public.schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := CheckMigrationIdle(ctx, pool, true); err != nil {
		t.Fatalf("first migration on an empty database: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE public.unexpected (id integer)`); err != nil {
		t.Fatal(err)
	}
	if err := CheckMigrationIdle(ctx, pool, true); err == nil {
		t.Fatal("bootstrap allowed existing application data outside the Go schemas")
	}
}
