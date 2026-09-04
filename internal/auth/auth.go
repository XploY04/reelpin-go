// Package auth verifies Supabase access tokens against the project's JWKS and
// carries the resulting user id on the request context.
package auth

import (
	"context"
	"errors"
)

// ErrUnauthenticated covers every rejected token. Callers must not tell the
// reasons apart in a response.
var ErrUnauthenticated = errors.New("token rejected")

type Authenticator interface {
	Authenticate(ctx context.Context, token string) (string, error)
}

type contextKey struct{}

var userIDKey contextKey

func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey, userID)
}

// UserID returns the authenticated user id placed on the context by the auth
// middleware. A request never carries a user id from its query or body.
func UserID(ctx context.Context) (string, bool) {
	userID, ok := ctx.Value(userIDKey).(string)
	return userID, ok && userID != ""
}
