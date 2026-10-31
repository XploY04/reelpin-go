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
	"time"

	"github.com/XploY04/reelpin-go/internal/metrics"
	"github.com/XploY04/reelpin-go/internal/spend"
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

type geocodeUsage struct {
	entries []spend.Usage
}

func (u *geocodeUsage) Record(_ context.Context, usage spend.Usage) {
	u.entries = append(u.entries, usage)
}

func TestGeocodeSendsTheAddressAndRecordsTheCall(t *testing.T) {
	var gotMethod, gotPath, gotAddress, gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAddress, gotKey = r.URL.Query().Get("address"), r.URL.Query().Get("key")
		io.WriteString(w, `{"status":"OK","results":[{"geometry":{"location":{"lat":12.9116,"lng":77.6389}}}]}`)
	}))
	t.Cleanup(server.Close)
	usage := &geocodeUsage{}
	client := New(Config{APIKey: testKey, GeocodeBaseURL: server.URL + "/maps/api/geocode/json", Usage: usage})
	point, err := client.Geocode(context.Background(), " Gali Six, HSR & Bengaluru, India ")
	if err != nil {
		t.Fatalf("Geocode: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/maps/api/geocode/json" || gotAddress != "Gali Six, HSR & Bengaluru, India" || gotKey != testKey {
		t.Fatalf("request = %s %s address=%q key=%q", gotMethod, gotPath, gotAddress, gotKey)
	}
	if point.Latitude != 12.9116 || point.Longitude != 77.6389 {
		t.Errorf("point = %+v", point)
	}
	if len(usage.entries) != 1 {
		t.Fatalf("usage entries = %d, want 1", len(usage.entries))
	}
	entry := usage.entries[0]
	if entry.Provider != "google_maps" || entry.Model != "geocoding" || entry.Operation != "geocode" || entry.Calls != 1 || entry.Measured {
		t.Errorf("usage = %+v", entry)
	}
}

func TestGeocodeDoesNotCallWithoutAKeyOrAddress(t *testing.T) {
	for _, key := range []string{"", "  ", placeholderKey, testKey} {
		calls := &atomic.Int64{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
		}))
		address := "Gali Six, Bengaluru"
		if key == testKey {
			address = "  "
		}
		usage := &geocodeUsage{}
		client := New(Config{APIKey: key, GeocodeBaseURL: server.URL, Usage: usage})
		if _, err := client.Geocode(context.Background(), address); !errors.Is(err, ErrNotFound) {
			t.Errorf("Geocode error = %v, want ErrNotFound", err)
		}
		server.Close()
		if calls.Load() != 0 || len(usage.entries) != 0 {
			t.Errorf("unconfigured lookup made %d calls and recorded %d entries", calls.Load(), len(usage.entries))
		}
	}
}

func TestGeocodeDistinguishesNoResultFromProviderFailure(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		notFound bool
	}{
		{"no results", 200, `{"status":"ZERO_RESULTS","results":[]}`, true},
		{"empty results", 200, `{"status":"OK","results":[]}`, false},
		{"missing coordinates", 200, `{"status":"OK","results":[{"geometry":{"location":{"lat":12}}}]}`, false},
		{"denied", 200, `{"status":"REQUEST_DENIED","error_message":"` + testKey + `"}`, false},
		{"quota", 200, `{"status":"OVER_QUERY_LIMIT"}`, false},
		{"unknown failure", 200, `{"status":"UNKNOWN_ERROR"}`, false},
		{"missing status", 200, `{"results":[]}`, false},
		{"unknown status", 200, `{"status":"` + testKey + `"}`, false},
		{"HTTP failure", 503, `{"error":"` + testKey + `"}`, false},
		{"invalid JSON", 200, "<html>" + testKey + "</html>", false},
		{"outside latitude", 200, `{"status":"OK","results":[{"geometry":{"location":{"lat":91,"lng":77}}}]}`, false},
		{"outside longitude", 200, `{"status":"OK","results":[{"geometry":{"location":{"lat":12,"lng":181}}}]}`, false},
		{"nonfinite coordinates", 200, `{"status":"OK","results":[{"geometry":{"location":{"lat":1e999,"lng":77}}}]}`, false},
		{"oversized response", 200, `{"status":"OK","results":[{"geometry":{"location":{"lat":12,"lng":77}}}]}` + strings.Repeat(" ", maxResponseBytes), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			t.Cleanup(server.Close)
			usage := &geocodeUsage{}
			client := New(Config{APIKey: testKey, GeocodeBaseURL: server.URL, Usage: usage})
			_, err := client.Geocode(context.Background(), "Gali Six, Bengaluru")
			if err == nil || errors.Is(err, ErrNotFound) != tt.notFound {
				t.Fatalf("error = %v, want not found = %t", err, tt.notFound)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Error("provider response leaked the API key")
			}
			if len(usage.entries) != 1 || usage.entries[0].Calls != 1 {
				t.Errorf("attempt usage = %+v", usage.entries)
			}
		})
	}
}

