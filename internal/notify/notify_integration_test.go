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

const legacySchema = `
CREATE TABLE public.device_push_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    fcm_token TEXT NOT NULL,
    platform TEXT DEFAULT '',
    last_seen_at TIMESTAMPTZ DEFAULT now(),
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE public.notification_campaigns (
    campaign_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    target TEXT NOT NULL DEFAULT 'announcement',
    announcement_id TEXT NOT NULL DEFAULT '',
    minimum_supported_app_build INTEGER,
    scheduled_at TIMESTAMPTZ,
    status TEXT NOT NULL DEFAULT 'draft',
    audience_filters JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);
CREATE TABLE public.notifications (
    notification_id UUID PRIMARY KEY,
    event_key TEXT NOT NULL,
    user_id TEXT NOT NULL,
    type TEXT NOT NULL,
    target TEXT NOT NULL,
    target_id TEXT NOT NULL,
    campaign_id UUID REFERENCES public.notification_campaigns(campaign_id) ON DELETE SET NULL,
    status TEXT NOT NULL DEFAULT 'queued',
    fcm_message_id TEXT,
    failure_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at TIMESTAMPTZ,
    opened_at TIMESTAMPTZ
);
`

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

// recordingSender stands in for the provider. No test sends a push.
type recordingSender struct {
	mu        sync.Mutex
	messages  []Message
	tokens    [][]string
	invalid   map[string]bool
	retryable bool
	err       error
}

func (r *recordingSender) Send(_ context.Context, tokens []string, message Message) ([]Delivery, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens = append(r.tokens, tokens)
	r.messages = append(r.messages, message)
	if r.err != nil {
		return nil, r.err
	}

	deliveries := make([]Delivery, 0, len(tokens))
	for index, token := range tokens {
		switch {
		case r.invalid[token]:
			deliveries = append(deliveries, Delivery{Token: token, Invalid: true})
		case r.retryable:
			deliveries = append(deliveries, Delivery{Token: token, Retryable: true})
		default:
			deliveries = append(deliveries, Delivery{Token: token, MessageID: "message-" + string(rune('a'+index))})
		}
	}
	return deliveries, nil
}

func (r *recordingSender) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.messages)
}

func testService(t *testing.T) (*Service, *pgxpool.Pool, *recordingSender) {
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

	parsed, _ := url.Parse(adminURL)
	parsed.Path = "/" + name
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connecting: %v", err)
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

	if _, err := pool.Exec(ctx, legacySchema); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	sender := &recordingSender{invalid: map[string]bool{}}
	return NewService(pool, sender, slog.New(slog.NewJSONHandler(io.Discard, nil)), time.Now), pool, sender
}

