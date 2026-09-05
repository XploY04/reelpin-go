package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/XploY04/reelpin-go/internal/search"
)

// Searcher is what the API needs from the search service.
type Searcher interface {
	Search(ctx context.Context, userID, query string, filters search.Filters, limit int) (search.Response, error)
}

type searchInput struct {
	Query       string `json:"query"`
	UserID      string `json:"user_id"`
	Platform    string `json:"platform"`
	Category    string `json:"category"`
	Subcategory string `json:"subcategory"`
	SavedDate   string `json:"saved_date"`
	Limit       int    `json:"limit"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	var input searchInput
	if !decodeJSONBody(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Query) == "" {
		validationError(w, "query is required")
		return
	}
	if input.Limit < 0 || input.Limit > search.MaxLimit {
		validationError(w, "limit must be between 1 and 20")
		return
	}

	platforms, ok := parsePlatformValue(w, input.Platform)
	if !ok {
		return
	}

	// user_id in the body is accepted for wire compatibility and ignored.
	response, err := s.deps.Search.Search(r.Context(), requestUserID(r), input.Query, search.Filters{
		Platforms:   platforms,
		Category:    input.Category,
		Subcategory: input.Subcategory,
		SavedDate:   input.SavedDate,
	}, input.Limit)
	if err != nil {
		s.deps.Logger.Error("search failed", "error", err)
		internalError(w, "search_failed", "Could not search right now.")
		return
	}
	writeJSON(w, http.StatusOK, response)
}
