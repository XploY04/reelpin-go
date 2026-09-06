package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/XploY04/reelpin-go/internal/auth"
	"github.com/XploY04/reelpin-go/internal/sharetoken"
)

// ShareTokenStore is the credential store behind the native-share mode.
type ShareTokenStore interface {
	Mint(ctx context.Context, userID string) (string, time.Time, error)
	Authenticate(ctx context.Context, raw string) (string, error)
	RevokeAll(ctx context.Context, userID string) (int, error)
}

// shareTokenAuthenticated guards the one endpoint a native share extension
// calls. The extension runs outside the app process and cannot refresh a
// Supabase session, so it presents a long-lived token instead.
//
// The token is never a substitute for a session anywhere else: only this mode
// accepts it, and it resolves to exactly one user.
func (s *Server) shareTokenAuthenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Share-Token")
		if token == "" {
			writeError(w, http.StatusUnauthorized, errorBody{
				Code:    "share_token_required",
				Message: "This endpoint requires a share token.",
			})
			return
		}

		userID, err := s.deps.ShareTokens.Authenticate(r.Context(), token)
		if errors.Is(err, sharetoken.ErrUnknownToken) {
			// Unknown, expired and revoked all answer identically: a probe
			// learns nothing about which tokens exist.
			writeError(w, http.StatusUnauthorized, errorBody{
				Code:    "invalid_share_token",
				Message: "This share token is not valid. Open the app to reconnect sharing.",
			})
			return
		}
		if err != nil {
			s.deps.Logger.Error("share token check failed", "error", err)
			internalError(w, "internal_error", "The server could not finish this request.")
			return
		}

		next(w, r.WithContext(auth.WithUserID(r.Context(), userID)))
	}
}
