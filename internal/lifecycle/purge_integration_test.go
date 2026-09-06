//go:build integration

package lifecycle

import (
	"context"
	"strings"
	"testing"
	"time"
)

// recordingObjects stands in for the object store, so a purge can be shown to
// hand over every stored file it found.
type recordingObjects struct {
	deleted []string
	err     error
}

func (r *recordingObjects) Delete(_ context.Context, ref string) error {
	if r.err != nil {
		return r.err
	}
	r.deleted = append(r.deleted, ref)
	return nil
}

func TestPurgeRemovesEveryDerivativeAndBlocksReingestion(t *testing.T) {
	pool := testPool(t)
	objects := &recordingObjects{}
	purge := NewPurge(pool, objects, quiet())
	ctx := context.Background()

	content := seedContent(t, pool, "PURGE1", "public")
	seedSave(t, pool, userA, content)
	seedSave(t, pool, userB, content)
	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.processing_runs (content_id, processor_version)
		VALUES ($1, 'v1')`, content); err != nil {
		t.Fatal(err)
	}

	// Something unrelated, to prove the purge is aimed rather than broad.
	other := seedContent(t, pool, "KEEP1", "public")
	seedSave(t, pool, userA, other)

	target := PurgeTarget{Platform: "instagram", ContentType: "reel", ContentID: "PURGE1"}
	report, err := purge.Run(ctx, target, "privacy request", "operator@example.com")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.Contents != 1 || report.Versions != 1 || report.Saves != 2 || report.Runs != 1 {
		t.Fatalf("report = %+v, want everything derived from the source", report)
	}
	if !report.Blocklisted {
		t.Error("the source was not blocklisted")
	}
	if report.Objects != 1 || report.ObjectsSkipped != 0 {
		t.Errorf("objects deleted = %d, skipped = %d, want the one thumbnail deleted",
			report.Objects, report.ObjectsSkipped)
	}
	if len(objects.deleted) != 1 || !strings.Contains(objects.deleted[0], "PURGE1") {
		t.Errorf("objects handed over = %v", objects.deleted)
	}

	// Every derivative is gone.
	for _, check := range []struct {
		name  string
		query string
	}{
		{"contents", `SELECT count(*) FROM reelpin.contents WHERE source_content_id = 'PURGE1'`},
		{"versions", `SELECT count(*) FROM reelpin.content_versions v
		              JOIN reelpin.contents c ON c.id = v.content_id
		              WHERE c.source_content_id = 'PURGE1'`},
		{"saves", `SELECT count(*) FROM reelpin.user_saves WHERE content_id = $1`},
	} {
		var n int
		if strings.Contains(check.query, "$1") {
			n = count(t, pool, check.query, content)
		} else {
			n = count(t, pool, check.query)
		}
		if n != 0 {
			t.Errorf("%s survived the purge: %d", check.name, n)
		}
	}

	// Unrelated content is untouched.
	if n := count(t, pool, `SELECT count(*) FROM reelpin.contents WHERE id = $1`, other); n != 1 {
		t.Error("the purge removed unrelated content")
	}

	// And the source cannot come back: the database refuses the insert, so it
	// does not matter which caller forgets to check the blocklist.
	_, err = pool.Exec(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id,
			 normalized_url, normalized_url_hash, access_scope_hash)
		VALUES ('instagram', 'reel', 'PURGE1', 'https://www.instagram.com/reel/PURGE1/', 'PURGE1', 'public')`)
	if err == nil {
		t.Fatal("the purged source was reingested")
	}
	if !strings.Contains(err.Error(), "blocklisted") {
		t.Errorf("err = %v, want the blocklist rejection", err)
	}

	if blocked, err := purge.Blocked(ctx, target); err != nil || !blocked {
		t.Errorf("Blocked = %v, %v; want true", blocked, err)
	}
}

func TestPurgeByURLHashForSourcesWithNoStableID(t *testing.T) {
	pool := testPool(t)
	purge := NewPurge(pool, &recordingObjects{}, quiet())
	ctx := context.Background()

	var contentID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, normalized_url, normalized_url_hash, access_scope_hash)
		VALUES ('web', 'page', 'https://example.com/a', 'urlhash1', 'public')
		RETURNING id::text`).Scan(&contentID); err != nil {
		t.Fatal(err)
	}
	seedSave(t, pool, userA, contentID)

	report, err := purge.Run(ctx,
		PurgeTarget{Platform: "web", ContentType: "page", URLHash: "urlhash1"},
		"legal request", "operator@example.com")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Contents != 1 || report.Saves != 1 {
		t.Fatalf("report = %+v", report)
	}

	// The same URL cannot be ingested again, whatever platform claims it.
	_, err = pool.Exec(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, normalized_url, normalized_url_hash, access_scope_hash)
		VALUES ('web', 'page', 'https://example.com/a', 'urlhash1', 'public')`)
	if err == nil {
		t.Fatal("the purged URL was reingested")
	}
}

