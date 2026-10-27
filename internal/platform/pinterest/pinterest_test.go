package pinterest

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

// pinterestCDN is the host the pin fixture points its og:image at.
const pinterestCDN = "https://i.pinimg.com"

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

// newHandler wires one handler. A nil uploader is a deployment with no
// storage credential, and the handler is expected to survive it.
func newHandler(uploader storage.Uploader) *Handler {
	client := safehttp.New(safehttp.Config{AllowPrivateAddresses: true})
	limits := providers.NewLimits()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(Deps{
		HTTP:       client,
		Thumbnails: platform.Thumbnails{HTTP: client, Storage: uploader, Limits: limits, Logger: logger},
		Limit:      limits,
		Logger:     logger,
	})
}

func identityFor(url string) sourceidentity.SourceIdentity {
	return sourceidentity.SourceIdentity{
		NormalizedURL: url,
		OriginalURL:   url,
		Platform:      PlatformName,
		ContentType:   "pin",
		ContentID:     "12345",
	}
}

func TestAPinIsAlwaysLightWork(t *testing.T) {
	server := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(fixture(t, "pin.html")))
	})

	prepared, err := newHandler(nil).Prepare(context.Background(), identityFor(server.URL))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.NeedsMedia {
		t.Fatal("a pin asked for media; there is no audio on a picture")
	}
	if !strings.Contains(prepared.Caption, "Walnut shelf") {
		t.Errorf("caption = %q", prepared.Caption)
	}
	// No uploader is a deployment with no storage credential. The pin still
	// saves; it just saves without a preview.
	if prepared.ThumbnailURL != "" {
		t.Errorf("thumbnail = %q with no uploader configured, want none", prepared.ThumbnailURL)
	}
}

func TestAPinThumbnailIsStoredRatherThanLinked(t *testing.T) {
	server := platformtest.Site(t, fixture(t, "pin.html"), pinterestCDN)
	uploader := &platformtest.Uploader{}

	prepared, err := newHandler(uploader).Prepare(context.Background(), identityFor(server.URL))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if uploader.Uploads != 1 {
		t.Fatalf("uploaded %d previews, want 1", uploader.Uploads)
	}
	// The reader renders images out of our own bucket and nowhere else, so a
	// pinimg.com URL saved as-is is a pin with no preview at all.
	if !strings.HasPrefix(prepared.ThumbnailURL, platformtest.StoredPrefix) {
		t.Errorf("thumbnail = %q, want the stored object", prepared.ThumbnailURL)
	}
}

func TestAFailedThumbnailUploadStillSavesThePin(t *testing.T) {
	server := platformtest.Site(t, fixture(t, "pin.html"), pinterestCDN)
	uploader := &platformtest.Uploader{Err: errors.New("the bucket refused")}

	prepared, err := newHandler(uploader).Prepare(context.Background(), identityFor(server.URL))
	if err != nil {
		t.Fatalf("Prepare: %v: a missing preview must not fail the run", err)
	}
	if prepared.ThumbnailURL != "" {
		t.Errorf("thumbnail = %q after a failed upload, want none", prepared.ThumbnailURL)
	}
	if !strings.Contains(prepared.Caption, "Walnut shelf") {
		t.Errorf("caption = %q: the pin itself was lost with the preview", prepared.Caption)
	}
}

func TestAnEmptyPinIsTerminal(t *testing.T) {
	server := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(fixture(t, "empty.html")))
	})

	_, err := newHandler(nil).Prepare(context.Background(), identityFor(server.URL))
	assertFailure(t, err, "page_empty", false)
}

func TestPinStatusesAreClassified(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		code      string
		retryable bool
	}{
		{"deleted", http.StatusNotFound, "content_unavailable", false},
		{"secret board", http.StatusForbidden, "login_required", false},
		{"throttled", http.StatusTooManyRequests, "provider_rate_limited", true},
		{"broken", http.StatusInternalServerError, "provider_unavailable", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			})
			_, err := newHandler(nil).Prepare(context.Background(), identityFor(server.URL))
			assertFailure(t, err, tt.code, tt.retryable)
		})
	}
}

func TestDownloadIsRefusedRatherThanPanicking(t *testing.T) {
	_, err := newHandler(nil).Download(context.Background(),
		identityFor("https://pin.it/abc"), t.TempDir())
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
