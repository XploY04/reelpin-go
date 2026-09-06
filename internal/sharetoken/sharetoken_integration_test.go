//go:build integration

package sharetoken

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const userA = "11111111-1111-4111-8111-111111111111"

func testStore(t *testing.T) (*Store, *pgxpool.Pool) {
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

	name := "reelpin_sharetoken_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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
	return NewStore(pool), pool
}

func TestAMintedTokenAuthenticatesItsUser(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()

	raw, expiresAt, err := store.Mint(ctx, userA)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if raw == "" || expiresAt.IsZero() {
		t.Fatal("mint returned nothing")
	}

	userID, err := store.Authenticate(ctx, raw)
	if err != nil || userID != userA {
		t.Fatalf("authenticate = %q, %v", userID, err)
	}

	// Only the hash is stored: the raw value appears nowhere in the database.
	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT token_hash FROM reelpin.native_share_tokens`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == raw || strings.Contains(stored, raw) {
		t.Fatal("the raw token was written down")
	}
}

func TestUnknownExpiredAndRevokedAllAnswerTheSame(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()

	// Unknown.
	if _, err := store.Authenticate(ctx, "never-minted"); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("unknown err = %v", err)
	}

	// Expired.
	expired, _, err := store.Mint(ctx, userA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE reelpin.native_share_tokens SET expires_at = now() - interval '1 second'
		WHERE token_hash = $1`, Hash(expired)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(ctx, expired); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("expired err = %v", err)
	}

	// Revoked.
	revoked, _, err := store.Mint(ctx, userA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RevokeAll(ctx, userA); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(ctx, revoked); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("revoked err = %v", err)
	}
}

func TestRevokeAllCountsOnlyLiveTokens(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()

	if _, _, err := store.Mint(ctx, userA); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Mint(ctx, userA); err != nil {
		t.Fatal(err)
	}

	revoked, err := store.RevokeAll(ctx, userA)
	if err != nil || revoked != 2 {
		t.Fatalf("revoked = %d, %v", revoked, err)
	}
	// A second revoke has nothing left to do.
	revoked, err = store.RevokeAll(ctx, userA)
	if err != nil || revoked != 0 {
		t.Fatalf("second revoke = %d, %v", revoked, err)
	}
}