func TestPurgeWithoutAnObjectStoreSaysSo(t *testing.T) {
	pool := testPool(t)
	// Nil store: the files stay on disk, and the report must not pretend
	// otherwise.
	purge := NewPurge(pool, nil, quiet())
	ctx := context.Background()

	content := seedContent(t, pool, "NOSTORE1", "public")
	seedSave(t, pool, userA, content)

	report, err := purge.Run(ctx,
		PurgeTarget{Platform: "instagram", ContentType: "reel", ContentID: "NOSTORE1"},
		"privacy request", "operator@example.com")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Objects != 0 || report.ObjectsSkipped != 1 {
		t.Fatalf("objects = %d, skipped = %d, want the file reported as not deleted",
			report.Objects, report.ObjectsSkipped)
	}
	if !report.Blocklisted || report.Contents != 1 {
		t.Errorf("report = %+v, want the database half done regardless", report)
	}
}

func TestPurgeRefusesAnIncompleteTarget(t *testing.T) {
	pool := testPool(t)
	purge := NewPurge(pool, nil, quiet())
	ctx := context.Background()

	for _, tt := range []struct {
		name             string
		target           PurgeTarget
		reason, operator string
	}{
		{"no identity", PurgeTarget{Platform: "instagram", ContentType: "reel"}, "r", "o"},
		{"no platform", PurgeTarget{ContentType: "reel", ContentID: "X"}, "r", "o"},
		{"no reason", PurgeTarget{Platform: "instagram", ContentType: "reel", ContentID: "X"}, "", "o"},
		{"no operator", PurgeTarget{Platform: "instagram", ContentType: "reel", ContentID: "X"}, "r", ""},
	} {
		if _, err := purge.Run(ctx, tt.target, tt.reason, tt.operator); err == nil {
			t.Errorf("%s: a purge ran without it", tt.name)
		}
	}
}

func TestRetentionSweepsWhatHasOutlivedItsUse(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	now := time.Now()

	// An expired key, a live one.
	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.idempotency_keys
			(user_id, endpoint, idempotency_key, request_hash, expires_at)
		VALUES ($1, 'e', gen_random_uuid(), 'h', now() - interval '1 hour'),
		       ($1, 'e', gen_random_uuid(), 'h', now() + interval '1 hour')`, userA); err != nil {
		t.Fatal(err)
	}

	// A published event past its window, and a fresh unpublished one.
	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.outbox_events
			(event_id, event_type, routing_key, schema_version, payload, published_at)
		VALUES (gen_random_uuid(), 'run.process.light', 'reelpin.processing.light', 1, '{}'::jsonb,
		        now() - interval '30 days'),
		       (gen_random_uuid(), 'run.process.light', 'reelpin.processing.light', 1, '{}'::jsonb, NULL)`); err != nil {
		t.Fatal(err)
	}

	// A stage result on a finished run, and one on a run still going.
	content := seedContent(t, pool, "RETAIN1", "public")
	var finished, running string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.processing_runs (content_id, processor_version, status, updated_at)
		VALUES ($1, 'v1', 'completed', now() - interval '30 days')
		RETURNING id::text`, content).Scan(&finished); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.processing_runs (content_id, processor_version, status, updated_at)
		VALUES ($1, 'v2', 'processing', now() - interval '30 days')
		RETURNING id::text`, content).Scan(&running); err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{finished, running} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO reelpin.processing_stage_results
				(run_id, stage, stage_version, input_hash)
			VALUES ($1, 'extract', 'v1', 'hash')`, runID); err != nil {
			t.Fatal(err)
		}
	}

	report, err := NewRetention(pool, "").Sweep(ctx, now)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if report.IdempotencyKeys != 1 {
		t.Errorf("idempotency keys removed = %d, want the expired one only", report.IdempotencyKeys)
	}
	if report.PublishedEvents != 1 {
		t.Errorf("published events removed = %d, want the old one only", report.PublishedEvents)
	}
	if report.StageResults != 1 {
		t.Errorf("stage results removed = %d, want the finished run's only", report.StageResults)
	}

	// The unfinished run keeps its checkpoints: it still needs them to resume.
	if n := count(t, pool, `SELECT count(*) FROM reelpin.processing_stage_results WHERE run_id = $1`, running); n != 1 {
		t.Error("an unfinished run lost the checkpoints it needs to resume")
	}
	if n := count(t, pool, `SELECT count(*) FROM reelpin.outbox_events WHERE published_at IS NULL`); n != 1 {
		t.Error("an unpublished event was swept")
	}
}