func TestRegisteringADeviceIsIdempotentAndFollowsTheNewestSignIn(t *testing.T) {
	service, pool, _ := testService(t)
	ctx := context.Background()

	if err := service.RegisterToken(ctx, userA, "token-1", "ios", "1.0", nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := service.RegisterToken(ctx, userA, "token-1", "ios", "1.1", nil); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	var count int
	pool.QueryRow(ctx, `SELECT count(*) FROM public.device_push_tokens`).Scan(&count)
	if count != 1 {
		t.Fatalf("rows = %d, want one per device", count)
	}

	// A shared phone: the newest sign-in owns the device, so the previous
	// account stops receiving notifications on it.
	if err := service.RegisterToken(ctx, userB, "token-1", "ios", "1.1", nil); err != nil {
		t.Fatalf("register for another user: %v", err)
	}
	var owner string
	pool.QueryRow(ctx, `SELECT user_id FROM public.device_push_tokens`).Scan(&owner)
	if owner != userB {
		t.Errorf("device owner = %q, want the newest sign-in", owner)
	}
}

func TestSendingIsIdempotentPerEvent(t *testing.T) {
	service, pool, sender := testService(t)
	ctx := context.Background()
	service.RegisterToken(ctx, userA, "token-1", "ios", "", nil)

	notification := ReelReady(userA, "reel-1", "job-1", "Best cafes", "instagram", "reel")

	first, err := service.SendToUser(ctx, notification)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	second, err := service.SendToUser(ctx, notification)
	if err != nil {
		t.Fatalf("second send: %v", err)
	}
	if first != second {
		t.Fatalf("two notifications for one event: %s and %s", first, second)
	}
	if sender.calls() != 1 {
		t.Fatalf("the provider was called %d times for one event", sender.calls())
	}

	var status, messageID string
	pool.QueryRow(ctx, `SELECT status, fcm_message_id FROM public.notifications`).Scan(&status, &messageID)
	if status != "sent_to_fcm" || messageID == "" {
		t.Errorf("status = %s, message id = %q", status, messageID)
	}

	// The routing fields the tap handler reads must reach the device.
	data := sender.messages[0].Data
	for _, key := range []string{"schema_version", "type", "target", "reel_id", "job_id", "notification_id"} {
		if data[key] == "" {
			t.Errorf("data is missing %q", key)
		}
	}
}

func TestNoDeviceLeavesTheNotificationQueuedForRetry(t *testing.T) {
	service, pool, sender := testService(t)
	ctx := context.Background()

	notificationID, err := service.SendToUser(ctx, ReelReady(userA, "reel-1", "job-1", "", "", ""))
	if !errors.Is(err, ErrNoDeviceTokens) {
		t.Fatalf("err = %v, want ErrNoDeviceTokens", err)
	}
	if sender.calls() != 0 {
		t.Error("the provider was called with no tokens")
	}

	var status string
	pool.QueryRow(ctx, `SELECT status FROM public.notifications WHERE notification_id = $1`, notificationID).Scan(&status)
	if status != "queued" {
		t.Fatalf("status = %s, want it left queued so a retry can send it", status)
	}

	// The device registers a moment later, and the retry goes through.
	service.RegisterToken(ctx, userA, "token-1", "ios", "", nil)
	retried, err := service.SendToUser(ctx, ReelReady(userA, "reel-1", "job-1", "", "", ""))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retried != notificationID {
		t.Errorf("the retry created a second notification")
	}
	if sender.calls() != 1 {
		t.Fatalf("the provider was called %d times", sender.calls())
	}
}

func TestInvalidTokensAreRemoved(t *testing.T) {
	service, pool, sender := testService(t)
	ctx := context.Background()

	service.RegisterToken(ctx, userA, "dead-token", "ios", "", nil)
	service.RegisterToken(ctx, userA, "live-token", "android", "", nil)
	sender.invalid["dead-token"] = true

	if _, err := service.SendToUser(ctx, ReelReady(userA, "reel-1", "job-1", "", "", "")); err != nil {
		t.Fatalf("send: %v", err)
	}

	var remaining []string
	rows, _ := pool.Query(ctx, `SELECT fcm_token FROM public.device_push_tokens`)
	for rows.Next() {
		var token string
		rows.Scan(&token)
		remaining = append(remaining, token)
	}
	rows.Close()

	if len(remaining) != 1 || remaining[0] != "live-token" {
		t.Fatalf("tokens = %v, want the rejected one removed", remaining)
	}
}

func TestATransientProviderFailureIsReported(t *testing.T) {
	service, pool, sender := testService(t)
	ctx := context.Background()
	service.RegisterToken(ctx, userA, "token-1", "ios", "", nil)
	sender.retryable = true

	if _, err := service.SendToUser(ctx, ReelReady(userA, "reel-1", "job-1", "", "", "")); err == nil {
		t.Fatal("a transient failure was reported as success")
	}

	var status, reason string
	pool.QueryRow(ctx, `SELECT status, COALESCE(failure_reason, '') FROM public.notifications`).Scan(&status, &reason)
	if status != "failed" || reason == "" {
		t.Errorf("status = %s, reason = %q", status, reason)
	}
	// The token must not leak into the failure reason.
	if strings.Contains(reason, "token-1") {
		t.Error("the failure reason carries a device token")
	}
}

func TestMarkOpenedIsScopedToTheUser(t *testing.T) {
	service, _, _ := testService(t)
	ctx := context.Background()
	service.RegisterToken(ctx, userA, "token-1", "ios", "", nil)

	notificationID, err := service.SendToUser(ctx, ReelReady(userA, "reel-1", "job-1", "", "", ""))
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	if err := service.MarkOpened(ctx, userB, notificationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user marked it opened: %v", err)
	}
	if err := service.MarkOpened(ctx, userA, notificationID); err != nil {
		t.Fatalf("MarkOpened: %v", err)
	}
	// Opening twice is not an error.
	if err := service.MarkOpened(ctx, userA, notificationID); err != nil {
		t.Fatalf("second MarkOpened: %v", err)
	}
}

func TestCampaignSendsOncePerRecipientAndIsResumable(t *testing.T) {
	service, pool, sender := testService(t)
	ctx := context.Background()

	service.RegisterToken(ctx, userA, "token-a", "ios", "", nil)
	service.RegisterToken(ctx, userB, "token-b", "android", "", nil)

	campaign, err := service.CreateCampaign(ctx, Campaign{Title: "New in ReelPin", Body: "Have a look"}, false)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if campaign.Status != StatusDraft {
		t.Fatalf("status = %s, want draft", campaign.Status)
	}

	sent, err := service.SendCampaign(ctx, campaign.CampaignID)
	if err != nil {
		t.Fatalf("SendCampaign: %v", err)
	}
	if sent.Status != StatusCompleted || sent.SentCount != 2 || sent.RecipientCount != 2 {
		t.Fatalf("campaign = %+v", sent)
	}

	// Running it again must not notify anyone twice.
	before := sender.calls()
	if _, err := service.SendCampaign(ctx, campaign.CampaignID); err == nil {
		t.Fatal("a completed campaign was sent again")
	}
	if sender.calls() != before {
		t.Errorf("a completed campaign called the provider again")
	}

	var notifications int
	pool.QueryRow(ctx, `SELECT count(*) FROM public.notifications`).Scan(&notifications)
	if notifications != 2 {
		t.Fatalf("notifications = %d, want one per recipient", notifications)
	}
}

func TestCampaignAudienceFilters(t *testing.T) {
	service, _, _ := testService(t)
	ctx := context.Background()

	service.RegisterToken(ctx, userA, "token-a", "ios", "", nil)
	service.RegisterToken(ctx, userB, "token-b", "android", "", nil)

	campaign, err := service.CreateCampaign(ctx, Campaign{
		Title: "iOS only", Body: "Hello",
		AudienceFilters: AudienceFilters{Platforms: []string{"ios"}},
	}, true)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if campaign.RecipientCount != 1 || campaign.SentCount != 1 {
		t.Fatalf("campaign = %+v, want just the ios device", campaign)
	}

	excluded, err := service.CreateCampaign(ctx, Campaign{
		Title: "Everyone but A", Body: "Hello",
		AudienceFilters: AudienceFilters{ExcludeUserIDs: []string{userA}},
	}, true)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if excluded.RecipientCount != 1 {
		t.Fatalf("recipients = %d, want the excluded user left out", excluded.RecipientCount)
	}
}

func TestCancellingOnlyWorksBeforeSending(t *testing.T) {
	service, _, _ := testService(t)
	ctx := context.Background()

	campaign, err := service.CreateCampaign(ctx, Campaign{Title: "Draft", Body: "Body"}, false)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	cancelled, err := service.CancelCampaign(ctx, campaign.CampaignID)
	if err != nil {
		t.Fatalf("CancelCampaign: %v", err)
	}
	if cancelled.Status != StatusCancelled {
		t.Fatalf("status = %s", cancelled.Status)
	}

	// A cancelled campaign cannot be sent, and cannot be cancelled again.
	if _, err := service.SendCampaign(ctx, campaign.CampaignID); err == nil {
		t.Error("a cancelled campaign was sent")
	}
	if _, err := service.CancelCampaign(ctx, campaign.CampaignID); err == nil {
		t.Error("a cancelled campaign was cancelled again")
	}
}

func TestCampaignWithNoTitleIsRefused(t *testing.T) {
	service, _, _ := testService(t)
	if _, err := service.CreateCampaign(context.Background(), Campaign{Body: "Body"}, false); err == nil {
		t.Fatal("a campaign with no title was created")
	}
}

func TestMissingCampaignIsNotFound(t *testing.T) {
	service, _, _ := testService(t)
	if _, err := service.GetCampaign(context.Background(), "99999999-9999-4999-8999-999999999999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
