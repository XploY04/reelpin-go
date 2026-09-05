package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/XploY04/reelpin-go/internal/auth"
	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// Enqueuer turns a share into a private job, creating global work only when
// the content has never been processed.
type Enqueuer interface {
	Enqueue(ctx context.Context, request enqueue.Request) (enqueue.Result, error)
}

// ShareTokens issues and checks the long-lived device tokens the native share
// extensions use.
type ShareTokens interface {
	Mint(ctx context.Context, userID, platform string) (string, error)
	UserID(ctx context.Context, raw string) (string, error)
	RevokeAll(ctx context.Context, userID string) (int, error)
}

type enqueueInput struct {
	URL            string   `json:"url"`
	RawPayloadText string   `json:"raw_payload_text"`
	UserID         string   `json:"user_id"`
	CollectionIDs  []string `json:"collection_ids"`
}

type shareTokenResponse struct {
	ShareToken string `json:"share_token"`
}

type genericSuccessResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

func (s *Server) handleEnqueueReel(w http.ResponseWriter, r *http.Request) {
	var input enqueueInput
	if !decodeJSONBody(w, r, &input) {
		return
	}

	// user_id is accepted in the body for wire compatibility and ignored: the
	// user always comes from the credential.
	userID := requestUserID(r)

	result, err := s.deps.Enqueue.Enqueue(r.Context(), enqueue.Request{
		UserID:          userID,
		URL:             input.URL,
		RawPayloadText:  input.RawPayloadText,
		CollectionIDs:   input.CollectionIDs,
		IngestionMethod: "url_share",
	})
	if err != nil {
		s.writeEnqueueError(w, r, err)
		return
	}

	s.deps.Logger.Info("processing job enqueued",
		"job_id", result.Job.ID,
		"platform", optionalText(result.Job.SourcePlatform),
		"reused", result.Reused,
		"url_hash", safehttp.URLHash(result.Job.URL),
	)

	writeJSON(w, http.StatusOK, jobs.BuildResponse(result.Job, s.jobResultReel(r, userID, result.Job), s.now()))
}

func (s *Server) writeEnqueueError(w http.ResponseWriter, r *http.Request, err error) {
	var limit *enqueue.LimitError
	switch {
	case errors.Is(err, enqueue.ErrNoURL), errors.Is(err, sourceidentity.ErrUnsupportedURL):
		writeError(w, http.StatusBadRequest, errorResponse{
			ErrorCode: "invalid_request",
			Message:   "The shared URL is invalid.",
			Detail:    "No supported link was found in the shared content",
		})
	case errors.As(err, &limit):
		// A submission limit is about outstanding work, not request rate, so it
		// keeps its own code and stays retryable.
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, errorResponse{
			ErrorCode: limit.Code,
			Message:   limit.Message,
			Detail:    limit.Detail,
			Retryable: true,
		})
	default:
		s.deps.Logger.Error("enqueue failed", "path", r.URL.Path, "error", err)
		internalError(w, "processing_job_enqueue_failed", "Could not create a processing job right now.")
	}
}

func (s *Server) handleMintShareToken(w http.ResponseWriter, r *http.Request) {
	token, err := s.deps.ShareTokens.Mint(r.Context(), requestUserID(r), r.Header.Get("X-Client-Platform"))
	if err != nil {
		s.deps.Logger.Error("mint share token failed", "error", err)
		internalError(w, "share_token_mint_failed", "Could not set up background sharing right now.")
		return
	}
	// The raw token is returned once and never stored, so it is not logged here
	// or anywhere else.
	writeJSON(w, http.StatusOK, shareTokenResponse{ShareToken: token})
}

func (s *Server) handleRevokeShareTokens(w http.ResponseWriter, r *http.Request) {
	revoked, err := s.deps.ShareTokens.RevokeAll(r.Context(), requestUserID(r))
	if err != nil {
		s.deps.Logger.Error("revoke share tokens failed", "error", err)
		internalError(w, "share_token_revoke_failed", "Could not revoke share access right now.")
		return
	}
	writeJSON(w, http.StatusOK, genericSuccessResponse{
		Success: true,
		Message: "revoked " + plural(revoked) + " share token(s)",
	})
}

func plural(count int) string {
	return strconv.Itoa(count)
}

// shareTokenOrJWT accepts the native share path's device token on the enqueue
// route only, and otherwise falls back to the normal bearer token.
func (s *Server) shareTokenOrJWT(next http.HandlerFunc) http.HandlerFunc {
	authenticated := s.authenticated(next)

	return func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimSpace(r.Header.Get("X-Share-Token"))
		if raw == "" {
			authenticated(w, r)
			return
		}

		userID, err := s.deps.ShareTokens.UserID(r.Context(), raw)
		if err != nil {
			// The device must know to refresh, which is a different message
			// from an expired session.
			writeError(w, http.StatusUnauthorized, errorResponse{
				ErrorCode: "invalid_share_token",
				Message:   "Your share access has expired. Open ReelPin to refresh it.",
				Detail:    "The share token is unknown or revoked",
			})
			return
		}
		next(w, r.WithContext(auth.WithUserID(r.Context(), userID)))
	}
}

func optionalText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
