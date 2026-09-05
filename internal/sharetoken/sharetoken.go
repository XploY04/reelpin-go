// Package sharetoken issues and checks the long-lived device tokens the native
// share extensions use. The app's Supabase session is short lived and often
// absent when a share arrives in the background, so the device holds one of
// these instead. Only the hash is stored: a database leak cannot be replayed.
package sharetoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUnknownToken covers an unknown, malformed or revoked token. The client is
// told the same thing in every case.
var ErrUnknownToken = errors.New("share token is unknown or revoked")

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Hash is the only form of a token that is ever written down. It matches the
// Python implementation, so tokens already on devices keep working.
func Hash(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}

// Mint returns the raw token once and stores its hash.
func (s *Store) Mint(ctx context.Context, userID, platform string) (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generating a share token: %w", err)
	}
	raw := base64.RawURLEncoding.EncodeToString(buffer)

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO public.device_share_tokens (user_id, token_hash, platform)
		VALUES ($1, $2, $3)`,
		userID, Hash(raw), platform,
	); err != nil {
		return "", fmt.Errorf("storing a share token: %w", err)
	}
	return raw, nil
}

// UserID resolves a raw token to its owner and records the use. An unknown or
// revoked token is indistinguishable to the caller.
func (s *Store) UserID(ctx context.Context, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", ErrUnknownToken
	}

	var userID string
	err := s.pool.QueryRow(ctx, `
		UPDATE public.device_share_tokens
		SET last_used_at = now()
		WHERE token_hash = $1 AND revoked = false
		RETURNING user_id`,
		Hash(raw),
	).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnknownToken
	}
	if err != nil {
		return "", fmt.Errorf("reading a share token: %w", err)
	}
	return userID, nil
}

// RevokeAll is what sign-out calls. Tokens are marked revoked rather than
// deleted, so a token that reappears is refused instead of being unknown.
func (s *Store) RevokeAll(ctx context.Context, userID string) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE public.device_share_tokens SET revoked = true WHERE user_id = $1 AND revoked = false`,
		userID,
	)
	if err != nil {
		return 0, fmt.Errorf("revoking share tokens: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
