package apify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func withServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	previous := baseURL
	baseURL = server.URL
	t.Cleanup(func() {
		baseURL = previous
		server.Close()
	})
	return server
}

func TestRunReturnsDatasetItems(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`[{"videoUrl":"https://cdn.example.com/v.mp4"}]`))
	})

	client := New(Config{Token: "token", Actors: map[string]string{"instagram": "apify/instagram-scraper"}})
	items, err := client.Run(context.Background(), "instagram", map[string]any{"directUrls": []string{"https://x"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}

	// The actor id is configuration, and the slash is escaped for the path.
	if !strings.Contains(gotPath, "apify~instagram-scraper") {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer token" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotBody["directUrls"] == nil {
		t.Errorf("body = %v", gotBody)
	}
}

func TestUnconfiguredPlatformsFallBackInsteadOfFailing(t *testing.T) {
	client := New(Config{Token: "token", Actors: map[string]string{"instagram": "apify/scraper"}})

	if client.Configured("youtube") {
		t.Error("a platform with no actor reported as configured")
	}
	if _, err := client.Run(context.Background(), "youtube", nil); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured so the caller falls back", err)
	}

	// No token means nothing is configured, whatever the actor map says.
	if New(Config{Actors: map[string]string{"instagram": "apify/scraper"}}).Configured("instagram") {
		t.Error("an actor without a token reported as configured")
	}
}

func TestRateLimitIsItsOwnError(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	client := New(Config{Token: "token", Actors: map[string]string{"instagram": "a/b"}})
	if _, err := client.Run(context.Background(), "instagram", nil); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestFailuresDoNotEchoTheActorBody(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad input https://www.instagram.com/reel/PRIVATE/"}`))
	})

	client := New(Config{Token: "token", Actors: map[string]string{"instagram": "a/b"}})
	_, err := client.Run(context.Background(), "instagram", nil)
	if err == nil {
		t.Fatal("a rejected run reported success")
	}
	if strings.Contains(err.Error(), "instagram.com") {
		t.Fatalf("the error echoes the actor body: %v", err)
	}
}
