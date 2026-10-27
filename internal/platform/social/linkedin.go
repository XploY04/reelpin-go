package social

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/platform/web"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// LinkedInPlatform is the source identity platform this handler serves.
const LinkedInPlatform = "linkedin"

// linkedinActor keys the configured Apify actor and its concurrency slot.
const linkedinActor = "linkedin"

// MaxComments bounds how much of a discussion is read.
const MaxComments = 8

// LinkedInHandler reads feed posts through the configured actor, and every
// other LinkedIn page through its published metadata.
//
// The split matters: a post is behind a wall that only the actor gets past, so
// without one there is nothing to read. A profile, company or article page
// publishes ordinary link-preview tags, which cost nothing, so paying an actor
// for them would be waste.
type LinkedInHandler struct {
	deps Deps
}

func NewLinkedIn(deps Deps) *LinkedInHandler { return &LinkedInHandler{deps: deps} }

var _ platform.Handler = (*LinkedInHandler)(nil)

func (h *LinkedInHandler) Platform() string { return LinkedInPlatform }

func (h *LinkedInHandler) Prepare(ctx context.Context, identity sourceidentity.SourceIdentity) (platform.Prepared, error) {
	if identity.ContentType != "post" {
		return h.preparePage(ctx, identity)
	}
	return h.preparePost(ctx, identity)
}

// Download is never reached: LinkedIn is always light work here.
func (h *LinkedInHandler) Download(context.Context, sourceidentity.SourceIdentity, string) ([]ai.Media, error) {
	return nil, web.Terminal("source_not_supported",
		"A LinkedIn page has no downloadable media.",
		fmt.Errorf("the linkedin handler is light-only"))
}

// preparePage reads a profile, company or article the way any other page is
// read. No actor, no credential, no cost.
func (h *LinkedInHandler) preparePage(ctx context.Context, identity sourceidentity.SourceIdentity) (platform.Prepared, error) {
	release, err := h.deps.limits().AcquireLightHTTP(ctx)
	if err != nil {
		return platform.Prepared{}, err
	}
	defer release()

	metadata, body, err := web.Fetch(ctx, h.deps.HTTP, identity.NormalizedURL)
	if err != nil {
		return platform.Prepared{}, classify(err)
	}
	if !metadata.Usable() {
		return platform.Prepared{}, classify(ErrNoPublicContent)
	}

	text := web.ReadableText(body)
	if text == "" {
		text = metadata.Caption()
	}
	h.deps.log(LinkedInPlatform).Info("linkedin page ready",
		"content_id", identity.ContentID, "content_type", identity.ContentType,
		"source", "linkedin_page")

	return platform.Prepared{
		Caption:      metadata.Caption(),
		PageText:     text,
		ThumbnailURL: h.deps.Thumbnails.Store(ctx, identity, metadata.ImageURL),
		NeedsMedia:   false,
	}, nil
}

// preparePost reads a feed update through the actor. The normalized URL keeps
// its urn type on purpose: the same numeric id means a different post under
// activity, ugcPost and share, and passing one under the wrong urn returns an
// empty post rather than an error.
func (h *LinkedInHandler) preparePost(ctx context.Context, identity sourceidentity.SourceIdentity) (platform.Prepared, error) {
	if h.deps.Apify == nil || !h.deps.Apify.Configured(linkedinActor) {
		return platform.Prepared{}, classify(fmt.Errorf(
			"%w: linkedin posts need the configured actor", ErrNotConfigured))
	}

	release, err := h.deps.limits().AcquireActor(ctx, linkedinActor)
	if err != nil {
		return platform.Prepared{}, err
	}
	defer release()

	items, err := h.deps.Apify.Run(ctx, linkedinActor, map[string]any{
		"urls":     []string{identity.NormalizedURL},
		"maxItems": 1,
	})
	if err != nil {
		h.deps.log(LinkedInPlatform).Info("linkedin actor failed",
			"content_id", identity.ContentID, "error", redact(err))
		return platform.Prepared{}, classify(err)
	}

	for _, item := range items {
		var row linkedinActorItem
		if err := json.Unmarshal(item, &row); err != nil {
			continue
		}
		body := strings.TrimSpace(row.Text)
		if body == "" {
			continue
		}

		parts := []string{body}
		for index, comment := range row.Comments {
			if index >= MaxComments {
				break
			}
			if text := strings.TrimSpace(comment.Text); text != "" {
				parts = append(parts, text)
			}
		}

		caption := body
		if author := strings.TrimSpace(row.Author); author != "" {
			caption = author + ": " + body
		}
		h.deps.log(LinkedInPlatform).Info("linkedin post ready",
			"content_id", identity.ContentID, "source", "linkedin_actor")

		return platform.Prepared{
			Caption:      caption,
			PageText:     strings.Join(parts, "\n\n"),
			ThumbnailURL: h.deps.Thumbnails.Store(ctx, identity, row.image()),
			NeedsMedia:   false,
		}, nil
	}

	return platform.Prepared{}, classify(ErrNoPublicContent)
}

// linkedinActorItem is this package's private view of the LinkedIn actor's
// dataset row.
type linkedinActorItem struct {
	Text     string `json:"text"`
	Title    string `json:"title"`
	Author   string `json:"authorName"`
	ImageURL string `json:"imageUrl"`
	Images   []struct {
		URL string `json:"url"`
	} `json:"images"`
	Comments []struct {
		Text string `json:"text"`
	} `json:"comments"`
}

func (i linkedinActorItem) image() string {
	if cleaned := strings.TrimSpace(i.ImageURL); cleaned != "" {
		return cleaned
	}
	for _, image := range i.Images {
		if cleaned := strings.TrimSpace(image.URL); cleaned != "" {
			return cleaned
		}
	}
	return ""
}
