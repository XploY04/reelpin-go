package mapview

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/googleplaces"
	"github.com/jackc/pgx/v5"
)

// ErrPlaceNotFound is a Google place that cannot be pinned: unknown to Google,
// without coordinates, or asked for while no key is configured.
var ErrPlaceNotFound = errors.New("place not found")

const (
	// MinPlaceQuery is the shortest query a place search runs.
	MinPlaceQuery = 2
	// MaxPlaceResults bounds each half of a place search's answer.
	MaxPlaceResults = 10

	// localMatchThreshold is the share of query tokens a pin must contain to
	// count as a match when the whole query is not a substring of it. It is
	// the Python service's value, so both answer the same query the same way.
	localMatchThreshold = 0.62

	// maxPlaceCandidates bounds how many of a user's pins one search scans.
	maxPlaceCandidates = 2000

	// uncategorised is the label an empty category reads as, here and in the
	// Python service.
	uncategorised = "Other"
)

// Candidate is a pin with the save text a place search matches against.
type Candidate struct {
	Pin
	Title   string
	Summary string
}

// PlacePin is what a manual pin made from a Google place stores.
type PlacePin struct {
	Place       googleplaces.Place
	Category    string
	Subcategory string
}

// PlaceStore is the storage a place search and pin need.
type PlaceStore interface {
	PlaceCandidates(ctx context.Context, userID string) ([]Candidate, error)
	PinByPlace(ctx context.Context, userID, googlePlaceID string) (Pin, error)
	UpsertPlacePin(ctx context.Context, userID string, pin PlacePin) (Pin, error)
}

// PlaceFinder is the Google Places lookup.
type PlaceFinder interface {
	Autocomplete(ctx context.Context, input, sessionToken string, limit int) ([]googleplaces.Suggestion, error)
	Details(ctx context.Context, placeID, sessionToken string) (googleplaces.Place, error)
}

type PlaceSearchParams struct {
	Query        string
	Category     string
	SessionToken string
	Limit        int
}

type PinPlaceParams struct {
	GooglePlaceID string
	SessionToken  string
}

// PlaceResult is one row of a place search: a pin the user already has, or a
// Google place they could pin.
type PlaceResult struct {
	ResultType     string   `json:"result_type"`
	SourceType     *string  `json:"source_type"`
	MapItem        *Pin     `json:"map_item"`
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

type PlaceSearch struct {
	Query      string        `json:"query"`
	SearchMode string        `json:"search_mode"`
	Total      int           `json:"total"`
	Results    []PlaceResult `json:"results"`
}

// Places searches for places to pin and pins them.
type Places struct {
	store  PlaceStore
	finder PlaceFinder
	logger *slog.Logger
}

func NewPlaces(store PlaceStore, finder PlaceFinder, logger *slog.Logger) *Places {
	return &Places{store: store, finder: finder, logger: logger}
}

// Search answers the user's own pins that match first, then Google places
// they have not pinned yet. Google failing is an answer without Google, not a
// failed search: the user's own pins are still worth showing.
func (p *Places) Search(ctx context.Context, userID string, params PlaceSearchParams) (PlaceSearch, error) {
	query := normalizeSpace(params.Query)
	if len([]rune(query)) < MinPlaceQuery {
		return PlaceSearch{Query: query, SearchMode: "empty", Results: []PlaceResult{}}, nil
	}

	candidates, err := p.store.PlaceCandidates(ctx, userID)
	if err != nil {
		return PlaceSearch{}, err
	}
	matched := matchCandidates(candidates, query, params.Category, params.Limit)

	suggestions, err := p.finder.Autocomplete(ctx, query, params.SessionToken, params.Limit)
	if err != nil {
		p.logger.Warn("google places autocomplete failed", "error", err)
		suggestions = nil
	}

	results := make([]PlaceResult, 0, len(matched)+len(suggestions))
	for _, candidate := range matched {
		results = append(results, existingResult(candidate))
	}
	google := dedupeSuggestions(suggestions, matched)
	results = append(results, google...)

	return PlaceSearch{
		Query:      query,
		SearchMode: searchMode(len(matched), len(google)),
		Total:      len(results),
		Results:    results,
	}, nil
}

// PinPlace pins a Google place, or returns the pin the user already has for
// it without asking Google again. created is false for the second case.
func (p *Places) PinPlace(ctx context.Context, userID string, params PinPlaceParams) (pin Pin, created bool, err error) {
	placeID := strings.TrimSpace(params.GooglePlaceID)
	if placeID == "" {
		return Pin{}, false, ErrPlaceNotFound
	}

	existing, err := p.store.PinByPlace(ctx, userID, placeID)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Pin{}, false, err
	}

	place, err := p.finder.Details(ctx, placeID, params.SessionToken)
	if errors.Is(err, googleplaces.ErrNotFound) {
		return Pin{}, false, ErrPlaceNotFound
	}
	if err != nil {
		return Pin{}, false, err
	}

	pin, err = p.store.UpsertPlacePin(ctx, userID, PlacePin{
		Place:       place,
		Category:    uncategorised,
		Subcategory: uncategorised,
	})
	if err != nil {
		return Pin{}, false, err
	}
	return pin, true, nil
}

