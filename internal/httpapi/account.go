package httpapi

import "net/http"

func (s *Server) handleLibraryStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.deps.Reels.Stats(r.Context(), requestUserID(r))
	if err != nil {
		s.deps.Logger.Error("library stats failed", "error", err)
		internalError(w, "library_stats_failed", "Could not load library stats right now.")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}
