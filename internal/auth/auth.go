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

// Principal is who a verified token speaks for.
type Principal struct {
	UserID string
	// ClientID is the OAuth client a Supabase OAuth-server token was issued
	// to, such as an MCP server or the browser extension. An app session token
	// carries none, and it is empty.
	ClientID string
}

type Authenticator interface {
	Authenticate(ctx context.Context, token string) (Principal, error)
}

type contextKey struct{}

type clientIDContextKey struct{}

var (
	userIDKey   contextKey
	clientIDKey clientIDContextKey
)

// WithClientID carries the token's OAuth client, when it has one.
func WithClientID(ctx context.Context, clientID string) context.Context {
	return context.WithValue(ctx, clientIDKey, clientID)
}

// ClientID is the OAuth client the request's token was issued to, or empty
// for an app session.
func ClientID(ctx context.Context) string {
	clientID, _ := ctx.Value(clientIDKey).(string)
	return clientID
}

func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey, userID)
}

// UserID returns the authenticated user id placed on the context by the auth
// middleware. A request never carries a user id from its query or body.
func UserID(ctx context.Context) (string, bool) {
	userID, ok := ctx.Value(userIDKey).(string)
	return userID, ok && userID != ""
}
