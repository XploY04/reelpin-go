// Package places ingests curated place and travel links: a map pin, a
// restaurant listing, a hotel page.
//
// The location is the content. There is nothing to download and nothing to
// transcribe, so these are always light, and probing them for video would
// spend a download attempt on a restaurant page.
package places

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/platform/web"
	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// Platforms are every place source the identity resolver names. Each needs its
// own registration because the registry is keyed by platform, so one handler
// implementation is registered once per name.
var Platforms = []string{
	"google_maps",
	"tripadvisor",
	"airbnb",
	"zomato",
	"swiggy",
	"makemytrip",
	"booking",
}

type Deps struct {
	HTTP   *safehttp.Client
	Limit  *providers.Limits
	Logger *slog.Logger
}

type Handler struct {
	platform string
	deps     Deps
}

// Handlers returns one handler per place platform, sharing one implementation.
// Registering them individually keeps the registry's "one handler per
// platform" rule honest instead of hiding a second lookup inside a handler.
func Handlers(deps Deps) []platform.Handler {
	handlers := make([]platform.Handler, 0, len(Platforms))
	for _, name := range Platforms {
		handlers = append(handlers, &Handler{platform: name, deps: deps})
	}
	return handlers
}

// New builds one handler for one place platform.
func New(platformName string, deps Deps) *Handler {
	return &Handler{platform: platformName, deps: deps}
}

func (h *Handler) Platform() string { return h.platform }

func (h *Handler) Prepare(ctx context.Context, identity sourceidentity.SourceIdentity) (platform.Prepared, error) {
	release, err := h.deps.Limit.AcquireLightHTTP(ctx)
	if err != nil {
		return platform.Prepared{}, err
	}
	defer release()

	metadata, body, err := web.Fetch(ctx, h.deps.HTTP, identity.NormalizedURL)
	if err != nil {
		return platform.Prepared{}, web.Classify(err)
	}
	if !metadata.Usable() {
		return platform.Prepared{}, web.Terminal("page_empty",
			"This place page has nothing to save.", web.ErrNothingToRead)
	}

	// A listing's prose carries the address, the hours and the reviews the
	// extractor turns into a location; the preview tags carry only the name.
	text := web.ReadableText(body)
	if text == "" {
		text = metadata.Caption()
	}

	return platform.Prepared{
		Caption:      metadata.Caption(),
		PageText:     text,
		ThumbnailURL: metadata.ImageURL,
		NeedsMedia:   false,
	}, nil
}

// Download is never reached: a place page is always light work.
func (h *Handler) Download(context.Context, sourceidentity.SourceIdentity, string) ([]ai.Media, error) {
	return nil, web.Terminal("source_not_supported",
		"A place page has no downloadable media.",
		fmt.Errorf("the places handler is light-only"))
}
