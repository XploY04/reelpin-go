//go:build integration

package sharetoken

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const tokenTable = `
CREATE TABLE public.device_share_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    token_hash TEXT NOT NULL,
    platform TEXT DEFAULT '',
    revoked BOOLEAN NOT NULL DEFAULT FALSE,
    last_used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT now()
);
`

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

	if _, err := pool.Exec(ctx, tokenTable); err != nil {
		t.Fatalf("creating the token table: %v", err)
	}
	return NewStore(pool), pool
}

func TestMintedTokenResolvesToItsOwner(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()

	raw, err := store.Mint(ctx, "user-1", "ios")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(raw) < 32 {
		t.Fatalf("token is only %d characters", len(raw))
	}

	// Only the hash is ever written down.
	var stored string
	if err := pool.QueryRow(ctx, `SELECT token_hash FROM public.device_share_tokens`).Scan(&stored); err != nil {
		t.Fatalf("reading the row: %v", err)
	}
	if stored == raw {
		t.Fatal("the raw token was stored")
	}
	if stored != Hash(raw) {
		t.Fatalf("stored hash does not match")
	}

	userID, err := store.UserID(ctx, raw)
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if userID != "user-1" {
		t.Errorf("user = %q, want user-1", userID)
	}

	var lastUsed *string
	pool.QueryRow(ctx, `SELECT last_used_at::text FROM public.device_share_tokens`).Scan(&lastUsed)
	if lastUsed == nil {
		t.Error("using a token did not record the use")
	}
}

func TestUnknownAndRevokedTokensAreIndistinguishable(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()

	raw, err := store.Mint(ctx, "user-1", "android")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	revoked, err := store.RevokeAll(ctx, "user-1")
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("revoked %d tokens, want 1", revoked)
	}

	for _, candidate := range []string{raw, "never-existed", ""} {
		if _, err := store.UserID(ctx, candidate); !errors.Is(err, ErrUnknownToken) {
			t.Errorf("token %q resolved with %v, want ErrUnknownToken", candidate, err)
		}
	}

	// Revoking twice is not an error and does not revoke again.
	if again, err := store.RevokeAll(ctx, "user-1"); err != nil || again != 0 {
		t.Errorf("second revoke = %d (%v), want 0", again, err)
	}
}

func TestRevokeOnlyTouchesOneUser(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()

	mine, err := store.Mint(ctx, "user-1", "ios")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := store.Mint(ctx, "user-2", "ios")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.RevokeAll(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UserID(ctx, mine); !errors.Is(err, ErrUnknownToken) {
		t.Error("the signed-out user's token still works")
	}
	if userID, err := store.UserID(ctx, theirs); err != nil || userID != "user-2" {
		t.Errorf("another user's token was revoked: %v", err)
	}
}
