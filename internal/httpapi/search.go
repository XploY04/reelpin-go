package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/XploY04/reelpin-go/internal/ratelimit"
	"github.com/XploY04/reelpin-go/internal/search"
)

// Searcher is what the API needs from the search service.
type Searcher interface {
	Search(ctx context.Context, userID, query string, filters search.Filters, limit int) (search.Response, error)
}

type searchRequest struct {
	Query       string `json:"query"`
	Platform    string `json:"platform"`
	Category    string `json:"category"`
	Subcategory string `json:"subcategory"`
	SavedDate   string `json:"saved_date"`
	Limit       int    `json:"limit"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	var request searchRequest
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Query) == "" {
		validationError(w, "query", "is required")
		return
	}
	if request.Limit < 0 || request.Limit > search.MaxLimit {
		validationError(w, "limit", "must be between 1 and "+strconv.Itoa(search.MaxLimit))
		return
	}

	platforms, ok := parsePlatformValue(w, request.Platform)
	if !ok {
		return
	}
	savedDate, ok := savedDateValue(w, request.SavedDate)
	if !ok {
		return
	}

	userID := requestUserID(r)
	if !s.allowSearch(w, r, userID) {
		return
	}

	response, err := s.deps.Search.Search(r.Context(), userID, request.Query, search.Filters{
		Platforms:   platforms,
		Category:    request.Category,
		Subcategory: request.Subcategory,
		SavedDate:   savedDate,
	}, request.Limit)
	if err != nil {
		s.deps.Logger.Error("search failed", "error", err)
		internalError(w, "search_failed", "Could not search right now.")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// allowSearch applies the user and IP windows and fails closed, for the same
// reason submissions do: a query costs an embedding call, and an unmetered
// provider call is worse than a stable 503.
func (s *Server) allowSearch(w http.ResponseWriter, r *http.Request, userID string) bool {
	unavailable := func() {
		writeError(w, http.StatusServiceUnavailable, errorBody{
			Code:      "search_unavailable",
			Message:   "Search is unavailable right now.",
			Retryable: true,
		})
	}

	if s.deps.Limiter == nil {
		unavailable()
		return false
	}

	for _, check := range []struct {
		policy  ratelimit.Policy
		subject string
	}{
		{ratelimit.Search, userID},
		{ratelimit.SearchIP, clientIP(r)},
	} {
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
				Message:   "Too many searches. Try again later.",
				Retryable: true,
				Details:   map[string]any{"retry_after_seconds": int(decision.RetryAfter.Seconds())},
			})
			return false
		}
	}
	return true
}
