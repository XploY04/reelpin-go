//go:build integration

package lease

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/migrations"
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

	parsed, _ := url.Parse(adminURL)
	parsed.Path = "/" + name
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}

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
	return pool
}

func seedRun(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var runID string
	err := pool.QueryRow(context.Background(), `
		WITH content AS (
			INSERT INTO reelpin.contents
				(source_platform, source_content_type, source_content_id, normalized_url, normalized_url_hash)
			VALUES ('instagram','reel','SEED','https://www.instagram.com/reel/SEED/','seed-hash')
			RETURNING id
		)
		INSERT INTO reelpin.processing_runs (content_id, processor_version, platform, status)
		SELECT id, 'v1', 'instagram', 'queued' FROM content
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
	if first.Owner != "worker-a" || first.ExpiresAt.Before(time.Now()) {
		t.Fatalf("lease = %+v", first)
	}

	// A duplicate delivery reaching another worker finds the run taken.
	if _, err := Acquire(ctx, pool, runID, "worker-b"); !errors.Is(err, ErrNotAcquired) {
		t.Fatalf("second acquire error = %v, want ErrNotAcquired", err)
	}

	// The holder may re-acquire: that is a duplicate delivery to itself.
	if _, err := Acquire(ctx, pool, runID, "worker-a"); err != nil {
		t.Fatalf("re-acquire by the holder: %v", err)
	}

	var attempts int
	if err := pool.QueryRow(ctx,
		`SELECT attempt_count FROM reelpin.processing_runs WHERE id = $1`, runID,
	).Scan(&attempts); err != nil {
		t.Fatalf("reading attempts: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempt_count = %d, want one per acquisition", attempts)
	}
}

func TestConcurrentAcquireHasOneWinner(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)
	ctx := context.Background()

	const workers = 12
	var wait sync.WaitGroup
	results := make(chan error, workers)
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			<-start
			_, err := Acquire(ctx, pool, runID, "worker-"+string(rune('a'+i)))
			results <- err
		}(i)
	}
	close(start)
	wait.Wait()
	close(results)

	won := 0
	for err := range results {
		if err == nil {
			won++
		} else if !errors.Is(err, ErrNotAcquired) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d workers acquired the same run, want 1", won)
	}
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)
	ctx := context.Background()

	if _, err := Acquire(ctx, pool, runID, "worker-a"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// The worker went away without releasing.
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.processing_runs SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, runID,
	); err != nil {
		t.Fatalf("expiring the lease: %v", err)
	}

	expired, err := ExpiredRuns(ctx, pool, 10)
	if err != nil {
		t.Fatalf("listing expired: %v", err)
	}
	if len(expired) != 1 || expired[0] != runID {
		t.Fatalf("expired = %v, want the abandoned run", expired)
	}

	// Another worker takes it over without waiting for a sweep.
	if _, err := Acquire(ctx, pool, runID, "worker-b"); err != nil {
		t.Fatalf("taking over an expired lease: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.processing_runs SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, runID,
	); err != nil {
		t.Fatalf("expiring again: %v", err)
	}
	reclaimed, err := ReclaimExpired(ctx, pool, 10)
	if err != nil {
		t.Fatalf("reclaiming: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed %d runs, want 1", reclaimed)
	}

	var status string
	var owner *string
	if err := pool.QueryRow(ctx,
		`SELECT status, lease_owner FROM reelpin.processing_runs WHERE id = $1`, runID,
	).Scan(&status, &owner); err != nil {
		t.Fatalf("reading the run: %v", err)
	}
	if status != "queued" || owner != nil {
		t.Fatalf("after reclaim status=%q owner=%v, want a free queued run", status, owner)
	}
}

func TestRenewAndRelease(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)
	ctx := context.Background()

	if _, err := Acquire(ctx, pool, runID, "worker-a"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := Renew(ctx, pool, runID, "worker-a"); err != nil {
		t.Fatalf("renew: %v", err)
	}
	// A worker that no longer holds the lease must learn that from a renewal.
	if err := Renew(ctx, pool, runID, "worker-b"); !errors.Is(err, ErrNotAcquired) {
		t.Fatalf("renew by a non-holder = %v, want ErrNotAcquired", err)
	}

	if err := Release(ctx, pool, runID, "worker-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := Acquire(ctx, pool, runID, "worker-b"); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func TestKeepAliveStopsWorkWhenTheLeaseIsLost(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)
	ctx := context.Background()

	if _, err := Acquire(ctx, pool, runID, "worker-a"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	work, cancel := KeepAlive(ctx, pool, runID, "worker-a")
	defer cancel()

	if work.Err() != nil {
		t.Fatal("the work context was cancelled while the lease was held")
	}

	// Simulate the lease being taken while work is running: the next renewal
	// fails and must stop the handler.
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.processing_runs SET lease_owner = 'worker-b' WHERE id = $1`, runID,
	); err != nil {
		t.Fatalf("stealing the lease: %v", err)
	}
	if err := Renew(ctx, pool, runID, "worker-a"); !errors.Is(err, ErrNotAcquired) {
		t.Fatalf("renew after the lease was taken = %v, want ErrNotAcquired", err)
	}
}

func TestFinishedRunCannotBeAcquired(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.processing_runs SET status='completed' WHERE id = $1`, runID,
	); err != nil {
		t.Fatalf("completing the run: %v", err)
	}

	// This is the duplicate-delivery case that matters most: the work is done,
	// and a redelivered message must not start it again.
	if _, err := Acquire(ctx, pool, runID, "worker-a"); !errors.Is(err, ErrNotAcquired) {
		t.Fatalf("acquiring a completed run = %v, want ErrNotAcquired", err)
	}
}
