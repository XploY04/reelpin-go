package httpapi

import (
	"net/http"
	"strings"

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

	var after *reels.Cursor
	if raw := strings.TrimSpace(query.Get("cursor")); raw != "" {
		cursor, err := reels.DecodeCursor(raw)
		if err != nil {
			validationError(w, "cursor", "is not a cursor this API issued")
			return
		}
		after = &cursor
	}

	userID := requestUserID(r)

	// The recent-saves page over-fetches by one to answer has_more, the same way
	// the reel list does.
	recent, err := s.deps.Reels.List(r.Context(), userID, reels.ListOptions{
		After: after,
		Limit: limit + 1,
	})
	if err != nil {
		s.deps.Logger.Error("discover recent saves failed", "error", err)
		internalError(w, "discover_failed", "Could not load discover right now.")
		return
	}

	// The aggregate sections (saved dates, the chosen date, the search prompts)
	// read a larger scan of the library than one page.
	scanned, err := s.deps.Reels.List(r.Context(), userID, reels.ListOptions{
		Limit: reels.DiscoverScanLimit,
	})
	if err != nil {
		s.deps.Logger.Error("discover library scan failed", "error", err)
		internalError(w, "discover_failed", "Could not load discover right now.")
		return
	}

	facets, err := s.deps.Reels.Facets(r.Context(), userID)
	if err != nil {
		s.deps.Logger.Error("discover category grid failed", "error", err)
		internalError(w, "discover_failed", "Could not load discover right now.")
		return
	}

	writeJSON(w, http.StatusOK,
		reels.BuildDiscoverResponse(userID, recent, scanned, facets, savedDate, limit, s.now()))
}