// matchCandidates keeps the pins that match the query, best first. The whole
// query appearing in a pin's text is a full match; otherwise the score is the
// share of query tokens it contains.
func matchCandidates(candidates []Candidate, query, category string, limit int) []Candidate {
	type scored struct {
		candidate Candidate
		score     float64
	}

	lowered := strings.ToLower(query)
	tokens := searchTokens(lowered)
	ranked := []scored{}
	for _, candidate := range candidates {
		if category != "" && labelOr(candidate.Category, uncategorised) != category {
			continue
		}
		score := matchScore(candidate, lowered, tokens)
		if score < localMatchThreshold {
			continue
		}
		ranked = append(ranked, scored{candidate, score})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })

	matched := []Candidate{}
	for _, entry := range ranked {
		if len(matched) >= limit {
			break
		}
		matched = append(matched, entry.candidate)
	}
	return matched
}

func matchScore(candidate Candidate, query string, tokens []string) float64 {
	parts := []string{
		displayTitle(candidate), candidate.Title, candidate.Summary, candidate.Name,
		displayAddress(candidate.Pin), deref(candidate.Address),
		labelOr(candidate.Category, uncategorised), labelOr(candidate.Subcategory, uncategorised),
	}
	haystack := strings.ToLower(normalizeSpace(strings.Join(parts, " ")))
	if strings.Contains(haystack, query) {
		return 1
	}
	if len(tokens) == 0 {
		return 0
	}
	hits := 0
	for _, token := range tokens {
		if strings.Contains(haystack, token) {
			hits++
		}
	}
	return float64(hits) / float64(len(tokens))
}

func existingResult(candidate Candidate) PlaceResult {
	pin := candidate.Pin.withLists()
	latitude, longitude := pin.Latitude, pin.Longitude
	return PlaceResult{
		ResultType:     "existing",
		SourceType:     &pin.Kind,
		MapItem:        &pin,
		GooglePlaceID:  pin.GooglePlaceID,
		DisplayTitle:   displayTitle(candidate),
		DisplayAddress: displayAddress(pin),
		PlaceName:      pin.Name,
		Latitude:       &latitude,
		Longitude:      &longitude,
		GoogleMapsURL:  pin.GoogleMapsURL,
		PlaceTypes:     pin.PlaceTypes,
		CanPin:         false,
	}
}

