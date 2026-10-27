package platform

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/XploY04/reelpin-go/internal/storage"
)

// Thumbnails copies a source's preview image into our own bucket. It sits on
// the seam every handler already implements, because where the image ends up
// is not a per-platform decision: the reader renders images from our storage
// and nowhere else, so a handler that passes a platform CDN URL straight
// through saves a link that shows no preview at all.
//
// The zero value stores nothing and reports nothing stored. That is how a
// deployment with no service key degrades: an empty thumbnail, never a URL the
// reader cannot render.
type Thumbnails struct {
	HTTP    *safehttp.Client
	Storage storage.Uploader
	Limits  *providers.Limits
	Logger  *slog.Logger
}

// Store fetches thumbnailURL and returns the public URL of the stored object.
// It never fails a run: a missing preview is cosmetic and the content is worth
// saving without one, so every failure along the way returns an empty string.
func (t Thumbnails) Store(ctx context.Context, identity sourceidentity.SourceIdentity, thumbnailURL string) string {
	if strings.TrimSpace(thumbnailURL) == "" || t.HTTP == nil || t.Storage == nil {
		return ""
	}

	limits := t.Limits
	if limits == nil {
		limits = providers.NewLimits()
	}
	release, err := limits.AcquireLightHTTP(ctx)
	if err != nil {
		return ""
	}
	response, err := t.HTTP.Get(ctx, thumbnailURL)
	release()
	if err != nil || response.Status < 200 || response.Status >= 300 || len(response.Body) == 0 {
		return ""
	}

	key := storage.Key(identity.Platform, identity.ContentType, identity.ContentID,
		identity.NormalizedURL, ".jpg")
	stored, err := t.Storage.Upload(ctx, key, bytes.NewReader(response.Body), "image/jpeg")
	if err != nil {
		t.log().Info("thumbnail upload failed",
			"platform", identity.Platform, "content_id", identity.ContentID, "error", redactURLs(err))
		return ""
	}
	return stored
}

func (t Thumbnails) log() *slog.Logger {
	if t.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return t.Logger
}

// urlPattern matches any absolute URL in a string.
var urlPattern = regexp.MustCompile(`https?://\S+`)

// redactURLs is what goes into a log line instead of an error. An upload
// failure quotes the endpoint it failed on and a CDN quotes the signed image
// URL it refused; the class of failure is what a person reading logs needs and
// the address is not.
func redactURLs(err error) string {
	return urlPattern.ReplaceAllString(err.Error(), "[url]")
}
