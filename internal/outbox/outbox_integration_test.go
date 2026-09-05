//go:build integration

package outbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/XploY04/reelpin-go/internal/queue"
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

	name := "reelpin_outbox_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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

	// Supabase owns auth.users in every real deployment.
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

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// recordingPublisher stands in for the broker: it can fail on demand and
// remembers what it accepted.
type recordingPublisher struct {
	mu        sync.Mutex
	published []queue.Message
	fail      bool
}

func (r *recordingPublisher) Publish(_ context.Context, routingKey string, message queue.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("the broker is away")
	}
	r.published = append(r.published, message)
	return nil
}

func (r *recordingPublisher) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.published)
}

func insertEvent(t *testing.T, pool *pgxpool.Pool, eventID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	insertErr := Insert(ctx, tx, Event{
		EventID:            eventID,
		EventType:          queue.EventProcessLight,
		RoutingKey:         queue.QueueLight,
		RunID:              "22222222-2222-4222-8222-222222222222",
		DispatchGeneration: 1,
	})
	if insertErr != nil {
		tx.Rollback(ctx)
		t.Fatalf("insert: %v", insertErr)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestACommittedEventIsPublishedOnceAndMarked(t *testing.T) {
	pool := testPool(t)
	publisher := &recordingPublisher{}
	dispatcher := NewDispatcher(pool, publisher, quiet(), 10)
	ctx := context.Background()

	insertEvent(t, pool, "11111111-1111-4111-8111-111111111111")

	published, err := dispatcher.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if published != 1 || publisher.count() != 1 {
		t.Fatalf("published %d, broker saw %d", published, publisher.count())
	}
	if publisher.published[0].DispatchGeneration != 1 {
		t.Errorf("dispatch generation = %d, want the inserted one", publisher.published[0].DispatchGeneration)
	}

	// A second pass finds nothing: the row is marked.
	again, err := dispatcher.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if again != 0 || publisher.count() != 1 {
		t.Fatalf("a marked event was republished (%d, %d)", again, publisher.count())
	}
}

func TestARolledBackEventNeverExists(t *testing.T) {
	pool := testPool(t)
	publisher := &recordingPublisher{}
	dispatcher := NewDispatcher(pool, publisher, quiet(), 10)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := Insert(ctx, tx, Event{
		EventID:    "33333333-3333-4333-8333-333333333333",
		EventType:  queue.EventProcessLight,
		RoutingKey: queue.QueueLight,
		RunID:      "22222222-2222-4222-8222-222222222222",
	}); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	// The business transaction failed: the work it described never happened,
	// so the event must not either. This is the kill-before-commit case.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	published, err := dispatcher.DispatchOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if published != 0 || publisher.count() != 0 {
		t.Fatal("a rolled-back event reached the broker")
	}
}

func TestABrokerOutageBacksOffAndRecovers(t *testing.T) {
	pool := testPool(t)
	publisher := &recordingPublisher{fail: true}
	dispatcher := NewDispatcher(pool, publisher, quiet(), 10)
	ctx := context.Background()

	insertEvent(t, pool, "44444444-4444-4444-8444-444444444444")

	// The broker is away: nothing publishes, the attempt is recorded, and the
	// row backs off rather than spinning.
	if published, err := dispatcher.DispatchOnce(ctx); err != nil || published != 0 {
		t.Fatalf("published %d (%v) during the outage", published, err)
	}
	var attempts int
	var available time.Time
	if err := pool.QueryRow(ctx, `
		SELECT attempts, available_at FROM reelpin.outbox_events
		WHERE event_id = '44444444-4444-4444-8444-444444444444'`).Scan(&attempts, &available); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !available.After(time.Now()) {
		t.Fatalf("attempts = %d, available_at = %s: the failure was not recorded with backoff", attempts, available)
	}

	// This is the stop-RabbitMQ-after-commit case: recovery publishes it.
	publisher.fail = false
	if _, err := pool.Exec(ctx, `
		UPDATE reelpin.outbox_events SET available_at = now()
		WHERE event_id = '44444444-4444-4444-8444-444444444444'`); err != nil {
		t.Fatal(err)
	}
	if published, err := dispatcher.DispatchOnce(ctx); err != nil || published != 1 {
		t.Fatalf("published %d (%v) after recovery", published, err)
	}
}

func TestTwoDispatchersShareWithoutDuplicating(t *testing.T) {
	pool := testPool(t)
	publisher := &recordingPublisher{}
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		insertEvent(t, pool, fmt.Sprintf("55555555-5555-4555-8555-%012d", i))
	}

	var wait sync.WaitGroup
	totals := make([]int, 4)
	for worker := 0; worker < 4; worker++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			dispatcher := NewDispatcher(pool, publisher, quiet(), 5)
			for {
				published, err := dispatcher.DispatchOnce(ctx)
				if err != nil {
					t.Errorf("dispatcher %d: %v", index, err)
					return
				}
				totals[index] += published
				if published == 0 {
					return
				}
			}
		}(worker)
	}
	wait.Wait()

	if publisher.count() != 20 {
		t.Fatalf("the broker saw %d messages, want each of the 20 exactly once", publisher.count())
	}
}

func TestInsertIsIdempotentByEventID(t *testing.T) {
	pool := testPool(t)
	insertEvent(t, pool, "66666666-6666-4666-8666-666666666666")
	insertEvent(t, pool, "66666666-6666-4666-8666-666666666666")

	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM reelpin.outbox_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("rows = %d, want the duplicate insert to change nothing", count)
	}
}
