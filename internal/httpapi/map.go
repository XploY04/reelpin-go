package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/mapview"
)

// MapService is what the API needs from the map and Discover service.
type MapService interface {
	Map(ctx context.Context, userID, category string, platforms []string) (mapview.MapResponse, error)
	Search(ctx context.Context, userID, query, category, sessionToken string, limit int) (mapview.SearchResponse, error)
	CreatePin(ctx context.Context, userID, googlePlaceID, sessionToken string) (mapview.MapItem, error)
	HideOrRemove(ctx context.Context, userID, mapItemID string) error
	Discover(ctx context.Context, userID string, offset, limit int, selectedDate string) (mapview.DiscoverResponse, error)
}

type mapPinInput struct {
	GooglePlaceID string `json:"google_place_id"`
	SessionToken  string `json:"session_token"`
}

func (s *Server) handleMap(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	platforms, ok := parsePlatforms(w, query)
	if !ok {
		return
	}

	response, err := s.deps.Map.Map(r.Context(), requestUserID(r), query.Get("category"), platforms)
	if err != nil {
		s.deps.Logger.Error("map data failed", "error", err)
		internalError(w, "map_data_failed", "Could not load map pins right now.")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleMapSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit, ok := intParam(w, query, "limit", 8, 1, 10)
	if !ok {
		return
	}

	response, err := s.deps.Map.Search(r.Context(), requestUserID(r),
		query.Get("query"), query.Get("category"), query.Get("session_token"), limit)
	if err != nil {
		s.deps.Logger.Error("map search failed", "error", err)
		internalError(w, "map_search_failed", "Could not search places right now.")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleCreateMapPin(w http.ResponseWriter, r *http.Request) {
	var input mapPinInput
	if !decodeJSONBody(w, r, &input) {
		return
	}
	if input.GooglePlaceID == "" {
		validationError(w, "google_place_id is required")
		return
	}

	item, err := s.deps.Map.CreatePin(r.Context(), requestUserID(r), input.GooglePlaceID, input.SessionToken)
	if errors.Is(err, mapview.ErrNotFound) {
		notFoundError(w, "map_place_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("map pin failed", "error", err)
		internalError(w, "map_pin_failed", "Could not save that place right now.")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleDeleteMapItem(w http.ResponseWriter, r *http.Request) {
	err := s.deps.Map.HideOrRemove(r.Context(), requestUserID(r), r.PathValue("map_item_id"))
	if errors.Is(err, mapview.ErrNotFound) {
		notFoundError(w, "map_item_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("map item removal failed", "error", err)
		internalError(w, "map_item_remove_failed", "Could not update the map right now.")
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	offset, ok := intParam(w, query, "offset", 0, 0, 1<<31-1)
	if !ok {
		return
	}
	limit, ok := intParam(w, query, "limit", 20, 1, 100)
	if !ok {
		return
	}
	selectedDate, ok := savedDateParamNamed(w, query, "selected_date")
	if !ok {
		return
	}

	response, err := s.deps.Map.Discover(r.Context(), requestUserID(r), offset, limit, selectedDate)
	if err != nil {
		s.deps.Logger.Error("discover failed", "error", err)
		internalError(w, "discover_failed", "Could not load Discover right now.")
		return
	}
	writeJSON(w, http.StatusOK, response)
}
