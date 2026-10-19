//go:build integration

package website

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const userA = "11111111-1111-4111-8111-111111111111"

func testService(t *testing.T) (*Service, *pgxpool.Pool) {
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

	name := "reelpin_website_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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
	if _, err := pool.Exec(ctx, `INSERT INTO auth.users (id) VALUES ($1)`, userA); err != nil {
		t.Fatal(err)
	}
	return New(pool), pool
}

type storedFeedback struct {
	userID, email  *string
	topic, message string
}

func readFeedback(t *testing.T, pool *pgxpool.Pool, id string) storedFeedback {
	t.Helper()
	var row storedFeedback
	if err := pool.QueryRow(context.Background(), `
		SELECT user_id::text, topic, email, message
		FROM reelpin.feedback WHERE id = $1`, id,
	).Scan(&row.userID, &row.topic, &row.email, &row.message); err != nil {
		t.Fatalf("reading feedback %s: %v", id, err)
	}
	return row
}

func TestFeedbackIsStoredWithItsUserOrWithNone(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	signedIn, err := service.SubmitFeedback(ctx, userA,
		Feedback{Topic: "Support", Email: " me@example.com ", Message: "  The map is blank.  "})
	if err != nil {
		t.Fatalf("signed-in feedback: %v", err)
	}
	row := readFeedback(t, pool, signedIn)
	if row.userID == nil || *row.userID != userA {
		t.Errorf("user_id = %v, want %s", row.userID, userA)
	}
	if row.topic != "Support" || row.message != "The map is blank." {
		t.Errorf("row = %+v, want the topic and the trimmed message", row)
	}
	if row.email == nil || *row.email != "me@example.com" {
		t.Errorf("email = %v", row.email)
	}

	anonymous, err := service.SubmitFeedback(ctx, "", Feedback{Topic: "Feedback", Message: "hi"})
	if err != nil {
		t.Fatalf("anonymous feedback: %v", err)
	}
	row = readFeedback(t, pool, anonymous)
	if row.userID != nil {
		t.Errorf("user_id = %q, want NULL for an anonymous message", *row.userID)
	}
	if row.email != nil {
		t.Errorf("email = %q, want NULL when none was given", *row.email)
	}
}

func TestDeletingTheAccountDeletesItsFeedbackOnly(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	if _, err := service.SubmitFeedback(ctx, userA,
		Feedback{Topic: "Feedback", Email: "me@example.com", Message: "mine"}); err != nil {
		t.Fatal(err)
	}
	anonymous, err := service.SubmitFeedback(ctx, "", Feedback{Topic: "Feedback", Message: "anyone's"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM auth.users WHERE id = $1`, userA); err != nil {
		t.Fatalf("deleting the account: %v", err)
	}

	var ids []string
	rows, err := pool.Query(ctx, `SELECT id::text FROM reelpin.feedback`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()

	if len(ids) != 1 || ids[0] != anonymous {
		t.Fatalf("feedback left = %v, want only the anonymous message %s", ids, anonymous)
	}
}

func TestALinkClickIsStoredWithBlankFieldsAsNull(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	if err := service.RecordClick(ctx, Click{
		Target: "ios", Referer: "https://reelpin.in/", UserAgent: strings.Repeat("a", 600), Country: "IN",
	}); err != nil {
		t.Fatalf("full click: %v", err)
	}
	if err := service.RecordClick(ctx, Click{Target: "play"}); err != nil {
		t.Fatalf("bare click: %v", err)
	}

	type click struct {
		target                      string
		referer, userAgent, country *string
	}
	byTarget := map[string]click{}
	rows, err := pool.Query(ctx, `SELECT target, referer, user_agent, country FROM reelpin.link_clicks`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c click
		if err := rows.Scan(&c.target, &c.referer, &c.userAgent, &c.country); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		byTarget[c.target] = c
	}
	rows.Close()

	if len(byTarget) != 2 {
		t.Fatalf("clicks = %+v, want one per target", byTarget)
	}
	full := byTarget["ios"]
	if full.referer == nil || *full.referer != "https://reelpin.in/" || full.country == nil || *full.country != "IN" {
		t.Errorf("ios click = %+v", full)
	}
	if full.userAgent == nil || len(*full.userAgent) != maxHeaderCap {
		t.Errorf("user_agent was not truncated to %d characters", maxHeaderCap)
	}
	bare := byTarget["play"]
	if bare.referer != nil || bare.userAgent != nil || bare.country != nil {
		t.Errorf("play click = %+v, want NULL for every blank field", bare)
	}
}
