package web

import (
	"context"
	"fmt"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// PlatformName is what this handler registers as.
//
// A generic link's identity platform is its hostname, so one registration
// cannot cover every site a user shares. Registering this handler is the
// registry's fallback slot, which the seam does not express yet; see the
// package tests and the task report.
const PlatformName = "web"

type Deps struct {
	HTTP  *safehttp.Client
	Limit *providers.Limits
}

// Handler serves an ordinary page: an article, a blog post, a documentation
// page, anything with no platform of its own. It is always light work. The
// download path in this service admits only the three platform hosts yt-dlp
// is allowlisted for, and the safe client's body cap is sized for pages, not
// video, so a generic page has no way to become a media job.
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

	metadata, body, err := Fetch(ctx, h.deps.HTTP, identity.NormalizedURL)
	if err != nil {
		return platform.Prepared{}, Classify(err)
	}
	if !metadata.Usable() {
		return platform.Prepared{}, Terminal("page_empty",
			"This page has nothing to save.", ErrNothingToRead)
	}

	// The body's prose beats the description when there is any: a link preview
	// is one sentence, an article is the reason the user saved it.
	text := ReadableText(body)
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

// Download is never reached: Prepare always reports light work. It answers
// with a terminal failure rather than a panic, so a future routing mistake
// surfaces as one bad job instead of a crashed worker.
func (h *Handler) Download(context.Context, sourceidentity.SourceIdentity, string) ([]ai.Media, error) {
	return nil, Terminal("source_not_supported",
		"This link has no downloadable media.",
		fmt.Errorf("the web handler is light-only"))
}