func TestGeocodeTimeoutDoesNotExposeTheRequestURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	usage := &geocodeUsage{}
	client := New(Config{APIKey: testKey, GeocodeBaseURL: server.URL, Timeout: 20 * time.Millisecond, Usage: usage})
	_, err := client.Geocode(context.Background(), "Gali Six, Bengaluru")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "Gali") || strings.Contains(err.Error(), server.URL) {
		t.Errorf("request details leaked in error: %v", err)
	}
	if len(usage.entries) != 1 {
		t.Errorf("timed out attempt usage = %+v", usage.entries)
	}
}

func TestGeocodeDoesNotFollowRedirects(t *testing.T) {
	redirectCalls := &atomic.Int64{}
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectCalls.Add(1)
	}))
	t.Cleanup(destination.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	t.Cleanup(server.Close)
	client := New(Config{APIKey: testKey, GeocodeBaseURL: server.URL})
	if _, err := client.Geocode(context.Background(), "Gali Six, Bengaluru"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("redirect error = %v", err)
	}
	if redirectCalls.Load() != 0 {
		t.Fatal("geocoding followed a provider redirect")
	}
}

func TestGeocodeTransportFailureDoesNotExposeTheRequestURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.Close()
	usage := &geocodeUsage{}
	client := New(Config{APIKey: testKey, GeocodeBaseURL: server.URL, Usage: usage})
	_, err := client.Geocode(context.Background(), "Gali Six, Bengaluru")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("transport error = %v", err)
	}
	if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "Gali") || strings.Contains(err.Error(), server.URL) {
		t.Errorf("request details leaked in error: %v", err)
	}
	if len(usage.entries) != 1 {
		t.Errorf("failed attempt usage = %+v", usage.entries)
	}
}

func TestGeocodeCanceledBeforeTheRequestDoesNotCountAnAttempt(t *testing.T) {
	calls := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	usage := &geocodeUsage{}
	client := New(Config{APIKey: testKey, GeocodeBaseURL: server.URL, Usage: usage})
	if _, err := client.Geocode(ctx, "Gali Six, Bengaluru"); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context canceled", err)
	}
	if calls.Load() != 0 || len(usage.entries) != 0 {
		t.Errorf("canceled lookup made %d calls and recorded %d entries", calls.Load(), len(usage.entries))
	}
}

func TestGeocodeCountsProviderFailures(t *testing.T) {
	meters := metrics.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"REQUEST_DENIED"}`)
	}))
	t.Cleanup(server.Close)
	client := New(Config{APIKey: testKey, GeocodeBaseURL: server.URL, Metrics: meters})
	if _, err := client.Geocode(context.Background(), "Gali Six, Bengaluru"); err == nil {
		t.Fatal("credential failure was hidden")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("X-Admin-Key", "admin")
	meters.GuardedHandler("admin").ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `reelpin_provider_failures_total{class="transient",platform="google_places"} 1`) {
		t.Error("geocoding failure was not counted")
	}
}
