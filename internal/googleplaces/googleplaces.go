// Package googleplaces reads the Google Places API (New): autocomplete
// suggestions for a typed query, and the details of one place.
//
// The endpoint is ours and constant, so this is the standard library client
// with a timeout rather than internal/safehttp, which guards URLs a user
// supplied.
package googleplaces

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/metrics"
)

// ErrNotFound is a place Google does not know, one without coordinates, or any
// lookup made while no key is configured.
var ErrNotFound = errors.New("google place not found")

const (
	DefaultBaseURL = "https://places.googleapis.com/v1"
	DefaultTimeout = 10 * time.Second

	// maxResponseBytes bounds one answer. Autocomplete returns at most a few
	// suggestions and details a handful of fields; an error page from a proxy
	// is the only thing that would come near this.
	maxResponseBytes = 1 << 20

	// placeholderKey is the value the Python service's example environment
	// ships with. A deployment still carrying it has no key.
	placeholderKey = "your_google_maps_api_key_here"

	searchFieldMask = "suggestions.placePrediction.placeId," +
		"suggestions.placePrediction.text," +
		"suggestions.placePrediction.structuredFormat," +
		"suggestions.placePrediction.types"
	detailsFieldMask = "id,displayName,formattedAddress,location,googleMapsUri,types"

	// failureLabel is the platform label provider failures are counted under.
	failureLabel = "google_places"
)

// Suggestion is one autocomplete prediction.
type Suggestion struct {
	PlaceID string
	Title   string
	Address string
	Types   []string
}

// Place is one place's details.
type Place struct {
	ID        string
	Name      string
	Address   string
	Latitude  float64
	Longitude float64
	MapsURL   string
	Types     []string
}

type Config struct {
	APIKey string
	// Timeout bounds each call. Zero means DefaultTimeout.
	Timeout time.Duration
	// BaseURL is the API root. Empty means DefaultBaseURL; tests point it at a
	// local server.
	BaseURL string
	// Metrics counts failed calls. Nil means nothing is counted.
	Metrics *metrics.Metrics
}

type Client struct {
	config Config
	client *http.Client
}

func New(config Config) *Client {
	config.APIKey = strings.TrimSpace(config.APIKey)
	if config.Timeout <= 0 {
		config.Timeout = DefaultTimeout
	}
	if config.BaseURL == "" {
		config.BaseURL = DefaultBaseURL
	}
	return &Client{config: config, client: &http.Client{Timeout: config.Timeout}}
}

// Configured reports whether a usable key is set.
func (c *Client) Configured() bool {
	return c.config.APIKey != "" && c.config.APIKey != placeholderKey
}

// Autocomplete returns at most limit suggestions. Without a key it returns
// none, which a search reports as no Google results rather than a failure.
func (c *Client) Autocomplete(ctx context.Context, input, sessionToken string, limit int) ([]Suggestion, error) {
	if !c.Configured() {
		return nil, nil
	}

	body := map[string]string{"input": input}
	if token := strings.TrimSpace(sessionToken); token != "" {
		body["sessionToken"] = token
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("google places autocomplete: encoding the request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.config.BaseURL+"/places:autocomplete", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("google places autocomplete: building the request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Goog-Api-Key", c.config.APIKey)
	request.Header.Set("X-Goog-FieldMask", searchFieldMask)

	var answer struct {
		Suggestions []struct {
			PlacePrediction *struct {
				PlaceID          string    `json:"placeId"`
				Text             textValue `json:"text"`
				StructuredFormat struct {
					MainText      textValue `json:"mainText"`
					SecondaryText textValue `json:"secondaryText"`
				} `json:"structuredFormat"`
				Types []string `json:"types"`
			} `json:"placePrediction"`
		} `json:"suggestions"`
	}
	status, err := c.do(request, &answer)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("answered %d", status)
	}
	if err != nil {
		c.recordFailure()
		return nil, fmt.Errorf("google places autocomplete: %w", err)
	}

	suggestions := []Suggestion{}
	for _, suggestion := range answer.Suggestions {
		prediction := suggestion.PlacePrediction
		if prediction == nil {
			continue
		}
		placeID := strings.TrimSpace(prediction.PlaceID)
		if placeID == "" {
			continue
		}
		title := firstNonEmpty(prediction.StructuredFormat.MainText.value(), prediction.Text.value(), placeID)
		suggestions = append(suggestions, Suggestion{
			PlaceID: placeID,
			Title:   title,
			Address: prediction.StructuredFormat.SecondaryText.value(),
			Types:   cleanList(prediction.Types),
		})
		if len(suggestions) >= limit {
			break
		}
	}
	return suggestions, nil
}

// Details looks one place up. A place Google does not know, or one it knows
// without coordinates, is ErrNotFound, and so is every lookup without a key.
func (c *Client) Details(ctx context.Context, placeID, sessionToken string) (Place, error) {
	if !c.Configured() {
		return Place{}, fmt.Errorf("google places is not configured: %w", ErrNotFound)
	}

	query := url.Values{"languageCode": {"en"}}
	if token := strings.TrimSpace(sessionToken); token != "" {
		query.Set("sessionToken", token)
	}
	target := c.config.BaseURL + "/places/" + url.PathEscape(placeID) + "?" + query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Place{}, fmt.Errorf("google place details: building the request: %w", err)
	}
	request.Header.Set("X-Goog-Api-Key", c.config.APIKey)
	request.Header.Set("X-Goog-FieldMask", detailsFieldMask)

	var answer struct {
		ID               string    `json:"id"`
		DisplayName      textValue `json:"displayName"`
		FormattedAddress string    `json:"formattedAddress"`
		Location         struct {
			Latitude  *float64 `json:"latitude"`
			Longitude *float64 `json:"longitude"`
		} `json:"location"`
		GoogleMapsURI string   `json:"googleMapsUri"`
		Types         []string `json:"types"`
	}
	status, err := c.do(request, &answer)
	if status == http.StatusNotFound {
		return Place{}, ErrNotFound
	}
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("answered %d", status)
	}
	if err != nil {
		c.recordFailure()
		return Place{}, fmt.Errorf("google place details: %w", err)
	}

	if answer.Location.Latitude == nil || answer.Location.Longitude == nil {
		return Place{}, fmt.Errorf("google place has no coordinates: %w", ErrNotFound)
	}
	return Place{
		ID:        firstNonEmpty(strings.TrimSpace(answer.ID), placeID),
		Name:      answer.DisplayName.value(),
		Address:   strings.TrimSpace(answer.FormattedAddress),
		Latitude:  *answer.Location.Latitude,
		Longitude: *answer.Location.Longitude,
		MapsURL:   strings.TrimSpace(answer.GoogleMapsURI),
		Types:     cleanList(answer.Types),
	}, nil
}

// do sends the request and decodes a 200 into target. Any other status is
// returned with no error so the caller decides what it means; the body is not
// quoted, because a provider error can echo the request and its key.
func (c *Client) do(request *http.Request, target any) (int, error) {
	response, err := c.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		return response.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(target); err != nil {
		return response.StatusCode, errors.New("the response was not JSON")
	}
	return response.StatusCode, nil
}

func (c *Client) recordFailure() {
	if c.config.Metrics != nil {
		c.config.Metrics.ProviderFailures.WithLabelValues(failureLabel, "transient").Inc()
	}
}

type textValue struct {
	Text string `json:"text"`
}

func (t textValue) value() string { return strings.TrimSpace(t.Text) }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// cleanList trims every entry and drops the empty ones, and is never nil.
func cleanList(values []string) []string {
	cleaned := []string{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			cleaned = append(cleaned, value)
		}
	}
	return cleaned
}
