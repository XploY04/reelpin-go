package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/XploY04/reelpin-go/internal/googleplaces"
	"github.com/XploY04/reelpin-go/internal/mapview"
)

// fakePlaceStore holds one user's pins in memory, so a place search and a
// place pin run through the real domain logic without a database.
type fakePlaceStore struct {
	candidates []mapview.Candidate
	byPlace    map[string]mapview.Pin
	upserted   []mapview.PlacePin
	lastUserID string
}

func (f *fakePlaceStore) PlaceCandidates(_ context.Context, userID string) ([]mapview.Candidate, error) {
	f.lastUserID = userID
	return f.candidates, nil
}

func (f *fakePlaceStore) PinByPlace(_ context.Context, userID, placeID string) (mapview.Pin, error) {
	f.lastUserID = userID
	pin, ok := f.byPlace[placeID]
	if !ok {
		return mapview.Pin{}, mapview.ErrNotFound
	}
	return pin, nil
}

func (f *fakePlaceStore) UpsertPlacePin(_ context.Context, userID string, input mapview.PlacePin) (mapview.Pin, error) {
	f.lastUserID = userID
	f.upserted = append(f.upserted, input)
	placeID, mapsURL := input.Place.ID, input.Place.MapsURL
	return mapview.Pin{
		ID: "66666666-6666-4666-8666-666666666666", Kind: "manual", Name: input.Place.Name,
		Address: &input.Place.Address, Latitude: input.Place.Latitude, Longitude: input.Place.Longitude,
		Category: &input.Category, Subcategory: &input.Subcategory,
		GooglePlaceID: &placeID, GoogleMapsURL: &mapsURL, PlaceTypes: input.Place.Types,
	}, nil
}

var discardLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))

// unconfiguredPlaces has no Google key, which is how a deployment without one
// runs: nothing reaches Google at all.
func unconfiguredPlaces(store *fakePlaceStore) *mapview.Places {
	return mapview.NewPlaces(store, googleplaces.New(googleplaces.Config{}), discardLogger)
}

// googleServer answers the Places API locally and counts the calls, which is
// what tells "the existing pin was returned" apart from "Google was asked".
func googleServer(t *testing.T, handler http.HandlerFunc) (*googleplaces.Client, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	key := strings.Join([]string{"places", "for", "tests"}, "-")
	return googleplaces.New(googleplaces.Config{APIKey: key, BaseURL: server.URL}), calls
}

func placesDeps(store *fakePlaceStore, finder mapview.PlaceFinder) Deps {
	deps := testDeps(&fakePinger{})
	deps.MapPlaces = mapview.NewPlaces(store, finder, discardLogger)
	deps.Limiter = allowAllLimiter{}
	return deps
}

// goaPins is a content pin whose save mentions Goa and a manual pin made from
// a Google place in Goa.
func goaPins() []mapview.Candidate {
	return []mapview.Candidate{
		{
			Pin: mapview.Pin{
				ID: "77777777-7777-4777-8777-777777777777", Kind: "content", Name: "Artjuna",
				Latitude: 15.5763, Longitude: 73.7408, ReelID: stringPtr(testReelID),
				Category: stringPtr("Food"), PlaceTypes: []string{},
			},
			Title: "Best cafes in Goa", Summary: "Three cafes worth the drive",
		},
		{
			Pin: mapview.Pin{
				ID: "88888888-8888-4888-8888-888888888888", Kind: "manual", Name: "Thalassa",
				Address: stringPtr("Small Vagator, Goa"), Latitude: 15.6012, Longitude: 73.7346,
				Category: stringPtr("Other"), GooglePlaceID: stringPtr("ChIJ-thalassa"),
				PlaceTypes: []string{"restaurant"},
			},
			Title: "Thalassa", Summary: "Small Vagator, Goa",
		},
	}
}

type placeSearchBody struct {
	Query      string `json:"query"`
	SearchMode string `json:"search_mode"`
	Total      int    `json:"total"`
	Results    []struct {
		ResultType    string       `json:"result_type"`
		SourceType    *string      `json:"source_type"`
		MapItem       *mapview.Pin `json:"map_item"`
		GooglePlaceID *string      `json:"google_place_id"`
		DisplayTitle  string       `json:"display_title"`
		PlaceTypes    []string     `json:"place_types"`
		CanPin        bool         `json:"can_pin"`
	} `json:"results"`
}

