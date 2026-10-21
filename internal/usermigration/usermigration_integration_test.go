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

	// No Supabase auth schema: on Neon the migrations create the mirror, and
	// nothing but the recorder puts users in it.
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

	// A user the mirror has never seen is materialised in auth.users by the
	// same write, so the record has its parent and so does everything the
	// user saves next.
	if err := second.record(ctx, unknownUser); err != nil {
		t.Fatalf("recording a user auth.users does not have yet: %v", err)
	}
	var mirrored, recorded bool
	if err := owner.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM auth.users WHERE id = $1),
		       EXISTS (SELECT 1 FROM reelpin.user_migrations WHERE user_id = $1)`, unknownUser,
	).Scan(&mirrored, &recorded); err != nil {
		t.Fatal(err)
	}
	if !mirrored || !recorded {
		t.Errorf("after the first request: mirrored %v, recorded %v; want both", mirrored, recorded)
	}

	// The two inserts commit together. With the record refused, the mirror
	// row is rolled back with it, and Seen forgets the user so the next
	// request retries.
	if _, err := owner.Exec(ctx, `REVOKE INSERT ON reelpin.user_migrations FROM reelpin_app`); err != nil {
		t.Fatal(err)
	}
	third := New(app, logger)
	err := third.record(ctx, userB)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("recording without the grant err = %v, want permission denied", err)
	}
	third.Seen(ctx, userB)
	if _, held := third.seen.Load(userB); held {
		t.Error("a failed write was remembered as seen")
	}
	if err := owner.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM auth.users WHERE id = $1)`, userB).Scan(&mirrored); err != nil {
		t.Fatal(err)
	}
	if mirrored {
		t.Error("a failed record left its mirror row behind")
	}
	if _, err := owner.Exec(ctx, `GRANT INSERT ON reelpin.user_migrations TO reelpin_app`); err != nil {
		t.Fatal(err)
	}

	// The record goes when the user does.
	if _, err := owner.Exec(ctx, `DELETE FROM auth.users WHERE id = $1`, userA); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `
		SELECT count(*) FROM reelpin.user_migrations WHERE user_id = $1`, userA).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Errorf("%d records left after the user was deleted, want 0", total)
	}

	// Account deletion leaves its request row behind, and a token issued
	// before the deletion reaching a process that never saw the user must not
	// bring them back.
	if _, err := owner.Exec(ctx, `
		INSERT INTO reelpin.account_deletion_requests (user_id, database_cleanup_state)
		VALUES ($1, 'done')`, userA); err != nil {
		t.Fatal(err)
	}
	err = New(app, logger).record(ctx, userA)
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Errorf("recording a deleted user err = %v, want the record's foreign key violation", err)
	}
	if err := owner.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM auth.users WHERE id = $1)`, userA).Scan(&mirrored); err != nil {
		t.Fatal(err)
	}
	if mirrored {
		t.Error("a user who deleted their account was materialised again")
	}
}
