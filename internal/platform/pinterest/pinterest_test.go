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

func newHandler() *Handler {
	return New(Deps{
		HTTP:   safehttp.New(safehttp.Config{AllowPrivateAddresses: true}),
		Limit:  providers.NewLimits(),
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
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

	prepared, err := newHandler().Prepare(context.Background(), identityFor(server.URL))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.NeedsMedia {
		t.Fatal("a pin asked for media; there is no audio on a picture")
	}
	if !strings.Contains(prepared.Caption, "Walnut shelf") {
		t.Errorf("caption = %q", prepared.Caption)
	}
	if prepared.ThumbnailURL == "" {
		t.Error("a pin with an og:image produced no thumbnail")
	}
}

func TestAnEmptyPinIsTerminal(t *testing.T) {
	server := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(fixture(t, "empty.html")))
	})

	_, err := newHandler().Prepare(context.Background(), identityFor(server.URL))
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
			_, err := newHandler().Prepare(context.Background(), identityFor(server.URL))
			assertFailure(t, err, tt.code, tt.retryable)
		})
	}
}

func TestDownloadIsRefusedRatherThanPanicking(t *testing.T) {
	_, err := newHandler().Download(context.Background(),
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
