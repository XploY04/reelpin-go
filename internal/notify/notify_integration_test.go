//go:build integration

package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

// recordingSender counts sends and can fail on demand.
type recordingSender struct {
	mu        sync.Mutex
	sends     int
	tokens    []string
	invalid   map[string]bool
	retryable bool
	fail      bool
}

func (r *recordingSender) Send(_ context.Context, tokens []string, _ Message) ([]Delivery, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends++
	r.tokens = append(r.tokens, tokens...)
	if r.fail {
		return nil, errors.New("the provider is away")
	}

	deliveries := make([]Delivery, 0, len(tokens))
	for _, token := range tokens {
		switch {
		case r.invalid[token]:
			deliveries = append(deliveries, Delivery{Token: token, Invalid: true})
		case r.retryable:
			deliveries = append(deliveries, Delivery{Token: token, Retryable: true})
		default:
			deliveries = append(deliveries, Delivery{Token: token, MessageID: "provider-" + token})
		}
	}
	return deliveries, nil
}

func (r *recordingSender) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sends
}

func testService(t *testing.T) (*Service, *recordingSender, *pgxpool.Pool) {
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

	name := "reelpin_notify_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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
	if _, err := pool.Exec(ctx, `INSERT INTO auth.users (id) VALUES ($1), ($2)`, userA, userB); err != nil {
		t.Fatal(err)
	}

	sender := &recordingSender{invalid: map[string]bool{}}
	return NewService(pool, sender, slog.New(slog.NewJSONHandler(io.Discard, nil)), time.Now), sender, pool
}

func TestDuplicateEventsProduceOneNotification(t *testing.T) {
	service, sender, pool := testService(t)
	ctx := context.Background()

	if err := service.RegisterToken(ctx, userA, "token-a", "ios"); err != nil {
		t.Fatal(err)
	}

	notification := Notification{
		UserID: userA, EventKey: "job:abc", Type: "processing_completed",
		Title: "Your reel is ready", Body: "A cafe in Goa",
	}

	first, err := service.SendToUser(ctx, notification)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	// The same event redelivered: the claim is already taken.
	second, err := service.SendToUser(ctx, notification)
	if err != nil {
		t.Fatalf("second send: %v", err)
	}

	if first != second {
		t.Fatalf("two ids for one event: %s and %s", first, second)
	}
	if sender.count() != 1 {
		t.Fatalf("the provider was called %d times for one event", sender.count())
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.notifications`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("notification rows = %d, want one buzz per event", rows)
	}
}

func TestAnInvalidTokenIsRemovedAndARetryableOneIsNot(t *testing.T) {
	service, sender, pool := testService(t)
	ctx := context.Background()

	if err := service.RegisterToken(ctx, userA, "dead-token", "android"); err != nil {
		t.Fatal(err)
	}
	sender.invalid["dead-token"] = true

	if _, err := service.SendToUser(ctx, Notification{
		UserID: userA, EventKey: "job:dead", Title: "t", Body: "b",
	}); !errors.Is(err, ErrNoDeviceTokens) {
		t.Fatalf("err = %v, want no device accepted it", err)
	}

	var tokens int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.device_push_tokens`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 0 {
		t.Fatal("a permanently rejected token was kept")
	}

	// A transient refusal keeps the token: it will work again.
	if err := service.RegisterToken(ctx, userB, "flaky-token", "ios"); err != nil {
		t.Fatal(err)
	}
	sender.retryable = true
	if _, err := service.SendToUser(ctx, Notification{
		UserID: userB, EventKey: "job:flaky", Title: "t", Body: "b",
	}); !errors.Is(err, ErrRetryable) {
		t.Fatalf("err = %v, want the retryable signal", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.device_push_tokens`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 1 {
		t.Fatal("a transient failure removed a good token")
	}
}

func TestNoDeviceIsRecordedNotFailed(t *testing.T) {
	service, sender, pool := testService(t)
	ctx := context.Background()

	// The app often registers its token seconds after a first share.
	if _, err := service.SendToUser(ctx, Notification{
		UserID: userA, EventKey: "job:nodevice", Title: "t", Body: "b",
	}); !errors.Is(err, ErrNoDeviceTokens) {
		t.Fatalf("err = %v", err)
	}
	if sender.count() != 0 {
		t.Error("the provider was called with no devices")
	}

	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM reelpin.notifications WHERE event_key = 'job:nodevice'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "no_devices" {
		t.Fatalf("status = %q, want the row to record where it went", status)
	}
}

func TestATokenMovesToItsNewestUser(t *testing.T) {
	service, _, pool := testService(t)
	ctx := context.Background()

	// A shared phone: the newest sign-in owns the device.
	if err := service.RegisterToken(ctx, userA, "shared-phone", "ios"); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterToken(ctx, userB, "shared-phone", "ios"); err != nil {
		t.Fatal(err)
	}

	var owner string
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT user_id::text, count(*) OVER () FROM reelpin.device_push_tokens`).Scan(&owner, &count); err != nil {
		t.Fatal(err)
	}
	if owner != userB || count != 1 {
		t.Fatalf("owner = %s, rows = %d; want one row owned by the newest sign-in", owner, count)
	}
}

func TestAnotherUsersTokenAndNotificationAreNotFound(t *testing.T) {
	service, _, pool := testService(t)
	ctx := context.Background()

	if err := service.RegisterToken(ctx, userA, "a-token", "ios"); err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteToken(ctx, userB, "a-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user deleted a token: %v", err)
	}

	id, err := service.SendToUser(ctx, Notification{
		UserID: userA, EventKey: "job:owned", Title: "t", Body: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.MarkOpened(ctx, userB, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user opened it: %v", err)
	}
	if err := service.MarkOpened(ctx, userA, id); err != nil {
		t.Fatalf("the owner could not open it: %v", err)
	}
	// Opening twice is not found: already recorded.
	if err := service.MarkOpened(ctx, userA, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a second open was recorded: %v", err)
	}

	var opened *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT opened_at FROM reelpin.notifications WHERE id = $1`, id).Scan(&opened); err != nil {
		t.Fatal(err)
	}
	if opened == nil {
		t.Fatal("opened_at was not recorded")
	}
}

func TestAProviderOutageDoesNotLoseTheRecord(t *testing.T) {
	service, sender, pool := testService(t)
	ctx := context.Background()

	if err := service.RegisterToken(ctx, userA, "token-a", "ios"); err != nil {
		t.Fatal(err)
	}
	sender.fail = true

	if _, err := service.SendToUser(ctx, Notification{
		UserID: userA, EventKey: "job:outage", Title: "t", Body: "b",
	}); err == nil {
		t.Fatal("a provider outage was reported as success")
	}

	var status, reason string
	if err := pool.QueryRow(ctx, `
		SELECT status, coalesce(failure_reason, '') FROM reelpin.notifications
		WHERE event_key = 'job:outage'`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason == "" {
		t.Fatalf("status = %q reason = %q; the attempt must be recorded", status, reason)
	}
}

func TestConcurrentDeliveriesOfOneEventBuzzOnce(t *testing.T) {
	service, sender, _ := testService(t)
	ctx := context.Background()

	if err := service.RegisterToken(ctx, userA, "token-a", "ios"); err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			service.SendToUser(ctx, Notification{
				UserID: userA, EventKey: "job:race", Title: "t", Body: "b",
			})
		}()
	}
	wait.Wait()

	if sender.count() != 1 {
		t.Fatalf("the provider was called %d times for one event", sender.count())
	}
}
