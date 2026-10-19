//go:build integration

package profiles

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

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

	name := "reelpin_profiles_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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
	return New(pool), pool
}

func text(value string) *string { return &value }

func TestUpsertCreatesTheProfileAndReturnsIt(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	created, err := service.Upsert(ctx, userA, Params{
		Email: text(" me@example.com "), FullName: text("Asha"), AvatarURL: text("https://example.com/a.png"),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if created.ID != userA {
		t.Errorf("id = %q, want the user id", created.ID)
	}
	if created.Email == nil || *created.Email != "me@example.com" {
		t.Errorf("email = %v, want it trimmed", created.Email)
	}
	if created.FullName == nil || *created.FullName != "Asha" || created.AvatarURL == nil {
		t.Errorf("profile = %+v", created)
	}
	if created.UpdatedAt.IsZero() {
		t.Error("RETURNING carried no updated_at")
	}

	read, err := service.Get(ctx, userA)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if read.ID != created.ID || *read.Email != *created.Email || !read.UpdatedAt.Equal(created.UpdatedAt) {
		t.Errorf("Get = %+v, want what Upsert returned %+v", read, created)
	}

	if _, err := service.Get(ctx, userB); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user's profile: err = %v, want ErrNotFound", err)
	}
}

func TestUpsertKeepsStoredFieldsTheRequestLeavesBlank(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	if _, err := service.Upsert(ctx, userA, Params{
		Email: text("me@example.com"), FullName: text("Asha"), AvatarURL: text("https://example.com/a.png"),
	}); err != nil {
		t.Fatal(err)
	}
	// Moved back so the second write's now() is measurably later.
	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.profiles SET updated_at = $2 WHERE user_id = $1`, userA, past); err != nil {
		t.Fatal(err)
	}

	updated, err := service.Upsert(ctx, userA, Params{
		Email: nil, FullName: text("   "), AvatarURL: text("https://example.com/b.png"),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if updated.Email == nil || *updated.Email != "me@example.com" {
		t.Errorf("email = %v; an absent field must keep the stored value", updated.Email)
	}
	if updated.FullName == nil || *updated.FullName != "Asha" {
		t.Errorf("full_name = %v; a blank field must keep the stored value", updated.FullName)
	}
	if updated.AvatarURL == nil || *updated.AvatarURL != "https://example.com/b.png" {
		t.Errorf("avatar_url = %v, want the new value", updated.AvatarURL)
	}
	if !updated.UpdatedAt.After(past) {
		t.Errorf("updated_at = %v, want later than %v", updated.UpdatedAt, past)
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.profiles`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("rows = %d, want the one upserted profile", rows)
	}
}
