package mapview

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Place is one result from the place provider.
type Place struct {
	PlaceID       string
	Name          string
	Address       string
	Latitude      float64
	Longitude     float64
	GoogleMapsURL string
	Types         []string
}

// Places is the provider seam. Search is billed per call, so it runs only
// after the user's own saved places have been searched.
type Places interface {
	Search(ctx context.Context, query, sessionToken string, limit int) ([]Place, error)
	Details(ctx context.Context, placeID, sessionToken string) (Place, error)
}

// GooglePlaces talks to the Places v1 API. A session token ties an autocomplete
// session to the details call that ends it, which is what makes the pair
// billable as one session instead of two.
type GooglePlaces struct {
	apiKey string
	client *http.Client
}

var (
	placesSearchURL  = "https://places.googleapis.com/v1/places:searchText"
	placesDetailsURL = "https://places.googleapis.com/v1/places/"
)

func NewGooglePlaces(apiKey string, timeout time.Duration) *GooglePlaces {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &GooglePlaces{apiKey: apiKey, client: &http.Client{Timeout: timeout}}
}

func (g *GooglePlaces) configured() bool { return strings.TrimSpace(g.apiKey) != "" }

func (g *GooglePlaces) Search(ctx context.Context, query, sessionToken string, limit int) ([]Place, error) {
	if !g.configured() {
		// Not configured is not an error: the user's own places are still
		// searchable, and that is most of what they look for.
		return nil, nil
	}
	if limit <= 0 || limit > 10 {
		limit = 8
	}

	body, err := json.Marshal(map[string]any{
		"textQuery":      query,
		"maxResultCount": limit,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding the place search: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, placesSearchURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("building the place search: %w", err)
	}
	g.setHeaders(request, sessionToken,
		"places.id,places.displayName,places.formattedAddress,places.location,places.googleMapsUri,places.types")

	response, err := g.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("calling the place provider: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the place provider returned HTTP %d", response.StatusCode)
	}

	var payload struct {
		Places []googlePlace `json:"places"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decoding the place search: %w", err)
	}

	places := make([]Place, 0, len(payload.Places))
	for _, place := range payload.Places {
		places = append(places, place.toPlace())
	}
	return places, nil
}

func (g *GooglePlaces) Details(ctx context.Context, placeID, sessionToken string) (Place, error) {
	if !g.configured() {
		return Place{}, fmt.Errorf("the place provider is not configured")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, placesDetailsURL+placeID, nil)
	if err != nil {
		return Place{}, fmt.Errorf("building the place details request: %w", err)
	}
	g.setHeaders(request, sessionToken, "id,displayName,formattedAddress,location,googleMapsUri,types")

	response, err := g.client.Do(request)
	if err != nil {
		return Place{}, fmt.Errorf("calling the place provider: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotFound {
		return Place{}, ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return Place{}, fmt.Errorf("the place provider returned HTTP %d", response.StatusCode)
	}

	var payload googlePlace
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return Place{}, fmt.Errorf("decoding the place details: %w", err)
	}
	return payload.toPlace(), nil
}

// setHeaders sends the key and the field mask. The mask is what keeps the
// response small and the call cheap.
func (g *GooglePlaces) setHeaders(request *http.Request, sessionToken, fieldMask string) {
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Goog-Api-Key", g.apiKey)
	request.Header.Set("X-Goog-FieldMask", fieldMask)
	if strings.TrimSpace(sessionToken) != "" {
		request.Header.Set("X-Goog-Session-Token", sessionToken)
	}
}

type googlePlace struct {
	ID          string `json:"id"`
	DisplayName struct {
		Text string `json:"text"`
	} `json:"displayName"`
	FormattedAddress string `json:"formattedAddress"`
	Location         struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"location"`
	GoogleMapsURI string   `json:"googleMapsUri"`
	Types         []string `json:"types"`
}

func (g googlePlace) toPlace() Place {
	return Place{
		PlaceID:       g.ID,
		Name:          g.DisplayName.Text,
		Address:       g.FormattedAddress,
		Latitude:      g.Location.Latitude,
		Longitude:     g.Location.Longitude,
		GoogleMapsURL: g.GoogleMapsURI,
		Types:         g.Types,
	}
}
