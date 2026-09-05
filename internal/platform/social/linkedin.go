package social

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/XploY04/reelpin-go/internal/apify"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// LinkedInHandler reads a post through the configured actor. Only posts are
// claimed: a profile, company or article page has public metadata and belongs
// to the generic handler.
type LinkedInHandler struct {
	deps Deps
}

func NewLinkedIn(deps Deps) *LinkedInHandler { return &LinkedInHandler{deps: deps} }

func (h *LinkedInHandler) Name() string { return "linkedin" }

func (h *LinkedInHandler) Match(identity sourceidentity.SourceIdentity) bool {
	return identity.Platform == "linkedin" && identity.ContentType == "post"
}

func (h *LinkedInHandler) Capabilities(sourceidentity.SourceIdentity) platform.Capabilities {
	return platform.Capabilities{Caption: true, Images: true}
}

func (h *LinkedInHandler) Normalize(_ context.Context, identity sourceidentity.SourceIdentity) (sourceidentity.SourceIdentity, error) {
	return identity, nil
}

func (h *LinkedInHandler) Prepare(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) (platform.Prepared, error) {
	if h.deps.Apify == nil || !h.deps.Apify.Configured("linkedin") {
		return platform.Prepared{}, ErrLinkedInNotConfigured
	}

	// The normalized URL keeps the urn type. Passing an id under the wrong urn
	// returns an empty post, which is why the type is part of the identity.
	items, err := h.deps.Apify.Run(ctx, "linkedin", map[string]any{
		"urls":     []string{identity.NormalizedURL},
		"maxItems": 1,
	})
	if err != nil {
		if err == apify.ErrRateLimited {
			return platform.Prepared{}, err
		}
		return platform.Prepared{}, fmt.Errorf("the linkedin actor could not read this post")
	}

	for _, item := range items {
		var payload struct {
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
		if err := json.Unmarshal(item, &payload); err != nil {
			continue
		}

		body := strings.TrimSpace(payload.Text)
		if body == "" {
			continue
		}

		parts := []string{body}
		for index, comment := range payload.Comments {
			if index >= MaxComments {
				break
			}
			if text := strings.TrimSpace(comment.Text); text != "" {
				parts = append(parts, text)
			}
		}

		prepared := platform.Prepared{
			Title:            firstOfNonEmpty(payload.Title, firstLine(body)),
			Caption:          body,
			Transcript:       strings.Join(parts, "\n\n"),
			IngestionMethod:  "linkedin_actor",
			TranscriptSource: "linkedin_post_text",
		}
		if author := strings.TrimSpace(payload.Author); author != "" {
			prepared.Caption = author + ": " + body
		}

		image := payload.ImageURL
		if image == "" && len(payload.Images) > 0 {
			image = payload.Images[0].URL
		}
		if image != "" && h.deps.Storage != nil {
			if response, err := h.deps.HTTP.Get(ctx, image); err == nil &&
				response.Status >= 200 && response.Status < 300 {
				if url, err := h.deps.Storage.Upload(ctx, storageKey(identity),
					strings.NewReader(string(response.Body)), "image/jpeg"); err == nil {
					prepared.ThumbnailURL = url
				}
			}
		}
		return prepared, nil
	}

	return platform.Prepared{}, ErrNoPublicContent
}

// ErrLinkedInNotConfigured is a deployment gap, not a content problem: without
// the actor there is no way to read a post at all.
var ErrLinkedInNotConfigured = fmt.Errorf("the linkedin actor is not configured")

func firstOfNonEmpty(values ...string) string {
	for _, value := range values {
		if cleaned := strings.TrimSpace(value); cleaned != "" {
			return cleaned
		}
	}
	return ""
}
