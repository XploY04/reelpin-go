package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/lifecycle"
)

// Lifecycle is what the API needs to remove things.
type Lifecycle interface {
	DeleteReel(ctx context.Context, userID, reelID string) error
	DeleteAccount(ctx context.Context, userID string) (lifecycle.DeleteAccountReport, error)
}

type reelDeletedResponse struct {
	Message string `json:"message"`
	ID      string `json:"id"`
}

type accountDeletedResponse struct {
	Deleted bool   `json:"deleted"`
	Message string `json:"message"`
	// AppleNote is the honest limitation: Apple sign-in tokens are not stored,
	// so the Apple-side authorization cannot be revoked from here.
	AppleNote string `json:"apple_note,omitempty"`
}

func (s *Server) handleDeleteReel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "reel_id", "reel_not_found")
	if !ok {
		return
	}

	err := s.deps.Lifecycle.DeleteReel(r.Context(), requestUserID(r), id.String())
	if errors.Is(err, lifecycle.ErrNotFound) {
		notFoundError(w, "reel_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("deleting a reel failed", "error", err)
		internalError(w, "reel_delete_failed", "Could not delete the reel right now.")
		return
	}
	writeJSON(w, http.StatusOK, reelDeletedResponse{Message: "Reel deleted", ID: id.String()})
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	userID := requestUserID(r)

	report, err := s.deps.Lifecycle.DeleteAccount(r.Context(), userID)
	if err != nil {
		// A partial delete is reported as a failure so the client retries. The
		// operation is safe to repeat.
		s.deps.Logger.Error("deleting an account failed", "error", err)
		internalError(w, "account_delete_failed", "Could not finish deleting the account. Please try again.")
		return
	}

	s.deps.Logger.Info("account deleted",
		"reels", report.Reels, "collections", report.Collections,
		"jobs", report.ProcessingJobs, "auth_user_deleted", report.AuthUserDeleted)

	writeJSON(w, http.StatusOK, accountDeletedResponse{
		Deleted: true,
		Message: "Account deleted.",
		AppleNote: "Sign in with Apple must also be revoked in your Apple ID settings, " +
			"because ReelPin does not store an Apple authorization token.",
	})
}
