package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/XploY04/reelpin-go/internal/geo"
	"github.com/XploY04/reelpin-go/internal/mapview"
)

// MapView is what the API needs from the map service.
type MapView interface {
	Pins(ctx context.Context, userID string, bounds geo.Bounds) ([]mapview.Pin, error)
	Nearby(ctx context.Context, userID string, centre geo.Point, radiusMetres float64, limit int) ([]mapview.Pin, error)
	CreateManualPin(ctx context.Context, userID, name string, address *string, point geo.Point) (mapview.Pin, error)
	DeleteManualPin(ctx context.Context, userID, pinID string) error
	HidePin(ctx context.Context, userID, locationID string, hidden bool) error
}

// floatParam reads a required coordinate. An unparseable one is a validation
// error naming the field rather than a silent zero, which would quietly move
// the map to the Gulf of Guinea.
func floatParam(w http.ResponseWriter, r *http.Request, name string) (float64, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		validationError(w, name, "is required")
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		validationError(w, name, "must be a number")
		return 0, false
	}
	return value, true
}

func (s *Server) handleMapPins(w http.ResponseWriter, r *http.Request) {
	south, ok := floatParam(w, r, "south")
	if !ok {
		return
	}
	west, ok := floatParam(w, r, "west")
	if !ok {
		return
	}
	north, ok := floatParam(w, r, "north")
	if !ok {
		return
	}
	east, ok := floatParam(w, r, "east")
	if !ok {
		return
	}

	bounds, err := geo.NewBounds(south, west, north, east)
	if err != nil {
		validationError(w, "bounds", geoReason(err))
		return
	}

	pins, err := s.deps.Map.Pins(r.Context(), requestUserID(r), bounds)
	if err != nil {
		s.deps.Logger.Error("reading map pins failed", "error", err)
		internalError(w, "map_failed", "Could not load the map right now.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pins": pins})
}

func (s *Server) handleMapNearby(w http.ResponseWriter, r *http.Request) {
	latitude, ok := floatParam(w, r, "latitude")
	if !ok {
		return
	}
	longitude, ok := floatParam(w, r, "longitude")
	if !ok {
		return
	}
	centre, err := geo.NewPoint(latitude, longitude)
	if err != nil {
		validationError(w, "latitude", geoReason(err))
		return
	}

	radius := float64(5000)
	if raw := r.URL.Query().Get("radius_metres"); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || parsed <= 0 || parsed > 100_000 {
			validationError(w, "radius_metres", "must be between 1 and 100000")
			return
		}
		radius = parsed
	}
	limit, ok := intParam(w, r.URL.Query(), "limit", 50, 1, mapview.MaxPins)
	if !ok {
		return
	}

	pins, err := s.deps.Map.Nearby(r.Context(), requestUserID(r), centre, radius, limit)
	if err != nil {
		s.deps.Logger.Error("reading nearby pins failed", "error", err)
		internalError(w, "map_failed", "Could not load the map right now.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pins": pins})
}

type manualPinInput struct {
	Name      string  `json:"name"`
	Address   *string `json:"address"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

func (s *Server) handleCreateManualPin(w http.ResponseWriter, r *http.Request) {
	var input manualPinInput
	if !decodeBody(w, r, &input) {
		return
	}
	if input.Name == "" {
		validationError(w, "name", "is required")
		return
	}
	point, err := geo.NewPoint(input.Latitude, input.Longitude)
	if err != nil {
		validationError(w, "latitude", geoReason(err))
		return
	}

	pin, err := s.deps.Map.CreateManualPin(r.Context(), requestUserID(r), input.Name, input.Address, point)
	if err != nil {
		s.deps.Logger.Error("creating a manual pin failed", "error", err)
		internalError(w, "map_failed", "Could not save that pin right now.")
		return
	}
	writeJSON(w, http.StatusCreated, pin)
}

func (s *Server) handleDeleteManualPin(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "pin_id", "pin_not_found")
	if !ok {
		return
	}
	err := s.deps.Map.DeleteManualPin(r.Context(), requestUserID(r), id.String())
	if errors.Is(err, mapview.ErrNotFound) {
		notFoundError(w, "pin_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("deleting a manual pin failed", "error", err)
		internalError(w, "map_failed", "Could not remove that pin right now.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleHidePin(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "location_id", "pin_not_found")
	if !ok {
		return
	}
	var input struct {
		Hidden bool `json:"hidden"`
	}
	if !decodeBody(w, r, &input) {
		return
	}

	err := s.deps.Map.HidePin(r.Context(), requestUserID(r), id.String(), input.Hidden)
	if errors.Is(err, mapview.ErrNotFound) {
		// A location the user cannot reach answers the same as one that does
		// not exist, so hiding cannot be used to enumerate ids.
		notFoundError(w, "pin_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("hiding a pin failed", "error", err)
		internalError(w, "map_failed", "Could not update that pin right now.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hidden": input.Hidden})
}

// geoReason turns a coordinate error into something a person can act on
// without repeating the driver's words.
func geoReason(err error) string {
	switch {
	case errors.Is(err, geo.ErrNotFinite):
		return "must be a finite number"
	case errors.Is(err, geo.ErrEmptyBounds):
		return "south must be below north"
	default:
		return "is outside the range of the world"
	}
}