// dedupeSuggestions drops a Google place the user already has among the
// matches, and a place Google suggested twice. Two places are the same when
// they share a place id, coordinates to about a metre, or an address.
func dedupeSuggestions(suggestions []googleplaces.Suggestion, existing []Candidate) []PlaceResult {
	taken := map[string]bool{}
	for _, candidate := range existing {
		latitude, longitude := candidate.Latitude, candidate.Longitude
		for _, key := range placeKeys(deref(candidate.GooglePlaceID), &latitude, &longitude, deref(candidate.Address)) {
			taken[key] = true
		}
	}

	seen := map[string]bool{}
	results := []PlaceResult{}
	for _, suggestion := range suggestions {
		keys := placeKeys(suggestion.PlaceID, nil, nil, suggestion.Address)
		duplicate := false
		for _, key := range keys {
			if taken[key] || seen[key] {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		for _, key := range keys {
			seen[key] = true
		}

		placeID := suggestion.PlaceID
		results = append(results, PlaceResult{
			ResultType:     "google",
			GooglePlaceID:  &placeID,
			DisplayTitle:   suggestion.Title,
			DisplayAddress: suggestion.Address,
			PlaceName:      suggestion.Title,
			PlaceTypes:     nonNil(suggestion.Types),
			CanPin:         true,
		})
	}
	return results
}

func placeKeys(placeID string, latitude, longitude *float64, address string) []string {
	keys := []string{}
	if normalized := normalizeDedupe(placeID); normalized != "" {
		keys = append(keys, "place:"+normalized)
	}
	if latitude != nil && longitude != nil {
		keys = append(keys, fmt.Sprintf("point:%.5f,%.5f", round5(*latitude), round5(*longitude)))
	}
	if normalized := normalizeDedupe(address); normalized != "" {
		keys = append(keys, "address:"+normalized)
	}
	return keys
}

func searchMode(existing, google int) string {
	switch {
	case existing > 0 && google > 0:
		return "mixed"
	case existing > 0:
		return "existing"
	default:
		return "google"
	}
}

// displayTitle is what a content pin's save is called, or the place's name.
func displayTitle(candidate Candidate) string {
	if title := strings.TrimSpace(candidate.Title); title != "" {
		return title
	}
	return candidate.Name
}

// displayAddress falls back to the name and then to the coordinates, so an
// existing result always has a second line to show.
func displayAddress(pin Pin) string {
	if address := strings.TrimSpace(deref(pin.Address)); address != "" {
		return address
	}
	if pin.Name != "" {
		return pin.Name
	}
	return fmt.Sprintf("%.5f, %.5f", pin.Latitude, pin.Longitude)
}

var (
	whitespace = regexp.MustCompile(`\s+`)
	tokenRun   = regexp.MustCompile(`[a-z0-9]+`)
	nonAlnum   = regexp.MustCompile(`[^a-z0-9]+`)
)

func normalizeSpace(value string) string {
	return whitespace.ReplaceAllString(strings.TrimSpace(value), " ")
}

// searchTokens are the ASCII words of two or more characters, as the Python
// service tokenises; a query in another script matches only as a substring.
func searchTokens(lowered string) []string {
	tokens := []string{}
	for _, token := range tokenRun.FindAllString(lowered, -1) {
		if len(token) > 1 {
			tokens = append(tokens, token)
		}
	}
	return tokens
}

func normalizeDedupe(value string) string {
	return strings.TrimSpace(nonAlnum.ReplaceAllString(strings.ToLower(value), " "))
}

func round5(value float64) float64 {
	return math.Round(value*1e5) / 1e5
}

func labelOr(value *string, fallback string) string {
	if cleaned := strings.TrimSpace(deref(value)); cleaned != "" {
		return cleaned
	}
	return fallback
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// PlaceCandidates is every pin this user can see, newest first, with the text
// a place search matches against. A manual pin's title is its name and its
// summary its address, as in the Python service.
func (s *Service) PlaceCandidates(ctx context.Context, userID string) ([]Candidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+contentPinColumns+`, v.title, v.summary, s.saved_at
		FROM reelpin.content_locations l
		JOIN reelpin.contents c ON c.current_version_id = l.content_version_id
		JOIN reelpin.content_versions v ON v.id = l.content_version_id
		JOIN reelpin.user_saves s ON s.content_id = c.id AND s.user_id = $1
		LEFT JOIN reelpin.user_pin_preferences p
		       ON p.location_id = l.id AND p.user_id = $1
		WHERE NOT coalesce(p.hidden, false)

		UNION ALL

		SELECT `+manualPinColumns+`, m.name, coalesce(m.address, ''), m.created_at
		FROM reelpin.user_manual_pins m
		WHERE m.user_id = $1

		ORDER BY 16 DESC, 1
		LIMIT $2`, userID, maxPlaceCandidates)
	if err != nil {
		return nil, fmt.Errorf("reading place candidates: %w", err)
	}
	defer rows.Close()

	candidates := []Candidate{}
	for rows.Next() {
		var (
			candidate Candidate
			savedAt   time.Time
		)
		targets := append(pinTargets(&candidate.Pin), &candidate.Title, &candidate.Summary, &savedAt)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("reading a place candidate: %w", err)
		}
		candidate.Pin = candidate.Pin.withLists()
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// PinByPlace is this user's pin for one Google place.
func (s *Service) PinByPlace(ctx context.Context, userID, googlePlaceID string) (Pin, error) {
	var pin Pin
	err := s.pool.QueryRow(ctx, `
		SELECT `+manualPinColumns+`
		FROM reelpin.user_manual_pins m
		WHERE m.user_id = $1 AND m.google_place_id = $2`,
		userID, googlePlaceID,
	).Scan(pinTargets(&pin)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pin{}, ErrNotFound
	}
	if err != nil {
		return Pin{}, fmt.Errorf("reading a place pin: %w", err)
	}
	return pin.withLists(), nil
}

// UpsertPlacePin stores a pin for a Google place. A second pin of the same
// place refreshes the first rather than adding another.
func (s *Service) UpsertPlacePin(ctx context.Context, userID string, input PlacePin) (Pin, error) {
	place := input.Place
	var address *string
	if place.Address != "" {
		address = &place.Address
	}
	var mapsURL *string
	if place.MapsURL != "" {
		mapsURL = &place.MapsURL
	}
	types := nonNil(place.Types)
	metadata := map[string]any{"google_place_id": place.ID, "place_types": types}

	var pin Pin
	err := s.pool.QueryRow(ctx, `
		INSERT INTO reelpin.user_manual_pins AS m
			(user_id, name, address, point, google_place_id, google_maps_url,
			 category, subcategory, place_types, metadata)
		VALUES ($1, $2, $3, ST_MakePoint($5, $4)::geography, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (user_id, google_place_id) DO UPDATE SET
			name = EXCLUDED.name,
			address = EXCLUDED.address,
			point = EXCLUDED.point,
			google_maps_url = EXCLUDED.google_maps_url,
			category = EXCLUDED.category,
			subcategory = EXCLUDED.subcategory,
			place_types = EXCLUDED.place_types,
			metadata = EXCLUDED.metadata
		RETURNING `+manualPinColumns,
		userID, place.Name, address, place.Latitude, place.Longitude, place.ID, mapsURL,
		input.Category, input.Subcategory, types, metadata,
	).Scan(pinTargets(&pin)...)
	if err != nil {
		return Pin{}, fmt.Errorf("storing a place pin: %w", err)
	}
	return pin.withLists(), nil
}
