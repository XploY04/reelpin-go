package mapview

import (
	"context"
	"strings"
)

// SearchResult is one row in the place picker: either a place the user already
// has, or one the provider suggested.
type SearchResult struct {
	ResultType     string   `json:"result_type"`
	SourceType     *string  `json:"source_type"`
	MapItem        *MapItem `json:"map_item"`
	GooglePlaceID  *string  `json:"google_place_id"`
	DisplayTitle   string   `json:"display_title"`
	DisplayAddress string   `json:"display_address"`
	PlaceName      string   `json:"place_name"`
	Latitude       *float64 `json:"latitude"`
	Longitude      *float64 `json:"longitude"`
	GoogleMapsURL  *string  `json:"google_maps_url"`
	PlaceTypes     []string `json:"place_types"`
	CanPin         bool     `json:"can_pin"`
}

type SearchResponse struct {
	Query      string         `json:"query"`
	SearchMode string         `json:"search_mode"`
	Total      int            `json:"total"`
	Results    []SearchResult `json:"results"`
}

// MinQueryRunes is the shortest query worth running. Anything shorter matches
// everything and costs a provider call.
const MinQueryRunes = 2

// Search looks through the user's own places first and only then asks the
// provider, which is billed per call. Places the user already has are never
// offered again as something to pin.
func (s *Service) Search(ctx context.Context, userID, query, category, sessionToken string, limit int) (SearchResponse, error) {
	normalized := strings.Join(strings.Fields(query), " ")
	if len([]rune(normalized)) < MinQueryRunes {
		return SearchResponse{Query: normalized, SearchMode: "empty", Results: []SearchResult{}}, nil
	}
	if limit <= 0 || limit > 10 {
		limit = 8
	}

	existing, err := s.Map(ctx, userID, category, nil)
	if err != nil {
		return SearchResponse{}, err
	}

	results := []SearchResult{}
	seenPlaceIDs := map[string]bool{}
	lowered := strings.ToLower(normalized)

	for _, item := range existing.MapItems {
		if len(results) >= limit {
			break
		}
		haystack := strings.ToLower(item.PlaceName + " " + item.DisplayAddress)
		if !strings.Contains(haystack, lowered) {
			continue
		}
		saved := item
		sourceType := item.SourceType
		if item.GooglePlaceID != nil {
			seenPlaceIDs[*item.GooglePlaceID] = true
		}
		results = append(results, SearchResult{
			ResultType:     "saved",
			SourceType:     &sourceType,
			MapItem:        &saved,
			GooglePlaceID:  item.GooglePlaceID,
			DisplayTitle:   item.DisplayTitle,
			DisplayAddress: item.DisplayAddress,
			PlaceName:      item.PlaceName,
			Latitude:       item.Latitude,
			Longitude:      item.Longitude,
			GoogleMapsURL:  item.GoogleMapsURL,
			PlaceTypes:     item.PlaceTypes,
			// It is already on the map; pinning it again would duplicate it.
			CanPin: false,
		})
	}

	mode := "saved"
	if s.places != nil && len(results) < limit {
		places, err := s.places.Search(ctx, normalized, sessionToken, limit-len(results))
		if err != nil {
			// A provider outage still leaves the user's own places searchable.
			return SearchResponse{
				Query: normalized, SearchMode: mode, Total: len(results), Results: results,
			}, nil
		}
		for _, place := range places {
			if len(results) >= limit || seenPlaceIDs[place.PlaceID] {
				continue
			}
			placeID := place.PlaceID
			latitude, longitude := place.Latitude, place.Longitude
			mapsURL := place.GoogleMapsURL
			results = append(results, SearchResult{
				ResultType:     "place",
				GooglePlaceID:  &placeID,
				DisplayTitle:   place.Name,
				DisplayAddress: place.Address,
				PlaceName:      place.Name,
				Latitude:       &latitude,
				Longitude:      &longitude,
				GoogleMapsURL:  &mapsURL,
				PlaceTypes:     place.Types,
				CanPin:         true,
			})
			if len(places) > 0 {
				mode = "mixed"
			}
		}
	}

	return SearchResponse{Query: normalized, SearchMode: mode, Total: len(results), Results: results}, nil
}
