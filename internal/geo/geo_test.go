package geo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The 833 rows the Python service already cached are only reusable if the key
// is computed the same way.
func TestCacheKeyMatchesThePythonImplementation(t *testing.T) {
	base := CacheKey("Artjuna Cafe, Anjuna, Goa, India")
	if len(base) != 64 {
		t.Fatalf("key = %q, want a sha-256 hex digest", base)
	}
	for _, equivalent := range []string{
		"  Artjuna Cafe, Anjuna, Goa, India  ",
		"artjuna cafe, anjuna, goa, india",
		"Artjuna   Cafe,   Anjuna,  Goa,  India",
	} {
		if CacheKey(equivalent) != base {
			t.Errorf("%q produced a different key", equivalent)
		}
	}
	if CacheKey("Artjuna Cafe, Anjuna") == base {
		t.Error("a different query produced the same key")
	}
}

func TestQueriesFallBackFromSpecificToName(t *testing.T) {
	got := Queries("Artjuna Cafe", "Anjuna", "Goa", "", "India")
	want := []string{
		"Artjuna Cafe, Anjuna, Goa, India",
		"Artjuna Cafe, Goa, India",
		"Artjuna Cafe",
	}
	if len(got) != len(want) {
		t.Fatalf("queries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("queries = %v, want %v", got, want)
		}
	}

	// With no neighbourhood there is no second attempt to make.
	if only := Queries("Artjuna Cafe", "", "", "", ""); len(only) != 1 {
		t.Errorf("queries = %v, want one attempt", only)
	}
	if none := Queries("", "", "", "", ""); len(none) != 0 {
		t.Errorf("queries = %v, want none", none)
	}
}

func TestGoogleResponses(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantErr  error
		wantLat  float64
		wantFail bool
	}{
		{
			name:    "found",
			body:    `{"status":"OK","results":[{"geometry":{"location":{"lat":15.58,"lng":73.74}}}]}`,
			wantLat: 15.58,
		},
		{name: "zero results", body: `{"status":"ZERO_RESULTS","results":[]}`, wantErr: ErrNotFound},
		{name: "ok with no results", body: `{"status":"OK","results":[]}`, wantErr: ErrNotFound},
		{name: "over query limit", body: `{"status":"OVER_QUERY_LIMIT"}`, wantFail: true},
		{name: "request denied", body: `{"status":"REQUEST_DENIED"}`, wantFail: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("address") == "" {
					t.Error("the request carried no address")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tt.body))
			}))
			defer server.Close()

			previous := endpoint
			endpoint = server.URL
			defer func() { endpoint = previous }()

			point, err := NewGoogle("test-key", 0).Geocode(context.Background(), "Artjuna Cafe")
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.wantFail:
				if err == nil {
					t.Fatal("a provider failure was reported as success")
				}
				if errors.Is(err, ErrNotFound) {
					t.Fatal("a transient failure was reported as a missing place, which would be cached")
				}
			default:
				if err != nil {
					t.Fatalf("Geocode: %v", err)
				}
				if point.Latitude != tt.wantLat {
					t.Errorf("latitude = %v, want %v", point.Latitude, tt.wantLat)
				}
			}
		})
	}
}

func TestGoogleWithoutAKeyDoesNotCall(t *testing.T) {
	if _, err := NewGoogle("", 0).Geocode(context.Background(), "anywhere"); err == nil {
		t.Fatal("a missing api key was not reported")
	}
}
