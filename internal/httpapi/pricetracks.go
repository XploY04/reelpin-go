package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/pricetracks"
)

// PriceTracks is what the API needs from the price tracks service.
type PriceTracks interface {
	List(ctx context.Context, userID string) ([]pricetracks.PriceTrack, error)
	Create(ctx context.Context, userID string, params pricetracks.CreateParams) (pricetracks.PriceTrack, error)
	Delete(ctx context.Context, userID, trackID string) error
}

type priceTrackListResponse struct {
	PriceTracks []pricetracks.PriceTrack `json:"price_tracks"`
}

type priceTrackCreateInput struct {
	URL         string   `json:"url"`
	TargetPrice *float64 `json:"target_price"`
	Title       string   `json:"title"`
	ReelID      *string  `json:"reel_id"`
}

func (s *Server) handleListPriceTracks(w http.ResponseWriter, r *http.Request) {
	list, err := s.deps.PriceTracks.List(r.Context(), requestUserID(r))
	if err != nil {
		s.deps.Logger.Error("listing price tracks failed", "error", err)
		internalError(w, "price_track_list_failed", "Could not load price tracks right now.")
		return
	}
	writeJSON(w, http.StatusOK, priceTrackListResponse{PriceTracks: list})
}

func (s *Server) handleCreatePriceTrack(w http.ResponseWriter, r *http.Request) {
	var input priceTrackCreateInput
	if !decodeJSONBody(w, r, &input) {
		return
	}

	track, err := s.deps.PriceTracks.Create(r.Context(), requestUserID(r), pricetracks.CreateParams{
		URL: input.URL, TargetPrice: input.TargetPrice, Title: input.Title, ReelID: input.ReelID,
	})
	var invalid *pricetracks.ValidationError
	switch {
	case errors.As(err, &invalid):
		validationError(w, invalid.Field, invalid.Reason)
		return
	case errors.Is(err, pricetracks.ErrUnknownSave):
		validationError(w, "reel_id", "must be one of your saves")
		return
	case err != nil:
		s.deps.Logger.Error("creating a price track failed", "error", err)
		internalError(w, "price_track_create_failed", "Could not start tracking that price right now.")
		return
	}
	writeJSON(w, http.StatusOK, track)
}

func (s *Server) handleDeletePriceTrack(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "track_id", "price_track_not_found")
	if !ok {
		return
	}
	err := s.deps.PriceTracks.Delete(r.Context(), requestUserID(r), id.String())
	if errors.Is(err, pricetracks.ErrNotFound) {
		notFoundError(w, "price_track_not_found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("deleting a price track failed", "error", err)
		internalError(w, "price_track_delete_failed", "Could not delete the price track right now.")
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}
