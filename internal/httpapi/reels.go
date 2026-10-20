package httpapi

import (
	"errors"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/reels"
)

func (s *Server) handleListReels(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	platforms, ok := parsePlatforms(w, query)
	if !ok {
		return
	}
	limit, ok := intParam(w, query, "limit", reels.DefaultPageSize, 1, reels.MaxPageSize)
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
	view, ok := viewParam(w, query)
	if !ok {
		return
	}

	page, err := s.reels.List(r.Context(), requestUserID(r), reels.ListParams{
		Platforms:   platforms,
		Category:    query.Get("category"),
		Subcategory: query.Get("subcategory"),
		SavedDate:   savedDate,
		After:       after,
		Limit:       limit,
		View:        view,
	})
	if err != nil {
		s.deps.Logger.Error("list reels failed", "error", err)
		internalError(w, "reel_list_failed", "Could not load reels right now.")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleGetReel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "reel_id", "reel_not_found")
	if !ok {
		return
	}

	reel, err := s.reels.Get(r.Context(), requestUserID(r), id)
	if errors.Is(err, reels.ErrNotFound) {
		notFoundError(w, "reel_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("get reel failed", "error", err)
		internalError(w, "reel_lookup_failed", "Could not load the reel right now.")
		return
	}
	writeJSON(w, http.StatusOK, reel)
}

func (s *Server) handlePlatformFilters(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	platforms, ok := parsePlatforms(w, query)
	if !ok {
		return
	}

	rows, err := s.deps.Reels.Facets(r.Context(), requestUserID(r))
	if err != nil {
		s.deps.Logger.Error("list reel platform filters failed", "error", err)
		internalError(w, "reel_platform_filters_failed", "Could not load filters right now.")
		return
	}

	writeJSON(w, http.StatusOK, reels.BuildPlatformFilters(
		rows, platforms, query.Get("category"), query.Get("subcategory"),
	))
}

func (s *Server) handleCategoryFilters(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	platforms, ok := parsePlatforms(w, query)
	if !ok {
		return
	}

	userID := requestUserID(r)
	rows, err := s.deps.Reels.Facets(r.Context(), userID)
	if err != nil {
		s.deps.Logger.Error("list reel category filters failed", "error", err)
		internalError(w, "reel_category_filters_failed", "Could not load category filters right now.")
		return
	}

	writeJSON(w, http.StatusOK, reels.BuildCategoryFilters(
		userID, rows, platforms, query.Get("category"), query.Get("subcategory"),
	))
}
