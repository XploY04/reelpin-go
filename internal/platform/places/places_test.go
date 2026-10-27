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
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/platform/platformtest"
	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/XploY04/reelpin-go/internal/storage"
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

// placesCDN is the host the listing fixture points its og:image at.
const placesCDN = "https://maps.example.com"

// deps wires one set of dependencies. A nil uploader is a deployment with no
// storage credential, and the handler is expected to survive it.
func deps(uploader storage.Uploader) Deps {
	client := safehttp.New(safehttp.Config{AllowPrivateAddresses: true})
	limits := providers.NewLimits()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return Deps{
		HTTP:       client,
		Thumbnails: platform.Thumbnails{HTTP: client, Storage: uploader, Limits: limits, Logger: logger},
		Limit:      limits,
		Logger:     logger,
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
	handlers := Handlers(deps(nil))
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

	prepared, err := New("google_maps", deps(nil)).
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
	// No uploader is a deployment with no storage credential. The listing
	// still saves; it just saves without a preview.
	if prepared.ThumbnailURL != "" {
		t.Errorf("thumbnail = %q with no uploader configured, want none", prepared.ThumbnailURL)
	}
}

func TestAListingThumbnailIsStoredRatherThanLinked(t *testing.T) {
	server := platformtest.Site(t, fixture(t, "listing.html"), placesCDN)
	uploader := &platformtest.Uploader{}

	prepared, err := New("google_maps", deps(uploader)).
		Prepare(context.Background(), identityFor(server.URL, "google_maps"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if uploader.Uploads != 1 {
		t.Fatalf("uploaded %d previews, want 1", uploader.Uploads)
	}
	// The reader renders images out of our own bucket and nowhere else, so a
	// listing's own CDN URL saved as-is is a place with no preview at all.
	if !strings.HasPrefix(prepared.ThumbnailURL, platformtest.StoredPrefix) {
		t.Errorf("thumbnail = %q, want the stored object", prepared.ThumbnailURL)
	}
}

func TestAFailedThumbnailUploadStillSavesTheListing(t *testing.T) {
	server := platformtest.Site(t, fixture(t, "listing.html"), placesCDN)
	uploader := &platformtest.Uploader{Err: errors.New("the bucket refused")}

	prepared, err := New("google_maps", deps(uploader)).
		Prepare(context.Background(), identityFor(server.URL, "google_maps"))
	if err != nil {
		t.Fatalf("Prepare: %v: a missing preview must not fail the run", err)
	}
	if prepared.ThumbnailURL != "" {
		t.Errorf("thumbnail = %q after a failed upload, want none", prepared.ThumbnailURL)
	}
	if !strings.Contains(prepared.PageText, "Monteiro Vaddo") {
		t.Error("the listing itself was lost with the preview")
	}
}

func TestAnEmptyListingIsTerminal(t *testing.T) {
	server := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<!doctype html><html><head></head><body></body></html>`))
	})

	_, err := New("zomato", deps(nil)).
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
			_, err := New("tripadvisor", deps(nil)).
				Prepare(context.Background(), identityFor(server.URL, "tripadvisor"))
			assertFailure(t, err, tt.code, tt.retryable)
		})
	}
}

func TestDownloadIsRefusedRatherThanPanicking(t *testing.T) {
	_, err := New("airbnb", deps(nil)).Download(context.Background(),
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
