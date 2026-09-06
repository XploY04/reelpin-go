package places

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/pipeline"
	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return string(body)
}

func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func deps() Deps {
	return Deps{
		HTTP:   safehttp.New(safehttp.Config{AllowPrivateAddresses: true}),
		Limit:  providers.NewLimits(),
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

func identityFor(url, platformName string) sourceidentity.SourceIdentity {
	return sourceidentity.SourceIdentity{
		NormalizedURL: url,
		OriginalURL:   url,
		Platform:      platformName,
		ContentType:   "place",
		ContentID:     "place-1",
	}
}

func TestEveryPlacePlatformGetsItsOwnRegistration(t *testing.T) {
	// The registry is keyed by platform, so one shared implementation still
	// needs one handler per name. A missing name is an unsupported_platform
	// failure at runtime, which is why this is asserted rather than assumed.
	handlers := Handlers(deps())
	if len(handlers) != len(Platforms) {
		t.Fatalf("built %d handlers for %d platforms", len(handlers), len(Platforms))
	}

	seen := map[string]bool{}
	for _, handler := range handlers {
		name := handler.Platform()
		if seen[name] {
			t.Fatalf("platform %q registered twice", name)
		}
		seen[name] = true
	}
	for _, name := range Platforms {
		if !seen[name] {
			t.Errorf("platform %q has no handler", name)
		}
	}
}

func TestAListingIsLightWorkWithItsProse(t *testing.T) {
	server := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(fixture(t, "listing.html")))
	})

	prepared, err := New("google_maps", deps()).
		Prepare(context.Background(), identityFor(server.URL, "google_maps"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.NeedsMedia {
		t.Fatal("a place page asked for media; probing it would spend a download on a restaurant")
	}
	if !strings.Contains(prepared.Caption, "Artjuna") {
		t.Errorf("caption = %q", prepared.Caption)
	}
	// The address lives in the body, not the preview tags, and it is the part
	// the extractor turns into a location.
	if !strings.Contains(prepared.PageText, "Monteiro Vaddo") {
		t.Errorf("page text lost the address:\n%s", prepared.PageText)
	}
}

func TestAnEmptyListingIsTerminal(t *testing.T) {
	server := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<!doctype html><html><head></head><body></body></html>`))
	})

	_, err := New("zomato", deps()).
		Prepare(context.Background(), identityFor(server.URL, "zomato"))
	assertFailure(t, err, "page_empty", false)
}

func TestPlaceStatusesAreClassified(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		code      string
		retryable bool
	}{
		{"closed permanently", http.StatusNotFound, "content_unavailable", false},
		{"blocked", http.StatusForbidden, "login_required", false},
		{"throttled", http.StatusTooManyRequests, "provider_rate_limited", true},
		{"broken", http.StatusServiceUnavailable, "provider_unavailable", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			})
			_, err := New("tripadvisor", deps()).
				Prepare(context.Background(), identityFor(server.URL, "tripadvisor"))
			assertFailure(t, err, tt.code, tt.retryable)
		})
	}
}

func TestDownloadIsRefusedRatherThanPanicking(t *testing.T) {
	_, err := New("airbnb", deps()).Download(context.Background(),
		identityFor("https://airbnb.com/rooms/1", "airbnb"), t.TempDir())
	assertFailure(t, err, "source_not_supported", false)
}

func assertFailure(t *testing.T, err error, wantCode string, retryable bool) {
	t.Helper()
	var failure *pipeline.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want a classified failure", err)
	}
	if failure.Code != wantCode {
		t.Errorf("code = %q, want %q", failure.Code, wantCode)
	}
	if failure.Retryable() != retryable {
		t.Errorf("retryable = %v, want %v", failure.Retryable(), retryable)
	}
}
