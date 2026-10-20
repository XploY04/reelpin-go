//go:build integration

package usermigration

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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const unknownUser = "33333333-3333-4333-8333-333333333333"

// appPool migrates a scratch database as the owner and returns a pool whose
// every connection runs as reelpin_app, the role the API writes with.
func appPool(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool) {
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

	name := "reelpin_usermigration_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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

	owner, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connecting to %s: %v", name, err)
	}
	config, err := pgxpool.ParseConfig(parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET ROLE reelpin_app`)
		return err
	}
	t.Cleanup(func() {
		owner.Close()
		cleanup, err := pgxpool.New(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})

	if _, err := owner.Exec(ctx, `
		CREATE SCHEMA auth;
		CREATE TABLE auth.users (id UUID PRIMARY KEY, email TEXT, created_at TIMESTAMPTZ DEFAULT now())`); err != nil {
		t.Fatalf("creating auth.users: %v", err)
	}
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO auth.users (id) VALUES ($1)`, userA); err != nil {
		t.Fatal(err)
	}

	app, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connecting as reelpin_app: %v", err)
	}
	t.Cleanup(app.Close)
	return app, owner
}

func TestTheRecordIsWrittenOnceAsTheAppRole(t *testing.T) {
	app, owner := appPool(t)
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	var role string
	if err := app.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil || role != "reelpin_app" {
		t.Fatalf("the pool runs as %q (%v), want reelpin_app", role, err)
	}

	first := New(app, logger)
	if err := first.record(ctx, userA); err != nil {
		t.Fatalf("recording a user as reelpin_app: %v", err)
	}
	var firstSeen time.Time
	if err := owner.QueryRow(ctx, `
		SELECT first_seen_at FROM reelpin.user_migrations WHERE user_id = $1`, userA).Scan(&firstSeen); err != nil {
		t.Fatalf("reading the record: %v", err)
	}

	// Another process, which has not seen the user, writes again. The
	// statement is a no-op, not an error, and the first sighting stands.
	second := New(app, logger)
	if err := second.record(ctx, userA); err != nil {
		t.Fatalf("recording the same user again: %v", err)
	}
	var total int
	var stillSeen time.Time
	if err := owner.QueryRow(ctx, `
		SELECT count(*), min(first_seen_at) FROM reelpin.user_migrations`).Scan(&total, &stillSeen); err != nil {
		t.Fatal(err)
	}
	if total != 1 || !stillSeen.Equal(firstSeen) {
		t.Errorf("after a second insert: %d rows, first_seen_at %v, want 1 row at %v", total, stillSeen, firstSeen)
	}

	// The foreign key holds for the app role too: a user auth does not know
	// is refused, and Seen forgets it so the next request retries.
	err := second.record(ctx, unknownUser)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Errorf("recording an unknown user err = %v, want a foreign key violation", err)
	}
	second.Seen(ctx, unknownUser)
	if _, held := second.seen.Load(unknownUser); held {
		t.Error("a failed write was remembered as seen")
	}

	// The record goes when the user does.
	if _, err := owner.Exec(ctx, `DELETE FROM auth.users WHERE id = $1`, userA); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM reelpin.user_migrations`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Errorf("%d records left after the user was deleted, want 0", total)
	}
}
