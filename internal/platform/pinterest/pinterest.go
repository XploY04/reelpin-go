// Package pinterest ingests pins.
//
// A pin is a picture and a caption pointing at somewhere else. There is no
// audio to transcribe, so this is always light work: the pin's own metadata is
// the content, and the image is what the reader sees.
package pinterest

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

// PlatformName is the source identity platform this handler serves.
const PlatformName = "pinterest"

type Deps struct {
	HTTP       *safehttp.Client
	Thumbnails platform.Thumbnails
	Limit      *providers.Limits
	Logger     *slog.Logger
}

type Handler struct {
	deps Deps
}

func New(deps Deps) *Handler { return &Handler{deps: deps} }

func (h *Handler) Platform() string { return PlatformName }

func (h *Handler) Prepare(ctx context.Context, identity sourceidentity.SourceIdentity) (platform.Prepared, error) {
	release, err := h.deps.Limit.AcquireLightHTTP(ctx)
	if err != nil {
		return platform.Prepared{}, err
	}
	defer release()

	metadata, _, err := web.Fetch(ctx, h.deps.HTTP, identity.NormalizedURL)
	if err != nil {
		return platform.Prepared{}, web.Classify(err)
	}
	if !metadata.Usable() {
		return platform.Prepared{}, web.Terminal("page_empty",
			"This pin has nothing to save.", web.ErrNothingToRead)
	}

	caption := metadata.Caption()
	return platform.Prepared{
		Caption:      caption,
		PageText:     caption,
		ThumbnailURL: h.deps.Thumbnails.Store(ctx, identity, metadata.ImageURL),
		NeedsMedia:   false,
	}, nil
}

// Download is never reached: a pin is always light work.
func (h *Handler) Download(context.Context, sourceidentity.SourceIdentity, string) ([]ai.Media, error) {
	return nil, web.Terminal("source_not_supported",
		"A pin has no downloadable media.",
		fmt.Errorf("the pinterest handler is light-only"))
}
