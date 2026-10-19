package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/XploY04/reelpin-go/internal/mapview"
)

// MapPlaces is what the API needs to search for places and pin them.
type MapPlaces interface {
	Search(ctx context.Context, userID string, params mapview.PlaceSearchParams) (mapview.PlaceSearch, error)
	PinPlace(ctx context.Context, userID string, params mapview.PinPlaceParams) (mapview.Pin, bool, error)
}

func (s *Server) handleMapPlaceSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	text := strings.TrimSpace(query.Get("query"))
	if utf8.RuneCountInString(text) < mapview.MinPlaceQuery {
		validationError(w, "query", "must be at least "+strconv.Itoa(mapview.MinPlaceQuery)+" characters")
		return
	}
	limit, ok := intParam(w, query, "limit", 8, 1, mapview.MaxPlaceResults)
	if !ok {
		return
	}

	// A query can cost a Google autocomplete call, so it shares the search
	// windows and fails closed the same way.
	userID := requestUserID(r)
	if !s.allowSearch(w, r, userID) {
		return
	}

	result, err := s.deps.MapPlaces.Search(r.Context(), userID, mapview.PlaceSearchParams{
		Query:        text,
		Category:     strings.TrimSpace(query.Get("category")),
		SessionToken: query.Get("session_token"),
		Limit:        limit,
	})
	if err != nil {
		s.deps.Logger.Error("searching map places failed", "error", err)
		internalError(w, "map_failed", "Could not search the map right now.")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// placePinInput accepts both spellings the Python service did, so an app
// build sending either one keeps working.
type placePinInput struct {
	GooglePlaceID      string `json:"google_place_id"`
	GooglePlaceIDCamel string `json:"googlePlaceId"`
	SessionToken       string `json:"session_token"`
	SessionTokenCamel  string `json:"sessionToken"`
}

func (s *Server) handlePinMapPlace(w http.ResponseWriter, r *http.Request) {
	var input placePinInput
	if !decodeBody(w, r, &input) {
		return
	}
	placeID := strings.TrimSpace(input.GooglePlaceID)
	if placeID == "" {
		placeID = strings.TrimSpace(input.GooglePlaceIDCamel)
	}
	if placeID == "" {
		validationError(w, "google_place_id", "is required")
		return
	}
	sessionToken := input.SessionToken
	if sessionToken == "" {
		sessionToken = input.SessionTokenCamel
	}

	pin, created, err := s.deps.MapPlaces.PinPlace(r.Context(), requestUserID(r), mapview.PinPlaceParams{
		GooglePlaceID: placeID,
		SessionToken:  sessionToken,
	})
	if errors.Is(err, mapview.ErrPlaceNotFound) {
		notFoundError(w, "place_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("pinning a place failed", "error", err)
		internalError(w, "map_failed", "Could not save that pin right now.")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, pin)
}