func searchPlaces(t *testing.T, deps Deps, target string) placeSearchBody {
	t.Helper()
	rec := serve(deps, "GET", target, "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var body placeSearchBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestAPlaceSearchPutsYourPinsFirstAndDropsGoogleDuplicates(t *testing.T) {
	var gotBody string
	finder, _ := googleServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		io.WriteString(w, `{"suggestions":[
			{"placePrediction":{"placeId":"chij thalassa","structuredFormat":{"mainText":{"text":"Thalassa"}}}},
			{"placePrediction":{"placeId":"ChIJ-beach","structuredFormat":{"mainText":{"text":"Vagator Beach"},"secondaryText":{"text":"small vagator goa"}}}},
			{"placePrediction":{"placeId":"ChIJ-airport","structuredFormat":{"mainText":{"text":"Goa Airport"},"secondaryText":{"text":"Dabolim, Goa"}},"types":["airport"]}},
			{"placePrediction":{"placeId":"ChIJ-airport","structuredFormat":{"mainText":{"text":"Goa Airport again"}}}}
		]}`)
	})
	store := &fakePlaceStore{candidates: goaPins()}

	body := searchPlaces(t, placesDeps(store, finder), "/api/v2/map/search?query=%20goa%20&session_token=tok-1")

	if store.lastUserID != testUserID {
		t.Errorf("read pins for %q, want the token subject", store.lastUserID)
	}
	if !strings.Contains(gotBody, `"input":"goa"`) || !strings.Contains(gotBody, `"sessionToken":"tok-1"`) {
		t.Errorf("google was asked %s", gotBody)
	}
	if body.Query != "goa" || body.SearchMode != "mixed" || body.Total != 3 || len(body.Results) != 3 {
		t.Fatalf("body = %+v, want two pins and one new Google place", body)
	}
	for i, want := range []string{"Best cafes in Goa", "Thalassa"} {
		result := body.Results[i]
		if result.ResultType != "existing" || result.CanPin || result.MapItem == nil || result.DisplayTitle != want {
			t.Errorf("result %d = %+v, want the existing pin %q", i, result, want)
		}
	}
	if kind := body.Results[0].SourceType; kind == nil || *kind != "content" {
		t.Errorf("source type = %v, want content", kind)
	}
	google := body.Results[2]
	if google.ResultType != "google" || !google.CanPin || google.MapItem != nil ||
		google.GooglePlaceID == nil || *google.GooglePlaceID != "ChIJ-airport" {
		t.Errorf("google result = %+v, want the airport alone", google)
	}
	if len(google.PlaceTypes) != 1 {
		t.Errorf("place types = %q", google.PlaceTypes)
	}
}

func TestAPlaceSearchNamesWhereItsAnswerCameFrom(t *testing.T) {
	airport := `{"suggestions":[{"placePrediction":{"placeId":"ChIJ-airport","text":{"text":"Goa Airport"}}}]}`
	tests := []struct {
		name, query, google, mode string
		total                     int
	}{
		{"only your pins", "thalassa", `{}`, "existing", 1},
		{"only google", "airport", airport, "google", 1},
		{"nothing at all", "zzzz", `{}`, "google", 0},
		{"a category narrows your pins", "goa&category=Food", `{}`, "existing", 1},
		{"tokens are enough", "best%20goa%20spots", `{}`, "existing", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			finder, _ := googleServer(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tt.google)
			})
			body := searchPlaces(t, placesDeps(&fakePlaceStore{candidates: goaPins()}, finder),
				"/api/v2/map/search?query="+tt.query)
			if body.SearchMode != tt.mode || body.Total != tt.total {
				t.Fatalf("mode %q total %d, want %q and %d", body.SearchMode, body.Total, tt.mode, tt.total)
			}
		})
	}
}

func TestAPlaceSearchWithoutGoogleStillAnswersYourPins(t *testing.T) {
	broken, calls := googleServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	for name, finder := range map[string]mapview.PlaceFinder{
		"no key":         googleplaces.New(googleplaces.Config{}),
		"google failing": broken,
	} {
		t.Run(name, func(t *testing.T) {
			body := searchPlaces(t, placesDeps(&fakePlaceStore{candidates: goaPins()}, finder),
				"/api/v2/map/search?query=goa")
			if body.SearchMode != "existing" || body.Total != 2 {
				t.Fatalf("mode %q total %d, want the two pins", body.SearchMode, body.Total)
			}
		})
	}
	if calls.Load() != 1 {
		t.Errorf("google was asked %d times, want once, by the failing server alone", calls.Load())
	}
}

func TestAPlaceSearchValidatesItsQuery(t *testing.T) {
	for _, target := range []string{
		"/api/v2/map/search",
		"/api/v2/map/search?query=%20g%20",
		"/api/v2/map/search?query=goa&limit=0",
		"/api/v2/map/search?query=goa&limit=11",
	} {
		rec := serve(placesDeps(&fakePlaceStore{}, googleplaces.New(googleplaces.Config{})), "GET", target, "Bearer good.token")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", target, rec.Code)
		}
	}
}

