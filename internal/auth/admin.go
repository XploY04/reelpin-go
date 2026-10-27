package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const adminRequestTimeout = 10 * time.Second

// IdentityDeleter removes the identity itself from the system that owns it.
// It is declared here as well as in the package that consumes it so that
// "nothing is configured" can be a nil interface rather than a nil *Admin
// inside a non-nil one, which a caller cannot tell apart from a working
// deleter and would only discover by panicking mid-deletion.
type IdentityDeleter interface {
	DeleteUser(ctx context.Context, userID string) error
}

// Admin calls the Supabase Admin API with the project's service-role key. That
// key bypasses row-level security entirely, so this type does exactly one
// thing with it and builds its URL from configuration, never from a request.
type Admin struct {
	baseURL string
	key     string
	client  *http.Client
}

// NewAdmin returns nil when no service-role key is configured, which is the
// state an account deletion already knows how to report: the data goes, the
// sign-in stays, and the request stays pending until a key is set.
//
// internal/safehttp is deliberately not used: it exists to guard URLs a user
// supplied, and it speaks only GET. This URL is the project's own and the call
// is a DELETE, so it is the stdlib client with a timeout.
func NewAdmin(supabaseURL, serviceRoleKey string) IdentityDeleter {
	key := strings.TrimSpace(serviceRoleKey)
	if key == "" {
		return nil
	}
	return &Admin{
		baseURL: strings.TrimSuffix(strings.TrimSpace(supabaseURL), "/"),
		key:     key,
		client:  &http.Client{Timeout: adminRequestTimeout},
	}
}

// DeleteUser removes the identity.
//
// It runs after the data deletion has already committed, and it cannot join
// that transaction. Ordering it last is what keeps the two halves recoverable:
// a failure here loses nothing, because the rows are already gone and the
// request row still says the identity half is outstanding, so the retry is
// free. The other order would leave a signed-out user whose rows are still
// there and nothing left to look them up by. That retry is also why an
// identity that is already gone counts as success.
func (a *Admin) DeleteUser(ctx context.Context, userID string) error {
	// The id goes into a URL path, so it is checked before it is concatenated,
	// and it stays out of the error: the deletion request row already has it.
	if _, err := uuid.Parse(userID); err != nil {
		return errors.New("supabase admin: the user id is not a uuid")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		a.baseURL+"/auth/v1/admin/users/"+userID, nil)
	if err != nil {
		return fmt.Errorf("supabase admin: building the request: %w", err)
	}
	req.Header.Set("apikey", a.key)
	req.Header.Set("Authorization", "Bearer "+a.key)

	response, err := a.client.Do(req)
	if err != nil {
		// The *url.Error wrapper carries the URL, and the URL carries the user
		// id, so only the reason survives into the log.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("supabase admin: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		response.Body.Close()
	}()

	switch {
	case response.StatusCode == http.StatusNotFound:
		return nil
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return nil
	default:
		// The body is not quoted: a provider error can echo the request, and
		// this request carries the service-role key.
		return fmt.Errorf("supabase admin: deleting the identity answered %d", response.StatusCode)
	}
}
