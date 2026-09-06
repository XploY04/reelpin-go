//go:build integration

package lease

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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

	name := "reelpin_lease_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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
	return pool
}

func seedRun(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var runID string
	err := pool.QueryRow(context.Background(), `
		WITH content AS (
			INSERT INTO reelpin.contents
				(source_platform, source_content_type, source_content_id,
				 normalized_url, normalized_url_hash, access_scope_hash)
			VALUES ('instagram', 'reel', 'SEED1',
			        'https://www.instagram.com/reel/SEED1/', 'seed-hash', 'public')
			RETURNING id
		)
		INSERT INTO reelpin.processing_runs (content_id, processor_version)
		SELECT id, 'v1' FROM content
		RETURNING id::text`).Scan(&runID)
	if err != nil {
		t.Fatalf("seeding a run: %v", err)
	}
	return runID
}

func TestOnlyOneOwnerHoldsARun(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)
	ctx := context.Background()

	first, err := Acquire(ctx, pool, runID, "worker-a")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if first.Generation != 1 {
		t.Fatalf("generation = %d, want the first claim to be 1", first.Generation)
	}

	if _, err := Acquire(ctx, pool, runID, "worker-b"); !errors.Is(err, ErrNotAcquired) {
		t.Fatalf("second acquire err = %v, want ErrNotAcquired while the lease is live", err)
	}
}

func TestAnExpiredLeaseIsReclaimedAndThePastIsFenced(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)
	ctx := context.Background()

	old, err := Acquire(ctx, pool, runID, "worker-a")
	if err != nil {
		t.Fatal(err)
	}

	// The worker pauses (GC, network, anything) and its lease runs out.
	if _, err := pool.Exec(ctx, `
		UPDATE reelpin.processing_runs
		SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, runID); err != nil {
		t.Fatal(err)
	}

	fresh, err := Acquire(ctx, pool, runID, "worker-b")
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if fresh.Generation != old.Generation+1 {
		t.Fatalf("generation = %d, want %d: the claim is what fences the past", fresh.Generation, old.Generation+1)
	}

	// The paused worker wakes up. Every path it has must refuse.
	if _, err := Renew(ctx, pool, old); !errors.Is(err, ErrFenced) {
		t.Errorf("stale renew err = %v, want ErrFenced", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = GuardedExec(ctx, tx, old, func(pgx.Tx) error { return nil })
	_ = tx.Rollback(ctx)
	if !errors.Is(err, ErrFenced) {
		t.Errorf("stale commit err = %v, want ErrFenced", err)
	}

	// The new claim still works.
	if _, err := Renew(ctx, pool, fresh); err != nil {
		t.Errorf("fresh renew: %v", err)
	}
}

func TestTheSweeperCreatesExactlyOneResumeEvent(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)
	ctx := context.Background()

	old, err := Acquire(ctx, pool, runID, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE reelpin.processing_runs
		SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, runID); err != nil {
		t.Fatal(err)
	}

	swept, err := SweepExpired(ctx, pool, queue.QueueLight, 10)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want the one expired run", swept)
	}

	// The old worker stays fenced even though the sweep requeued the run.
	if _, err := Renew(ctx, pool, old); !errors.Is(err, ErrFenced) {
		t.Errorf("the swept worker can still renew: %v", err)
	}

	var events int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM reelpin.outbox_events WHERE event_type = 'run.resume'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("resume events = %d, want exactly one", events)
	}

	// Sweeping again finds a queued run (not expired-processing) and adds
	// nothing: the event id is deterministic per generation.
	again, err := SweepExpired(ctx, pool, queue.QueueLight, 10)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("second sweep swept %d", again)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM reelpin.outbox_events WHERE event_type = 'run.resume'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("resume events after resweep = %d, want still one", events)
	}
}

func TestAGuardedCommitCarriesTheWork(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)
	ctx := context.Background()

	held, err := Acquire(ctx, pool, runID, "worker-a")
	if err != nil {
		t.Fatal(err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = GuardedExec(ctx, tx, held, func(guarded pgx.Tx) error {
		_, err := guarded.Exec(ctx, `
			UPDATE reelpin.processing_runs SET stage = 'download' WHERE id = $1`, runID)
		return err
	})
	if err != nil {
		tx.Rollback(ctx)
		t.Fatalf("guarded commit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var stage string
	if err := pool.QueryRow(ctx,
		`SELECT stage FROM reelpin.processing_runs WHERE id = $1`, runID).Scan(&stage); err != nil {
		t.Fatal(err)
	}
	if stage != "download" {
		t.Fatalf("stage = %q", stage)
	}
}

func TestReleaseReturnsTheRunToTheQueue(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)
	ctx := context.Background()

	held, err := Acquire(ctx, pool, runID, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := Release(ctx, pool, held); err != nil {
		t.Fatal(err)
	}

	// Anyone can claim it again immediately, at a higher generation.
	next, err := Acquire(ctx, pool, runID, "worker-b")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if next.Generation != held.Generation+1 {
		t.Errorf("generation = %d, want %d", next.Generation, held.Generation+1)
	}
}
