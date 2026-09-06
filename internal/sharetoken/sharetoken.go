// Package sharetoken issues and checks the long-lived tokens the native share
// extensions present. The app's Supabase session is short-lived and often
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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUnknownToken covers an unknown, malformed, expired or revoked token. The
// client is told the same thing in every case.
var ErrUnknownToken = errors.New("share token is unknown or revoked")

// Lifetime is deliberately long: the token outlives app sessions by design and
// is revoked explicitly on sign-out or device loss.
const Lifetime = 365 * 24 * time.Hour

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Hash is the only form of a token that is ever written down.
func Hash(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}

// Mint returns the raw token once and stores its hash with its expiry.
func (s *Store) Mint(ctx context.Context, userID string) (token string, expiresAt time.Time, err error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", time.Time{}, fmt.Errorf("generating a share token: %w", err)
	}
	raw := base64.RawURLEncoding.EncodeToString(buffer)

	err = s.pool.QueryRow(ctx, `
		INSERT INTO reelpin.native_share_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, now() + $3::interval)
		RETURNING expires_at`,
		userID, Hash(raw), Lifetime.String(),
	).Scan(&expiresAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("storing the share token: %w", err)
	}
	return raw, expiresAt, nil
}

// Authenticate exchanges a presented token for its user. Unknown, expired and
// revoked all answer the same error.
func (s *Store) Authenticate(ctx context.Context, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", ErrUnknownToken
	}

	var userID string
	err := s.pool.QueryRow(ctx, `
		SELECT user_id::text FROM reelpin.native_share_tokens
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()`,
		Hash(raw),
	).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnknownToken
	}
	if err != nil {
		return "", fmt.Errorf("checking the share token: %w", err)
	}
	return userID, nil
}

// RevokeAll revokes every live token this user holds and reports how many.
func (s *Store) RevokeAll(ctx context.Context, userID string) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE reelpin.native_share_tokens
		SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL`,
		userID,
	)
	if err != nil {
		return 0, fmt.Errorf("revoking share tokens: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
