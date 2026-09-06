package social

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/platform/web"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// XPlatform is the source identity platform this handler serves.
const XPlatform = "x"

// xActor keys the configured Apify actor and its concurrency slot.
const xActor = "x"

// oembedEndpoint is the public publish endpoint. It is a variable so a test
// can serve it locally.
var oembedEndpoint = "https://publish.twitter.com/oembed"

// XHandler reads a post through the public oEmbed endpoint: no session, no
// scraping, and nothing that is not already public. The paid actor is reached
// only to add replies and a preview image, which are worth having and never
// worth failing a run over.
type XHandler struct {
	deps Deps
}

func NewX(deps Deps) *XHandler { return &XHandler{deps: deps} }

var _ platform.Handler = (*XHandler)(nil)

func (h *XHandler) Platform() string { return XPlatform }

func (h *XHandler) Prepare(ctx context.Context, identity sourceidentity.SourceIdentity) (platform.Prepared, error) {
	text, author, err := h.readPost(ctx, identity)
	if err != nil {
		return platform.Prepared{}, classify(err)
	}

	caption := text
	if author != "" {
		caption = author + ": " + text
	}
	prepared := platform.Prepared{
		Caption:    caption,
		PageText:   text,
		NeedsMedia: false,
	}

	// Replies and a preview are extra. A failure here costs nothing, because
	// the post's own words are already in hand.
	if h.deps.Apify != nil && h.deps.Apify.Configured(xActor) {
		replies, thumbnail := h.enrich(ctx, identity)
		if replies != "" {
			prepared.PageText = text + "\n\nReplies:\n" + replies
		}
		prepared.ThumbnailURL = h.deps.storeThumbnail(ctx, identity, thumbnail)
		h.deps.log(XPlatform).Info("x post ready",
			"content_id", identity.ContentID, "source", "x_oembed+actor")
		return prepared, nil
	}

	h.deps.log(XPlatform).Info("x post ready",
		"content_id", identity.ContentID, "source", "x_oembed")
	return prepared, nil
}

// Download is never reached: an X post is always light work. It answers with a
// terminal failure rather than a panic, so a future routing mistake surfaces
// as one bad job instead of a crashed worker.
func (h *XHandler) Download(context.Context, sourceidentity.SourceIdentity, string) ([]ai.Media, error) {
	return nil, web.Terminal("source_not_supported",
		"An X post has no downloadable media.",
		fmt.Errorf("the x handler is light-only"))
}

// readPost fetches the post's words. Both the provider name and the returned
// canonical URL have to agree that this is the post we asked about: a response
// about a different post would otherwise be saved under this identity.
func (h *XHandler) readPost(ctx context.Context, identity sourceidentity.SourceIdentity) (text, author string, err error) {
	release, err := h.deps.limits().AcquireLightHTTP(ctx)
	if err != nil {
		return "", "", err
	}
	defer release()

	query := url.Values{
		"url":         {identity.NormalizedURL},
		"omit_script": {"1"},
		"hide_thread": {"1"},
		"dnt":         {"true"},
	}
	response, err := h.deps.HTTP.Get(ctx, oembedEndpoint+"?"+query.Encode())
	if err != nil {
		return "", "", err
	}
	if err := statusError(response.Status); err != nil {
		return "", "", err
	}

	var payload struct {
		ProviderName string `json:"provider_name"`
		URL          string `json:"url"`
		HTML         string `json:"html"`
		AuthorName   string `json:"author_name"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		return "", "", fmt.Errorf("%w: the oembed response was not usable JSON", ErrNoPublicContent)
	}

	provider := strings.ToLower(strings.TrimSpace(payload.ProviderName))
	if provider != "twitter" && provider != "x" {
		return "", "", fmt.Errorf("%w: the response came from %q", ErrPostMismatch, provider)
	}
	returned, resolveErr := sourceidentity.Resolve(payload.URL)
	if resolveErr != nil || returned.ContentID != identity.ContentID {
		return "", "", ErrPostMismatch
	}

	text = tweetText(payload.HTML)
	if strings.TrimSpace(text) == "" {
		return "", "", ErrNoPublicContent
	}
	return text, strings.TrimSpace(payload.AuthorName), nil
}

// enrich asks the configured actor for replies and a preview image.
func (h *XHandler) enrich(ctx context.Context, identity sourceidentity.SourceIdentity) (replies, thumbnail string) {
	release, err := h.deps.limits().AcquireActor(ctx, xActor)
	if err != nil {
		return "", ""
	}
	defer release()

	items, err := h.deps.Apify.Run(ctx, xActor, map[string]any{
		"startUrls":   []string{identity.NormalizedURL},
		"maxItems":    MaxReplies + 1,
		"addUserInfo": false,
	})
	if err != nil {
		h.deps.log(XPlatform).Info("x actor unavailable",
			"content_id", identity.ContentID, "error", redact(err))
		return "", ""
	}

	collected := []string{}
	for _, item := range items {
		var row xActorItem
		if err := json.Unmarshal(item, &row); err != nil {
			continue
		}
		// The post itself carries the preview; everything else is a reply.
		if row.ID == identity.ContentID {
			thumbnail = firstNonEmpty(row.Thumbnail, row.mediaThumbnail())
			continue
		}
		if cleaned := strings.TrimSpace(row.Text); cleaned != "" && len(collected) < MaxReplies {
			collected = append(collected, cleaned)
		}
	}
	return strings.Join(collected, "\n"), thumbnail
}

// xActorItem is this package's private view of the X actor's dataset row.
// Other handlers call different actors with different shapes; one shared
// struct would make every one of them wrong.
type xActorItem struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	Thumbnail string `json:"thumbnailUrl"`
	Media     []struct {
		ThumbnailURL string `json:"thumbnailUrl"`
		MediaURL     string `json:"media_url_https"`
	} `json:"media"`
}

func (i xActorItem) mediaThumbnail() string {
	for _, item := range i.Media {
		if url := firstNonEmpty(item.ThumbnailURL, item.MediaURL); url != "" {
			return url
		}
	}
	return ""
}

var (
	tweetParagraph = regexp.MustCompile(`(?is)<p[^>]*>(.*?)</p>`)
	htmlTag        = regexp.MustCompile(`(?s)<[^>]*>`)
	whitespaceRun  = regexp.MustCompile(`\s+`)
)

// tweetText pulls the post's words out of the oEmbed blockquote and leaves the
// attribution line behind.
func tweetText(embedHTML string) string {
	match := tweetParagraph.FindStringSubmatch(embedHTML)
	if match == nil {
		return ""
	}
	text := match[1]
	for _, br := range []string{"<br>", "<br/>", "<br />"} {
		text = strings.ReplaceAll(text, br, "\n")
	}
	text = htmlTag.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	return strings.TrimSpace(whitespaceRun.ReplaceAllString(text, " "))
}
