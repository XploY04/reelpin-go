package httpapi

import (
	"context"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/entitlements"
)

// ProStatus reports whether a user is on the paid plan. The implementation
// caches the active grants, so a call is in-memory on the request path.
type ProStatus interface {
	IsPro(ctx context.Context, userID string) bool
}

func (s *Server) handleLibraryStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.deps.Reels.Stats(r.Context(), requestUserID(r))
	if err != nil {
		s.deps.Logger.Error("library stats failed", "error", err)
		internalError(w, "library_stats_failed", "Could not load library stats right now.")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleEntitlements(w http.ResponseWriter, r *http.Request) {
	userID := requestUserID(r)

	// Usage is the cumulative library totals, the same numbers library-stats
	// reports. If they cannot be read the plan cannot be shown honestly, so the
	// response is the restricted body at 200 rather than a 500: the client
	// degrades and retries.
	stats, err := s.deps.Reels.Stats(r.Context(), userID)
	if err != nil {
		s.deps.Logger.Error("entitlements usage failed", "error", err)
		writeJSON(w, http.StatusOK,
			entitlements.Restricted(userID, "Plan access could not be verified right now."))
		return
	}

	usage := entitlements.Usage{
		SavedReels:      stats.TotalReels,
		PinnedLocations: stats.TotalPinnedLocations,
		Tags:            stats.TotalTags,
		Categories:      stats.TotalCategories,
		Subcategories:   stats.TotalSubcategories,
	}
	isPremium := s.deps.Entitlements.IsPro(r.Context(), userID)
	writeJSON(w, http.StatusOK, entitlements.Build(userID, usage, isPremium))
}
