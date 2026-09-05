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

// recordingPublisher stands in for the broker.
type recordingPublisher struct {
	mu       sync.Mutex
	messages []queue.Message
	queues   []string
	err      error
}

func (p *recordingPublisher) Publish(_ context.Context, name string, message queue.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.queues = append(p.queues, name)
	p.messages = append(p.messages, message)
	return nil
}

func (p *recordingPublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.messages)
}

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func insertEvent(t *testing.T, pool *pgxpool.Pool, eventID, runID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	if err := Insert(ctx, tx, Event{
		EventID:    eventID,
		EventType:  "content.process",
		RoutingKey: queue.QueueInstagram,
		Payload:    map[string]any{"run_id": runID, "platform": "instagram"},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestEventIsPublishedOnceAndMarked(t *testing.T) {
	pool := testPool(t)
	publisher := &recordingPublisher{}
	dispatcher := NewDispatcher(pool, publisher, quiet(), 10)
	ctx := context.Background()

	insertEvent(t, pool, "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222")

	published, err := dispatcher.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if published != 1 {
		t.Fatalf("published %d, want 1", published)
	}
	if publisher.queues[0] != queue.QueueInstagram {
		t.Errorf("published to %q, want the routing key", publisher.queues[0])
	}
	if publisher.messages[0].RunID != "22222222-2222-4222-8222-222222222222" {
		t.Errorf("message = %+v", publisher.messages[0])
	}

	// The second pass must find nothing: a published row is never republished.
	again, err := dispatcher.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if again != 0 || publisher.count() != 1 {
		t.Fatalf("the event was published twice")
	}
}

func TestAnEventRolledBackIsNeverPublished(t *testing.T) {
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
		EventType:  "content.process",
		RoutingKey: queue.QueueInstagram,
		Payload:    map[string]any{"run_id": "44444444-4444-4444-8444-444444444444", "platform": "instagram"},
	}); err != nil {
		t.Fatal(err)
	}
	// The business transaction failed, so the work it described never happened.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	if published, err := dispatcher.DispatchOnce(ctx); err != nil || published != 0 {
		t.Fatalf("published %d (err %v), want nothing", published, err)
	}
}

func TestPublishFailureLeavesTheRowForRetry(t *testing.T) {
	pool := testPool(t)
	publisher := &recordingPublisher{err: errors.New("broker is unreachable")}
	dispatcher := NewDispatcher(pool, publisher, quiet(), 10)
	ctx := context.Background()

	insertEvent(t, pool, "55555555-5555-4555-8555-555555555555", "66666666-6666-4666-8666-666666666666")

	published, err := dispatcher.DispatchOnce(ctx)
	if err != nil || published != 0 {
		t.Fatalf("published %d (err %v), want nothing", published, err)
	}

	var attempts int
	var publishedAt *time.Time
	var lastError *string
	var availableAt time.Time
	if err := pool.QueryRow(ctx, `
		SELECT attempts, published_at, last_error, available_at FROM reelpin.outbox_events`,
	).Scan(&attempts, &publishedAt, &lastError, &availableAt); err != nil {
		t.Fatalf("reading the row: %v", err)
	}
	if attempts != 1 || publishedAt != nil {
		t.Fatalf("attempts=%d published_at=%v, want one recorded failure", attempts, publishedAt)
	}
	if lastError == nil || !strings.Contains(*lastError, "unreachable") {
		t.Errorf("last_error = %v", lastError)
	}
	if !availableAt.After(time.Now()) {
		t.Error("the failed row is available immediately, so there is no backoff")
	}

	// Once the broker recovers, the same row publishes.
	publisher.err = nil
	if _, err := pool.Exec(ctx, `UPDATE reelpin.outbox_events SET available_at = now()`); err != nil {
		t.Fatal(err)
	}
	if published, err := dispatcher.DispatchOnce(ctx); err != nil || published != 1 {
		t.Fatalf("published %d (err %v), want the recovered event", published, err)
	}
}

func TestPoisonedRowIsParkedAfterMaxAttempts(t *testing.T) {
	pool := testPool(t)
	publisher := &recordingPublisher{err: errors.New("nope")}
	dispatcher := NewDispatcher(pool, publisher, quiet(), 10)
	ctx := context.Background()

	insertEvent(t, pool, "77777777-7777-4777-8777-777777777777", "88888888-8888-4888-8888-888888888888")
	if _, err := pool.Exec(ctx, `UPDATE reelpin.outbox_events SET attempts = $1`, maxAttempts); err != nil {
		t.Fatal(err)
	}

	// A row that has exhausted its attempts must not be claimed again, or it
	// blocks everything behind it forever.
	if published, err := dispatcher.DispatchOnce(ctx); err != nil || published != 0 {
		t.Fatalf("published %d (err %v)", published, err)
	}
	var attempts int
	pool.QueryRow(ctx, `SELECT attempts FROM reelpin.outbox_events`).Scan(&attempts)
	if attempts != maxAttempts {
		t.Errorf("attempts = %d, want the row left alone at %d", attempts, maxAttempts)
	}
}

func TestConcurrentDispatchersDoNotDoubleSend(t *testing.T) {
	pool := testPool(t)
	publisher := &recordingPublisher{}
	ctx := context.Background()

	const events = 20
	for i := 0; i < events; i++ {
		insertEvent(t, pool,
			fmt.Sprintf("aaaaaaaa-0000-4000-8000-%012d", i),
			"bbbbbbbb-0000-4000-8000-000000000001")
	}

	const dispatchers = 4
	var wait sync.WaitGroup
	for i := 0; i < dispatchers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			dispatcher := NewDispatcher(pool, publisher, quiet(), 5)
			for pass := 0; pass < 5; pass++ {
				if _, err := dispatcher.DispatchOnce(ctx); err != nil {
					t.Errorf("dispatch: %v", err)
					return
				}
			}
		}()
	}
	wait.Wait()

	if publisher.count() != events {
		t.Fatalf("published %d messages for %d events", publisher.count(), events)
	}

	var unpublished int
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.outbox_events WHERE published_at IS NULL`).Scan(&unpublished)
	if unpublished != 0 {
		t.Errorf("%d events were left unpublished", unpublished)
	}
}

func TestDuplicateEventIDIsWrittenOnce(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	insertEvent(t, pool, "99999999-9999-4999-8999-999999999999", "cccccccc-0000-4000-8000-000000000001")
	insertEvent(t, pool, "99999999-9999-4999-8999-999999999999", "cccccccc-0000-4000-8000-000000000001")

	var count int
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.outbox_events`).Scan(&count)
	if count != 1 {
		t.Fatalf("%d rows for one event id, want 1", count)
	}
}

// The database's clock decides when a row is due. This test fails if Insert
// ever goes back to stamping available_at from the application process, which
// makes rows undispatchable whenever the app host runs ahead of the database.
func TestAvailabilityUsesTheDatabaseClock(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	insertEvent(t, pool, "dddddddd-0000-4000-8000-000000000001", "eeeeeeee-0000-4000-8000-000000000001")

	var due bool
	if err := pool.QueryRow(ctx,
		`SELECT available_at <= now() FROM reelpin.outbox_events`,
	).Scan(&due); err != nil {
		t.Fatalf("reading availability: %v", err)
	}
	if !due {
		t.Fatal("a freshly written event is not due yet by the database's clock")
	}
}
