package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/lifecycle"
)

// Lifecycle is deletion, from the API's point of view.
type Lifecycle interface {
	DeleteReel(ctx context.Context, userID, reelID string) error
	DeleteAccount(ctx context.Context, userID string) (lifecycle.Report, error)
}

func (s *Server) handleDeleteReel(w http.ResponseWriter, r *http.Request) {
	err := s.deps.Lifecycle.DeleteReel(r.Context(), requestUserID(r), r.PathValue("reel_id"))
	switch {
	case err == nil:
		// Nothing to say: the reel is gone and the client already knows which.
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, lifecycle.ErrNotFound):
		notFoundError(w, "reel_not_found")
	case errors.Is(err, lifecycle.ErrDeletionPending):
		s.deletionPending(w)
	default:
		s.deps.Logger.Error("deleting a reel failed", "error", err)
		internalError(w, "reel_delete_failed", "Could not delete the reel right now.")
	}
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	report, err := s.deps.Lifecycle.DeleteAccount(r.Context(), requestUserID(r))
	if err != nil {
		s.deps.Logger.Error("deleting an account failed", "error", err)
		internalError(w, "account_delete_failed", "Could not delete the account right now.")
		return
	}

	// The response says which half finished. A client that is told the data is
	// gone while the sign-in still works can act on that; one that is told
	// "deleted" cannot.
	writeJSON(w, http.StatusOK, map[string]any{
		"data_deleted":     report.DatabaseCleaned,
		"identity_deleted": report.IdentityDeleted,
		"pending":          report.Pending,
		"removed": map[string]any{
			"saves":            report.Saves,
			"processing_jobs":  report.ProcessingJobs,
			"idempotency_keys": report.IdempotencyKeys,
			"private_content":  report.PrivateContent,
		},
	})
}

// deletionPending is the answer to any request from a subject whose account is
// on its way out. Their rows are being removed; accepting more work would
// recreate what is being deleted.
func (s *Server) deletionPending(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, errorBody{
		Code:    "account_deletion_pending",
		Message: "This account is being deleted.",
	})
}
