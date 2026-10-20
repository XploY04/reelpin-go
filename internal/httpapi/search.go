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
	Search(ctx context.Context, userID string, params search.Params) (search.Response, error)
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
	view, ok := viewParam(w, r.URL.Query())
	if !ok {
		return
	}

	userID := requestUserID(r)
	if !s.allowSearch(w, r, userID) {
		return
	}

	response, err := s.deps.Search.Search(r.Context(), userID, search.Params{
		Query: request.Query,
		Filters: search.Filters{
			Platforms:   platforms,
			Category:    request.Category,
			Subcategory: request.Subcategory,
			SavedDate:   savedDate,
		},
		Limit: request.Limit,
		View:  view,
	})
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
	return s.allowMetered(w, r, userID, ratelimit.Search, ratelimit.SearchIP,
		errorBody{Code: "search_unavailable", Message: "Search is unavailable right now.", Retryable: true},
		"Too many searches. Try again later.")
}

type meteredCheck struct {
	policy  ratelimit.Policy
	subject string
}

// meteredChecks are the buckets one metered request draws on, in order: the
// OAuth client's under the user if any, the user's total, then the IP's.
func meteredChecks(r *http.Request, userID string, user, ip ratelimit.Policy, ipSubject string) []meteredCheck {
	var checks []meteredCheck
	for _, subject := range user.UserSubjects(userID, requestClientID(r)) {
		checks = append(checks, meteredCheck{user, subject})
	}
	return append(checks, meteredCheck{ip, ipSubject})
}

// allowMetered checks the user window, then the IP window, and answers the
// refusal itself. A missing or unreachable limiter answers unavailable.
func (s *Server) allowMetered(w http.ResponseWriter, r *http.Request, userID string, user, ip ratelimit.Policy, unavailableBody errorBody, limitedMessage string) bool {
	unavailable := func() {
		writeError(w, http.StatusServiceUnavailable, unavailableBody)
	}

	if s.deps.Limiter == nil {
		unavailable()
		return false
	}

	for _, check := range meteredChecks(r, userID, user, ip, s.limitSubject(r)) {
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
				Message:   limitedMessage,
				Retryable: true,
				Details:   map[string]any{"retry_after_seconds": int(decision.RetryAfter.Seconds())},
			})
			return false
		}
	}
	return true
}
