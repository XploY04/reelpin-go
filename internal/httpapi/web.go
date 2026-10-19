package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/ratelimit"
	"github.com/XploY04/reelpin-go/internal/website"
)

// Website is what the API needs from the service behind the marketing site.
type Website interface {
	SubmitFeedback(ctx context.Context, userID string, feedback website.Feedback) (string, error)
	RecordClick(ctx context.Context, click website.Click) error
}

type feedbackInput struct {
	Topic   string `json:"topic"`
	Email   string `json:"email"`
	Message string `json:"message"`
}

type linkClickInput struct {
	Target    string `json:"target"`
	Referer   string `json:"referer"`
	UserAgent string `json:"user_agent"`
	Country   string `json:"country"`
}

type limitCheck struct {
	policy  ratelimit.Policy
	subject string
}

// allowWebWrite applies each window and fails closed. Both endpoints store
// text from someone else's keyboard, so without a working limiter the safe
// answer is the stable 503, not an unmetered write.
func (s *Server) allowWebWrite(w http.ResponseWriter, r *http.Request, unavailableCode string, checks ...limitCheck) bool {
	unavailable := func() {
		writeError(w, http.StatusServiceUnavailable, errorBody{
			Code:      unavailableCode,
			Message:   "This is unavailable right now.",
			Retryable: true,
		})
	}
	if s.deps.Limiter == nil {
		unavailable()
		return false
	}

	for _, check := range checks {
		decision, err := s.deps.Limiter.Allow(r.Context(), check.policy, check.subject)
		if errors.Is(err, ratelimit.ErrUnavailable) {
			unavailable()
			return false
		}
		if err != nil {
			s.deps.Logger.Error("rate limit check failed", "policy", check.policy.Name, "error", err)
			internalError(w, "internal_error", "The server could not finish this request.")
			return false
		}
		if !decision.Allowed {
			w.Header().Set("Retry-After", formatSeconds(decision.RetryAfter))
			writeError(w, http.StatusTooManyRequests, errorBody{
				Code:      "rate_limited",
				Message:   "Too many requests. Try again later.",
				Retryable: true,
				Details:   map[string]any{"retry_after_seconds": int(decision.RetryAfter.Seconds())},
			})
			return false
		}
	}
	return true
}

// optionalUserID is the token's user when a valid bearer token came with the
// request, and empty otherwise. A missing or rejected token is not an error on
// a public route; the write is simply anonymous.
func (s *Server) optionalUserID(r *http.Request) string {
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return ""
	}
	userID, err := s.deps.Auth.Authenticate(r.Context(), token)
	if err != nil {
		return ""
	}
	return userID
}

func (s *Server) handleSubmitFeedback(w http.ResponseWriter, r *http.Request) {
	var input feedbackInput
	if !decodeBody(w, r, &input) {
		return
	}
	if !s.allowWebWrite(w, r, "feedback_unavailable",
		limitCheck{ratelimit.FeedbackIP, s.limitSubject(r)}) {
		return
	}
	userID := s.optionalUserID(r)

	id, err := s.deps.Website.SubmitFeedback(r.Context(), userID, website.Feedback{
		Topic: input.Topic, Email: input.Email, Message: input.Message,
	})
	var invalid *website.ValidationError
	if errors.As(err, &invalid) {
		validationError(w, invalid.Field, invalid.Reason)
		return
	}
	if err != nil {
		s.deps.Logger.Error("saving feedback failed", "error", err)
		internalError(w, "feedback_failed", "Could not send your message right now.")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleRecordLinkClick(w http.ResponseWriter, r *http.Request) {
	var input linkClickInput
	if !decodeBody(w, r, &input) {
		return
	}
	if !s.allowWebWrite(w, r, "link_clicks_unavailable",
		limitCheck{ratelimit.LinkClick, s.limitSubject(r)}) {
		return
	}

	err := s.deps.Website.RecordClick(r.Context(), website.Click{
		Target: input.Target, Referer: input.Referer,
		UserAgent: input.UserAgent, Country: input.Country,
	})
	var invalid *website.ValidationError
	if errors.As(err, &invalid) {
		validationError(w, invalid.Field, invalid.Reason)
		return
	}
	if err != nil {
		s.deps.Logger.Error("recording a link click failed", "error", err)
		internalError(w, "link_click_failed", "Could not record that click right now.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
