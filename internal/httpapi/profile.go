package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/profiles"
)

// Profiles is what the API needs from the profile service.
type Profiles interface {
	Get(ctx context.Context, userID string) (profiles.Profile, error)
	Upsert(ctx context.Context, userID string, params profiles.Params) (profiles.Profile, error)
}

type profileInput struct {
	Email     *string `json:"email"`
	FullName  *string `json:"full_name"`
	AvatarURL *string `json:"avatar_url"`
}

func (s *Server) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	profile, err := s.deps.Profiles.Get(r.Context(), requestUserID(r))
	if errors.Is(err, profiles.ErrNotFound) {
		notFoundError(w, "profile_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("reading the profile failed", "error", err)
		internalError(w, "profile_failed", "Could not load your profile right now.")
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

func (s *Server) handlePutProfile(w http.ResponseWriter, r *http.Request) {
	var input profileInput
	if !decodeBody(w, r, &input) {
		return
	}

	profile, err := s.deps.Profiles.Upsert(r.Context(), requestUserID(r), profiles.Params{
		Email: input.Email, FullName: input.FullName, AvatarURL: input.AvatarURL,
	})
	var invalid *profiles.ValidationError
	if errors.As(err, &invalid) {
		validationError(w, invalid.Field, invalid.Reason)
		return
	}
	if err != nil {
		s.deps.Logger.Error("saving the profile failed", "error", err)
		internalError(w, "profile_failed", "Could not save your profile right now.")
		return
	}
	writeJSON(w, http.StatusOK, profile)
}
