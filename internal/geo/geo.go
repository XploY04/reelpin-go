// Package geo turns a place description into coordinates. Geocoding is billed
// per call, so every lookup goes through the shared cache first, including the
// misses: a place Google could not find will not be found next week either.
package geo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Point is a resolved place.
type Point struct {
	Latitude  float64
	Longitude float64
}

// ErrNotFound means the provider answered, and the answer was "no such place".
// It is cached, unlike a transport failure.
var ErrNotFound = errors.New("place not found")

// Geocoder is the seam the pipeline depends on.
type Geocoder interface {
	Geocode(ctx context.Context, query string) (Point, error)
}

// CacheKey normalizes a query so "Artjuna Cafe, Anjuna" and
// "  artjuna  cafe ,  anjuna " are one cache entry.
func CacheKey(query string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(query), " "))
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// Cached wraps a geocoder with the shared geocode_cache table, which the Python
// service already filled with 833 answers.
type Cached struct {
	pool  *pgxpool.Pool
	inner Geocoder
}

func NewCached(pool *pgxpool.Pool, inner Geocoder) *Cached {
	return &Cached{pool: pool, inner: inner}
}

func (c *Cached) Geocode(ctx context.Context, query string) (Point, error) {
	cleaned := strings.TrimSpace(query)
	if cleaned == "" {
		return Point{}, ErrNotFound
	}

	key := CacheKey(cleaned)
	if point, found, err := c.lookup(ctx, key); err == nil && found {
		return point, nil
	} else if err == nil && !found {
		// A cached miss is an answer: do not pay for it again.
		if cachedMiss, err := c.isCachedMiss(ctx, key); err == nil && cachedMiss {
			return Point{}, ErrNotFound
		}
	}

	point, err := c.inner.Geocode(ctx, cleaned)
	if errors.Is(err, ErrNotFound) {
		c.store(ctx, key, cleaned, "not_found", nil)
		return Point{}, err
	}
	if err != nil {
		// A transport failure is not an answer, so nothing is cached.
		return Point{}, err
	}

	c.store(ctx, key, cleaned, "ok", &point)
	return point, nil
}

func (c *Cached) lookup(ctx context.Context, key string) (Point, bool, error) {
	var status string
	var latitude, longitude *float64
	err := c.pool.QueryRow(ctx,
		`SELECT status, latitude, longitude FROM public.geocode_cache WHERE query_key = $1`, key,
	).Scan(&status, &latitude, &longitude)
	if errors.Is(err, pgx.ErrNoRows) {
		return Point{}, false, nil
	}
	if err != nil {
		return Point{}, false, err
	}
	if strings.EqualFold(status, "ok") && latitude != nil && longitude != nil {
		return Point{Latitude: *latitude, Longitude: *longitude}, true, nil
	}
	return Point{}, false, nil
}

func (c *Cached) isCachedMiss(ctx context.Context, key string) (bool, error) {
	var status string
	err := c.pool.QueryRow(ctx,
		`SELECT status FROM public.geocode_cache WHERE query_key = $1`, key,
	).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.EqualFold(status, "not_found"), nil
}

func (c *Cached) store(ctx context.Context, key, query, status string, point *Point) {
	var latitude, longitude any
	if point != nil {
		latitude, longitude = point.Latitude, point.Longitude
	}
	// A cache write failure must never fail the pipeline: the answer is already
	// in hand.
	_, _ = c.pool.Exec(ctx, `
		INSERT INTO public.geocode_cache (query_key, query_text, status, latitude, longitude)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (query_key)
		DO UPDATE SET status = EXCLUDED.status,
		              latitude = EXCLUDED.latitude,
		              longitude = EXCLUDED.longitude,
		              updated_at = now()`,
		key, query, status, latitude, longitude,
	)
}

// Google is the provider. It is a trusted endpoint, so it uses a plain client
// with timeouts rather than the guarded one used for user-supplied URLs.
type Google struct {
	apiKey string
	client *http.Client
}

func NewGoogle(apiKey string, timeout time.Duration) *Google {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Google{apiKey: apiKey, client: &http.Client{Timeout: timeout}}
}

// endpoint is a variable so tests can point it at a local server.
var endpoint = "https://maps.googleapis.com/maps/api/geocode/json"

func (g *Google) Geocode(ctx context.Context, query string) (Point, error) {
	if strings.TrimSpace(g.apiKey) == "" {
		return Point{}, fmt.Errorf("GOOGLE_MAPS_API_KEY is not configured")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		endpoint+"?"+url.Values{"address": {query}, "key": {g.apiKey}}.Encode(), nil)
	if err != nil {
		return Point{}, fmt.Errorf("building the geocode request: %w", err)
	}

	response, err := g.client.Do(request)
	if err != nil {
		return Point{}, fmt.Errorf("calling the geocoder: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return Point{}, fmt.Errorf("the geocoder returned HTTP %d", response.StatusCode)
	}

	var body struct {
		Status  string `json:"status"`
		Results []struct {
			Geometry struct {
				Location struct {
					Lat float64 `json:"lat"`
					Lng float64 `json:"lng"`
				} `json:"location"`
			} `json:"geometry"`
		} `json:"results"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return Point{}, fmt.Errorf("decoding the geocode response: %w", err)
	}

	switch body.Status {
	case "OK":
		if len(body.Results) == 0 {
			return Point{}, ErrNotFound
		}
		location := body.Results[0].Geometry.Location
		return Point{Latitude: location.Lat, Longitude: location.Lng}, nil
	case "ZERO_RESULTS":
		return Point{}, ErrNotFound
	case "OVER_QUERY_LIMIT", "UNKNOWN_ERROR":
		// Transient: worth retrying, so it must not be cached as a miss.
		return Point{}, fmt.Errorf("the geocoder is unavailable: %s", body.Status)
	default:
		return Point{}, fmt.Errorf("the geocoder rejected the request: %s", body.Status)
	}
}

// Queries builds the ordered attempts for a place, most specific first. The
// fallbacks are what make a weakly described place still land on the map.
func Queries(name, neighborhood, city, state, country string) []string {
	specific := join(name, neighborhood, city, state, country)
	withoutNeighborhood := join(name, city, state, country)
	nameOnly := strings.TrimSpace(name)

	queries := []string{}
	for _, candidate := range []string{specific, withoutNeighborhood, nameOnly} {
		if candidate == "" {
			continue
		}
		if !contains(queries, candidate) {
			queries = append(queries, candidate)
		}
	}
	return queries
}

func join(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if cleaned := strings.TrimSpace(part); cleaned != "" {
			kept = append(kept, cleaned)
		}
	}
	return strings.Join(kept, ", ")
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
