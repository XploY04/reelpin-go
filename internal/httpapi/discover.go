package httpapi

import (
	"net/http"

	"github.com/XploY04/reelpin-go/internal/reels"
)

// discoverDefaultPageSize is the recent-saves page size when the client sends
// none. It is smaller than the reel list default because Discover shows a
// preview, not the whole library.
const discoverDefaultPageSize = 20

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	limit, ok := intParam(w, query, "limit", discoverDefaultPageSize, 1, reels.MaxPageSize)
	if !ok {
		return
	}
	savedDate, ok := savedDateParam(w, query)
	if !ok {
		return
	}
	after, ok := cursorParam(w, query)
	if !ok {
		return
	}

	response, err := s.reels.Discover(r.Context(), requestUserID(r), reels.DiscoverParams{
		After:     after,
		Limit:     limit,
		SavedDate: savedDate,
	})
	if err != nil {
		s.deps.Logger.Error("discover failed", "error", err)
		internalError(w, "discover_failed", "Could not load discover right now.")
		return
	}
	writeJSON(w, http.StatusOK, response)
}
