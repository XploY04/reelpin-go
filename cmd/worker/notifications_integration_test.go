//go:build integration

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/XploY04/reelpin-go/internal/notify"
	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type pushSender struct {
	messages []notify.Message
	fail     bool
}

func (s *pushSender) Send(_ context.Context, tokens []string, message notify.Message) ([]notify.Delivery, error) {
	s.messages = append(s.messages, message)
	if s.fail {
		return nil, errors.New("provider failure with private-token")
	}
	deliveries := make([]notify.Delivery, 0, len(tokens))
	for _, token := range tokens {
		deliveries = append(deliveries, notify.Delivery{Token: token, MessageID: "accepted"})
	}
	return deliveries, nil
}

func notificationFixture(t *testing.T) (queue.Handler, *pushSender, queue.Message) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := "push_worker_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), `DROP DATABASE `+name+` WITH (FORCE)`); err != nil {
			t.Error(err)
		}
	})
	user, content, save, run, job := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, query := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO auth.users(id) VALUES($1)`, []any{user}},
		{`INSERT INTO reelpin.contents(id,source_platform,source_content_type,normalized_url,normalized_url_hash,access_scope_hash) VALUES($1,'x','post','https://example.com/post','hash','public')`, []any{content}},
		{`INSERT INTO reelpin.user_saves(id,user_id,content_id) VALUES($1,$2,$3)`, []any{save, user, content}},
		{`INSERT INTO reelpin.processing_runs(id,content_id,processor_version,status) VALUES($1,$2,'go-v1','completed')`, []any{run, content}},
		{`INSERT INTO reelpin.processing_jobs(id,user_id,run_id,user_save_id,url,normalized_url,status) VALUES($1,$2,$3,$4,'https://example.com/post','https://example.com/post','completed')`, []any{job, user, run, save}},
	} {
		if _, err := pool.Exec(ctx, query.sql, query.args...); err != nil {
			t.Fatal(err)
		}
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	sender := &pushSender{}
	service := notify.NewService(pool, sender, logger, time.Now)
	if err := service.RegisterToken(ctx, user, notify.Device{Token: "phone", Platform: "android"}); err != nil {
		t.Fatal(err)
	}
	return notificationHandler(pool, service, logger), sender, queue.Message{RunID: run}
}

func TestCompletedJobUsesTheAppNotificationContract(t *testing.T) {
	handle, sender, message := notificationFixture(t)
	outcome, err := handle(context.Background(), message)
	if err != nil || outcome.Kind != queue.Done || len(sender.messages) != 1 {
		t.Fatalf("outcome=%v sends=%d err=%v", outcome, len(sender.messages), err)
	}
	data := sender.messages[0].Data
	if data["schema_version"] != "1" || data["type"] != "reel_ready" || data["target"] != "reel_detail" || data["notification_id"] == "" || data["reel_id"] == "" || data["job_id"] == "" {
		t.Fatalf("payload does not satisfy the app contract: %v", data)
	}
}

func TestWorkerRetriesPushWithoutRepeatingACompletedSend(t *testing.T) {
	handle, sender, message := notificationFixture(t)
	sender.fail = true
	for attempt := 1; attempt <= 2; attempt++ {
		outcome, err := handle(context.Background(), message)
		if outcome.Kind != queue.Retry || outcome.Attempt != attempt || err == nil || strings.Contains(err.Error(), "private-token") {
			t.Fatalf("attempt=%d outcome=%v err=%v", attempt, outcome, err)
		}
	}
	sender.fail = false
	for range 2 {
		outcome, err := handle(context.Background(), message)
		if outcome.Kind != queue.Done || err != nil {
			t.Fatalf("outcome=%v err=%v", outcome, err)
		}
	}
	if len(sender.messages) != 3 {
		t.Fatalf("sends=%d, want two failed and one accepted", len(sender.messages))
	}
}
