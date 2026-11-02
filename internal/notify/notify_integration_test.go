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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

func TestQueuedCollectionUpdateSkipsRevokedOrDeletedCollection(t *testing.T) {
	for _, revoke := range []string{"membership", "collection"} {
		t.Run(revoke, func(t *testing.T) {
			service, sender, pool := testService(t)
			ctx := context.Background()
			if err := service.RegisterToken(ctx, userB, Device{Token: "recipient", Platform: "ios"}); err != nil {
				t.Fatal(err)
			}
			collectionID := uuid.NewString()
			if _, err := pool.Exec(ctx, `INSERT INTO reelpin.collections(id,owner_id,name) VALUES($1,$2,'Shared')`, collectionID, userA); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO reelpin.collection_members(collection_id,user_id,role) VALUES($1,$2,'viewer')`, collectionID, userB); err != nil {
				t.Fatal(err)
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err := EnqueueCollectionUpdate(ctx, tx, collectionID, userA, 1); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var id string
			if err := pool.QueryRow(ctx, `SELECT id::text FROM reelpin.notifications WHERE user_id=$1`, userB).Scan(&id); err != nil {
				t.Fatal(err)
			}
			query := `DELETE FROM reelpin.collection_members WHERE collection_id=$1`
			if revoke == "collection" {
				query = `DELETE FROM reelpin.collections WHERE id=$1`
			}
			if _, err := pool.Exec(ctx, query, collectionID); err != nil {
				t.Fatal(err)
			}
			if _, err := service.SendCollectionUpdate(ctx, id); !errors.Is(err, ErrNotFound) || sender.count() != 0 {
				t.Fatalf("revoked update sends=%d err=%v", sender.count(), err)
			}
			var status string
			if err := pool.QueryRow(ctx, `SELECT status FROM reelpin.notifications WHERE id=$1`, id).Scan(&status); err != nil || status != "failed" {
				t.Fatalf("status=%s err=%v", status, err)
			}
		})
	}
}

// recordingSender counts sends and can fail on demand.
type recordingSender struct {
	mu          sync.Mutex
	sends       int
	tokens      []string
	invalid     map[string]bool
	retryable   bool
	retryTokens map[string]bool
	fail        bool
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
		case r.retryable || r.retryTokens[token]:
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

	if err := service.RegisterToken(ctx, userA, Device{Token: "token-a", Platform: "ios"}); err != nil {
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

	if err := service.RegisterToken(ctx, userA, Device{Token: "dead-token", Platform: "android"}); err != nil {
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
	if err := service.RegisterToken(ctx, userB, Device{Token: "flaky-token", Platform: "ios"}); err != nil {
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
	if err := service.RegisterToken(ctx, userA, Device{Token: "shared-phone", Platform: "ios"}); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterToken(ctx, userB, Device{Token: "shared-phone", Platform: "ios"}); err != nil {
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

	if err := service.RegisterToken(ctx, userA, Device{Token: "a-token", Platform: "ios"}); err != nil {
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

	if err := service.RegisterToken(ctx, userA, Device{Token: "token-a", Platform: "ios"}); err != nil {
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

	if err := service.RegisterToken(ctx, userA, Device{Token: "token-a", Platform: "ios"}); err != nil {
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

// TestARegistrationRecordsItsBuild is what the forced-update rollout reads: each
// registration overwrites the build fields, so a row describes the build that
// registered it last, and the live count groups tokens by platform and build.
func TestARegistrationRecordsItsBuild(t *testing.T) {
	service, _, pool := testService(t)
	ctx := context.Background()

	version, build, zone, locale := "2.4.0", 412, "Asia/Kolkata", "en-IN"
	if err := service.RegisterToken(ctx, userA, Device{
		Token: "phone", Platform: "android",
		AppVersion: &version, AppBuild: &build, Timezone: &zone, Locale: &locale,
	}); err != nil {
		t.Fatal(err)
	}

	var gotVersion, gotZone, gotLocale *string
	var gotBuild *int
	read := func() {
		t.Helper()
		if err := pool.QueryRow(ctx, `
			SELECT app_version, app_build, timezone, locale
			FROM reelpin.device_push_tokens WHERE fcm_token = 'phone'`).Scan(
			&gotVersion, &gotBuild, &gotZone, &gotLocale); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if gotVersion == nil || *gotVersion != version || gotBuild == nil || *gotBuild != build ||
		gotZone == nil || *gotZone != zone || gotLocale == nil || *gotLocale != locale {
		t.Fatalf("stored %v %v %v %v", gotVersion, gotBuild, gotZone, gotLocale)
	}

	newer := 415
	if err := service.RegisterToken(ctx, userA, Device{Token: "phone", Platform: "android", AppBuild: &newer}); err != nil {
		t.Fatal(err)
	}
	read()
	if gotBuild == nil || *gotBuild != newer || gotVersion != nil {
		t.Fatalf("after re-registering: build %v, version %v; want 415 and the old version gone", gotBuild, gotVersion)
	}

	if err := service.RegisterToken(ctx, userB, Device{Token: "tablet", Platform: "android", AppBuild: &newer}); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterToken(ctx, userB, Device{Token: "old-iphone", Platform: "ios"}); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterToken(ctx, userB, Device{Token: "stale", Platform: "ios", AppBuild: &build}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE reelpin.device_push_tokens SET last_seen_at = now() - interval '90 days'
		WHERE fcm_token = 'stale'`); err != nil {
		t.Fatal(err)
	}

	counts, err := service.LiveBuilds(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 2 {
		t.Fatalf("counts = %+v, want android 415 and ios unreported", counts)
	}
	if counts[0].Platform != "android" || counts[0].AppBuild == nil || *counts[0].AppBuild != newer || counts[0].Tokens != 2 {
		t.Errorf("first = %+v, want two android tokens on 415", counts[0])
	}
	if counts[1].Platform != "ios" || counts[1].AppBuild != nil || counts[1].Tokens != 1 {
		t.Errorf("second = %+v, want one ios token with no build, and the stale one left out", counts[1])
	}
}

func TestFailedPushIsRetriedAndThenDeduplicated(t *testing.T) {
	service, sender, pool := testService(t)
	ctx := context.Background()
	if err := service.RegisterToken(ctx, userA, Device{Token: "phone", Platform: "android"}); err != nil {
		t.Fatal(err)
	}
	notification := ReelReady(userA, "save", "job", "Saved item", "x", "post")
	sender.fail = true
	first, err := service.SendToUser(ctx, notification)
	if err == nil {
		t.Fatal("provider failure was reported as success")
	}
	sender.fail = false
	second, err := service.SendToUser(ctx, notification)
	if err != nil || first != second || sender.count() != 2 {
		t.Fatalf("retry: first=%q second=%q sends=%d err=%v", first, second, sender.count(), err)
	}
	if _, err := service.SendToUser(ctx, notification); err != nil || sender.count() != 2 {
		t.Fatalf("successful push repeated: sends=%d err=%v", sender.count(), err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM reelpin.notifications WHERE id=$1`, first).Scan(&status); err != nil || status != "sent" {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

func TestPartialDeliveryRetriesOnlyTheUnsentDevice(t *testing.T) {
	service, sender, _ := testService(t)
	ctx := context.Background()
	for _, token := range []string{"first-phone", "second-phone"} {
		if err := service.RegisterToken(ctx, userA, Device{Token: token, Platform: "android"}); err != nil {
			t.Fatal(err)
		}
	}
	sender.retryTokens = map[string]bool{"second-phone": true}
	notification := ReelReady(userA, "save", "job", "Saved item", "x", "post")
	if _, err := service.SendToUser(ctx, notification); !errors.Is(err, ErrRetryable) {
		t.Fatalf("partial delivery was lost: %v", err)
	}
	sender.retryTokens = nil
	if _, err := service.SendToUser(ctx, notification); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, token := range sender.tokens {
		counts[token]++
	}
	if counts["first-phone"] != 1 || counts["second-phone"] != 2 {
		t.Fatalf("delivery attempts=%v; the accepted device must not be sent twice", counts)
	}
}

func TestPushRetriesHaveADurableBudget(t *testing.T) {
	service, sender, _ := testService(t)
	ctx := context.Background()
	if err := service.RegisterToken(ctx, userA, Device{Token: "phone", Platform: "android"}); err != nil {
		t.Fatal(err)
	}
	sender.retryable = true
	notification := ReelReady(userA, "save", "job", "Saved item", "x", "post")
	for range 8 {
		_, _ = service.SendToUser(ctx, notification)
	}
	if sender.count() != 5 {
		t.Fatalf("provider sends=%d, want the bounded five-attempt budget", sender.count())
	}
}

func TestPushReceiptsWorkWithOneApplicationRoleConnection(t *testing.T) {
	_, sender, owner := testService(t)
	cfg, err := pgxpool.ParseConfig(owner.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET ROLE reelpin_app`)
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	service := NewService(pool, sender, slog.New(slog.NewJSONHandler(io.Discard, nil)), time.Now)
	if err := service.RegisterToken(ctx, userA, Device{Token: "phone", Platform: "android"}); err != nil {
		t.Fatal(err)
	}
	notification := ReelReady(userA, "save", "job", "Saved item", "x", "post")
	for range 2 {
		if _, err := service.SendToUser(ctx, notification); err != nil {
			t.Fatal(err)
		}
	}
	if sender.count() != 1 {
		t.Fatalf("provider sends=%d, want one", sender.count())
	}
}
