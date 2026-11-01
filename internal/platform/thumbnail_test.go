package platform

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/platform/platformtest"
	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

func TestMissingPreviewsReportTheFailedStepWithoutSourceURLs(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		reason string
	}{
		{name: "no source image", reason: "source_missing"},
		{name: "storage unavailable", reason: "storage_unconfigured"},
		{name: "download rejected", status: http.StatusForbidden, body: "private", reason: "http_status"},
		{name: "empty image", status: http.StatusOK, reason: "empty_body"},
		{name: "unsafe source", reason: "fetch_failed"},
		{name: "upload failed", status: http.StatusOK, body: "image", reason: "upload_failed"},
		{name: "waiting cancelled", reason: "provider_slot"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.status != 0 {
					w.WriteHeader(test.status)
				}
				w.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)
			var logs bytes.Buffer
			uploader := &platformtest.Uploader{}
			thumbnails := Thumbnails{
				HTTP:    safehttp.New(safehttp.Config{AllowPrivateAddresses: true}),
				Storage: uploader,
				Limits:  providers.NewLimits(),
				Logger:  slog.New(slog.NewJSONHandler(&logs, nil)),
			}
			imageURL := server.URL + "/image?token=private-image-token"
			ctx := context.Background()
			switch test.name {
			case "no source image":
				imageURL = ""
			case "storage unavailable":
				thumbnails.Storage = nil
			case "unsafe source":
				imageURL = "file:///private/image"
			case "upload failed":
				uploader.Err = errors.New("upload rejected https://storage.example/path?token=private-image-token")
			case "waiting cancelled":
				for range providers.LightHTTPConcurrency {
					release, err := thumbnails.Limits.AcquireLightHTTP(ctx)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(release)
				}
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			got := thumbnails.Store(ctx, sourceidentity.SourceIdentity{Platform: "pinterest", ContentID: "123", ContentType: "image"}, imageURL)
			if got != "" {
				t.Fatalf("failed preview returned %q", got)
			}
			if !strings.Contains(logs.String(), `"reason":"`+test.reason+`"`) {
				t.Fatalf("missing failure reason %q: %s", test.reason, logs.String())
			}
			for _, secret := range []string{"http://", "https://", "file://", "private-image-token"} {
				if strings.Contains(logs.String(), secret) {
					t.Errorf("preview source or token reached logs: %s", logs.String())
				}
			}
		})
	}
}
