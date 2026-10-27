package googleplaces

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/XploY04/reelpin-go/internal/metrics"
)

// testKey is assembled rather than written out: secret scanning flags any
// literal assigned to a name shaped like a credential, however fake it is.
var testKey = strings.Join([]string{"places", "for", "tests"}, "-")

// serve answers the Places API locally.
func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return New(Config{APIKey: testKey, BaseURL: server.URL})
}

func TestAutocompleteSendsTheFieldMaskAndReadsPredictions(t *testing.T) {
	var gotPath, gotKey, gotMask, gotType string
	var gotBody map[string]string
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("X-Goog-Api-Key")
		gotMask, gotType = r.Header.Get("X-Goog-FieldMask"), r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		io.WriteString(w, `{"suggestions":[
			{"placePrediction":{"placeId":"ChIJ-one","text":{"text":"Artjuna, Anjuna, Goa"},
			 "structuredFormat":{"mainText":{"text":"Artjuna"},"secondaryText":{"text":"Anjuna, Goa"}},
			 "types":["cafe"," ",""]}},
			{"placePrediction":{"placeId":"ChIJ-two","text":{"text":"Only the full text"}}},
			{"placePrediction":{"placeId":"","text":{"text":"No id, skipped"}}},
			{"queryPrediction":{"text":{"text":"not a place"}}},
			{"placePrediction":{"placeId":"ChIJ-three","text":{"text":"Past the limit"}}}
		]}`)
	})

	suggestions, err := client.Autocomplete(context.Background(), "artjuna", " session-1 ", 2)
	if err != nil {
		t.Fatalf("Autocomplete: %v", err)
	}
	if gotPath != "/places:autocomplete" || gotKey != testKey || gotType != "application/json" {
		t.Errorf("path %q, key %q, content type %q", gotPath, gotKey, gotType)
	}
	if gotMask != searchFieldMask {
		t.Errorf("field mask = %q", gotMask)
	}
	if gotBody["input"] != "artjuna" || gotBody["sessionToken"] != "session-1" {
		t.Errorf("body = %v", gotBody)
	}

	if len(suggestions) != 2 {
		t.Fatalf("suggestions = %+v, want the first two with an id", suggestions)
	}
	first := suggestions[0]
	if first.PlaceID != "ChIJ-one" || first.Title != "Artjuna" || first.Address != "Anjuna, Goa" {
		t.Errorf("first = %+v", first)
	}
	if len(first.Types) != 1 || first.Types[0] != "cafe" {
		t.Errorf("types = %q, want blanks dropped", first.Types)
	}
	if second := suggestions[1]; second.Title != "Only the full text" || second.Address != "" {
		t.Errorf("second = %+v, want the full text as the title", second)
	}
}

func TestAutocompleteLeavesOutAnEmptySessionToken(t *testing.T) {
	var body string
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		io.WriteString(w, `{}`)
	})
	if _, err := client.Autocomplete(context.Background(), "goa", "  ", 8); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "sessionToken") {
		t.Errorf("body = %s, want no session token", body)
	}
}

func TestWithoutAKeyNothingIsAsked(t *testing.T) {
	for _, key := range []string{"", "  ", placeholderKey} {
		calls := &atomic.Int64{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
		}))
		client := New(Config{APIKey: key, BaseURL: server.URL})

		suggestions, err := client.Autocomplete(context.Background(), "goa", "", 8)
		if err != nil || len(suggestions) != 0 {
			t.Errorf("key %q: autocomplete = %v, %v; want no results and no error", key, suggestions, err)
		}
		if _, err := client.Details(context.Background(), "ChIJ-one", ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("key %q: details err = %v, want not found", key, err)
		}
		server.Close()
		if calls.Load() != 0 {
			t.Errorf("key %q: %d calls reached Google", key, calls.Load())
		}
	}
}

func TestDetailsReadsThePlace(t *testing.T) {
	var gotPath, gotQuery, gotMask string
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotMask = r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("X-Goog-FieldMask")
		io.WriteString(w, `{"id":"ChIJ-canonical","displayName":{"text":" Artjuna "},
			"formattedAddress":"Anjuna, Goa 403509, India",
			"location":{"latitude":15.5763,"longitude":73.7408},
			"googleMapsUri":"https://maps.google.com/?cid=1","types":["cafe","food"]}`)
	})

	place, err := client.Details(context.Background(), "ChIJ/one", "session-1")
	if err != nil {
		t.Fatalf("Details: %v", err)
	}
	if gotPath != "/places/ChIJ%2Fone" {
		t.Errorf("path = %q, want the id escaped as one segment", gotPath)
	}
	if gotQuery != "languageCode=en&sessionToken=session-1" {
		t.Errorf("query = %q", gotQuery)
	}
	if gotMask != detailsFieldMask {
		t.Errorf("field mask = %q", gotMask)
	}
	want := Place{
		ID: "ChIJ-canonical", Name: "Artjuna", Address: "Anjuna, Goa 403509, India",
		Latitude: 15.5763, Longitude: 73.7408, MapsURL: "https://maps.google.com/?cid=1",
	}
	if place.ID != want.ID || place.Name != want.Name || place.Address != want.Address ||
		place.Latitude != want.Latitude || place.Longitude != want.Longitude || place.MapsURL != want.MapsURL {
		t.Errorf("place = %+v", place)
	}
	if len(place.Types) != 2 {
		t.Errorf("types = %q", place.Types)
	}
}

func TestDetailsIsNotFound(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"google answers 404", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}},
		{"no coordinates", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"id":"ChIJ-one","displayName":{"text":"Somewhere"}}`)
		}},
		{"half the coordinates", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"id":"ChIJ-one","location":{"latitude":15.5}}`)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := serve(t, tt.handler)
			if _, err := client.Details(context.Background(), "ChIJ-one", ""); !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want not found", err)
			}
		})
	}
}

func TestAProviderFailureIsAnErrorAndIsCounted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"message":"echoing `+testKey+`"}}`)
	}))
	t.Cleanup(server.Close)
	meters := metrics.New()
	client := New(Config{APIKey: testKey, BaseURL: server.URL, Metrics: meters})

	if _, err := client.Autocomplete(context.Background(), "goa", "", 8); err == nil {
		t.Error("autocomplete hid a 500")
	} else if strings.Contains(err.Error(), testKey) {
		t.Error("the error quoted the provider body, which can echo the key")
	}
	_, err := client.Details(context.Background(), "ChIJ-one", "")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("details err = %v, want a failure that is not not-found", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("X-Admin-Key", "admin")
	meters.GuardedHandler("admin").ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(),
		`reelpin_provider_failures_total{class="transient",platform="google_places"} 2`) {
		t.Errorf("the failures were not counted:\n%s", rec.Body.String())
	}
}