func TestAPlaceSearchFailsClosedWithoutALimiter(t *testing.T) {
	deps := placesDeps(&fakePlaceStore{}, googleplaces.New(googleplaces.Config{}))
	deps.Limiter = nil
	rec := serve(deps, "GET", "/api/v2/map/search?query=goa", "Bearer good.token")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: a Google call must never be unmetered", rec.Code)
	}
}

func pinPlace(deps Deps, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v2/map/pins", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer good.token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	routes(deps).ServeHTTP(rec, req)
	return rec
}

func TestPinningAPlaceTwiceReturnsTheFirstPinWithoutAskingGoogle(t *testing.T) {
	finder, calls := googleServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("google was asked for a place the user has already pinned")
	})
	existing := goaPins()[1].Pin
	store := &fakePlaceStore{byPlace: map[string]mapview.Pin{"ChIJ-thalassa": existing}}

	rec := pinPlace(placesDeps(store, finder), `{"googlePlaceId":" ChIJ-thalassa "}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var pin mapview.Pin
	if err := json.Unmarshal(rec.Body.Bytes(), &pin); err != nil {
		t.Fatal(err)
	}
	if pin.ID != existing.ID || calls.Load() != 0 || len(store.upserted) != 0 {
		t.Fatalf("pin %s, %d google calls, %d writes; want the existing pin and nothing else",
			pin.ID, calls.Load(), len(store.upserted))
	}
}

func TestPinningANewPlaceStoresItsDetails(t *testing.T) {
	var gotPath, gotQuery string
	finder, _ := googleServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		io.WriteString(w, `{"id":"ChIJ-airport","displayName":{"text":"Goa Airport"},
			"formattedAddress":"Dabolim, Goa","location":{"latitude":15.38,"longitude":73.83},
			"googleMapsUri":"https://maps.google.com/?cid=2","types":["airport"]}`)
	})
	store := &fakePlaceStore{}

	rec := pinPlace(placesDeps(store, finder), `{"google_place_id":"ChIJ-airport","session_token":"tok-1"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if gotPath != "/places/ChIJ-airport" || !strings.Contains(gotQuery, "sessionToken=tok-1") {
		t.Errorf("google was asked %s?%s", gotPath, gotQuery)
	}
	if store.lastUserID != testUserID || len(store.upserted) != 1 {
		t.Fatalf("stored %d pins for %q", len(store.upserted), store.lastUserID)
	}
	stored := store.upserted[0]
	if stored.Place.Name != "Goa Airport" || stored.Category != "Other" || stored.Subcategory != "Other" {
		t.Errorf("stored %+v, want the details under the default category", stored)
	}
	var pin mapview.Pin
	if err := json.Unmarshal(rec.Body.Bytes(), &pin); err != nil {
		t.Fatal(err)
	}
	if pin.Kind != "manual" || pin.GooglePlaceID == nil || *pin.GooglePlaceID != "ChIJ-airport" {
		t.Errorf("pin = %+v", pin)
	}
}

func TestPinningAPlaceGoogleCannotFindIsNotFound(t *testing.T) {
	missing, _ := googleServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	for name, finder := range map[string]mapview.PlaceFinder{
		"google answers 404": missing,
		"no key":             googleplaces.New(googleplaces.Config{}),
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakePlaceStore{}
			rec := pinPlace(placesDeps(store, finder), `{"google_place_id":"ChIJ-nowhere"}`)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeError(t, rec).Error.Code; code != "place_not_found" {
				t.Errorf("code = %q", code)
			}
			if len(store.upserted) != 0 {
				t.Error("a place that was not found was stored")
			}
		})
	}
}

func TestPinningAPlaceNeedsAnID(t *testing.T) {
	for _, body := range []string{`{}`, `{"google_place_id":"  "}`, `{"place":"ChIJ-one"}`} {
		rec := pinPlace(placesDeps(&fakePlaceStore{}, googleplaces.New(googleplaces.Config{})), body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", body, rec.Code)
		}
	}
}

func TestPlaceRoutesRequireASession(t *testing.T) {
	deps := placesDeps(&fakePlaceStore{}, googleplaces.New(googleplaces.Config{}))
	for _, request := range [][2]string{
		{"GET", "/api/v2/map/search?query=goa"},
		{"POST", "/api/v2/map/pins"},
	} {
		if rec := serve(deps, request[0], request[1], ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d, want 401", request[0], request[1], rec.Code)
		}
	}
}
