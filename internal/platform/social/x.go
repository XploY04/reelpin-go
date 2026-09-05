// Package social prepares X, LinkedIn and Reddit. All three are text-first:
// the post's words are the content, and any media is supporting.
package social

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/url"
	"regexp"
	"strings"

	"github.com/XploY04/reelpin-go/internal/apify"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/XploY04/reelpin-go/internal/storage"
)

// ErrNoPublicContent is a post that exists but published nothing readable.
var ErrNoPublicContent = errors.New("the post has no public content")

// ErrPostMismatch means the provider answered about a different post, which is
// never safe to save under this identity.
var ErrPostMismatch = errors.New("the provider returned a different post")

type Deps struct {
	HTTP    *safehttp.Client
	Apify   *apify.Client
	Storage storage.Uploader
	Reddit  *RedditClient
	Logger  *slog.Logger
}

// XHandler reads a post through the public oEmbed endpoint. No session, no
// scraping, and nothing that is not already public.
type XHandler struct {
	deps Deps
}

func NewX(deps Deps) *XHandler { return &XHandler{deps: deps} }

func (h *XHandler) Name() string { return "x" }

func (h *XHandler) Match(identity sourceidentity.SourceIdentity) bool {
	return identity.Platform == "x"
}

func (h *XHandler) Capabilities(sourceidentity.SourceIdentity) platform.Capabilities {
	return platform.Capabilities{Caption: true, Images: true}
}

func (h *XHandler) Normalize(_ context.Context, identity sourceidentity.SourceIdentity) (sourceidentity.SourceIdentity, error) {
	return identity, nil
}

// oembedEndpoint is a variable so tests can serve it locally.
var oembedEndpoint = "https://publish.twitter.com/oembed"

func (h *XHandler) Prepare(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) (platform.Prepared, error) {
	query := url.Values{
		"url":         {identity.NormalizedURL},
		"omit_script": {"1"},
		"hide_thread": {"1"},
		"dnt":         {"true"},
	}

	response, err := h.deps.HTTP.Get(ctx, oembedEndpoint+"?"+query.Encode())
	if err != nil {
		return platform.Prepared{}, err
	}
	if response.Status == 404 {
		return platform.Prepared{}, ErrPostNotFound
	}
	if response.Status == 401 || response.Status == 403 {
		return platform.Prepared{}, ErrPostProtected
	}
	if response.Status < 200 || response.Status >= 300 {
		return platform.Prepared{}, fmt.Errorf("the oembed endpoint returned HTTP %d", response.Status)
	}

	var payload struct {
		ProviderName string `json:"provider_name"`
		URL          string `json:"url"`
		HTML         string `json:"html"`
		AuthorName   string `json:"author_name"`
		AuthorURL    string `json:"author_url"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		return platform.Prepared{}, fmt.Errorf("the oembed response was not usable JSON")
	}

	// The provider and the returned URL both have to agree that this is the
	// post we asked about.
	if provider := strings.ToLower(strings.TrimSpace(payload.ProviderName)); provider != "twitter" && provider != "x" {
		return platform.Prepared{}, fmt.Errorf("the oembed response came from an unexpected provider")
	}
	returned, err := sourceidentity.Resolve(payload.URL)
	if err != nil || returned.ContentID != identity.ContentID {
		return platform.Prepared{}, ErrPostMismatch
	}

	text := tweetText(payload.HTML)
	if strings.TrimSpace(text) == "" {
		return platform.Prepared{}, ErrNoPublicContent
	}

	prepared := platform.Prepared{
		Caption:          text,
		Title:            firstLine(text),
		Transcript:       text,
		IngestionMethod:  "x_oembed",
		TranscriptSource: "x_post_text",
	}
	if author := strings.TrimSpace(payload.AuthorName); author != "" {
		prepared.Caption = author + ": " + text
	}

	// Replies and a thumbnail are extra, and only when an actor is configured.
	if h.deps.Apify != nil && h.deps.Apify.Configured("x") {
		if extra, thumbnail := h.enrich(ctx, identity); extra != "" || thumbnail != "" {
			if extra != "" {
				prepared.Transcript = text + "\n\n" + extra
			}
			prepared.ThumbnailURL = h.storeThumbnail(ctx, identity, thumbnail)
		}
	}
	return prepared, nil
}

// enrich asks the configured actor for replies and a thumbnail. A failure here
// costs nothing: the post text is already in hand.
func (h *XHandler) enrich(ctx context.Context, identity sourceidentity.SourceIdentity) (string, string) {
	items, err := h.deps.Apify.Run(ctx, "x", map[string]any{
		"startUrls":   []string{identity.NormalizedURL},
		"maxItems":    MaxReplies + 1,
		"addUserInfo": false,
	})
	if err != nil {
		h.deps.Logger.Info("x actor unavailable", "content_id", identity.ContentID, "error", err)
		return "", ""
	}

	replies := []string{}
	thumbnail := ""
	for _, item := range items {
		var payload struct {
			Text      string `json:"text"`
			ID        string `json:"id"`
			Thumbnail string `json:"thumbnailUrl"`
			Media     []struct {
				ThumbnailURL string `json:"thumbnailUrl"`
				MediaURL     string `json:"media_url_https"`
			} `json:"media"`
		}
		if err := json.Unmarshal(item, &payload); err != nil {
			continue
		}
		if payload.ID == identity.ContentID {
			thumbnail = firstNonEmpty(payload.Thumbnail, mediaThumbnail(payload.Media))
			continue
		}
		if cleaned := strings.TrimSpace(payload.Text); cleaned != "" && len(replies) < MaxReplies {
			replies = append(replies, cleaned)
		}
	}

	if len(replies) == 0 {
		return "", thumbnail
	}
	return "Replies:\n" + strings.Join(replies, "\n"), thumbnail
}

// MaxReplies bounds how much of a thread is read. A whole thread is rarely
// about the post any more.
const MaxReplies = 10

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
	text := strings.ReplaceAll(match[1], "<br>", "\n")
	text = strings.ReplaceAll(text, "<br/>", "\n")
	text = strings.ReplaceAll(text, "<br />", "\n")
	text = htmlTag.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	return strings.TrimSpace(whitespaceRun.ReplaceAllString(text, " "))
}

func (h *XHandler) storeThumbnail(ctx context.Context, identity sourceidentity.SourceIdentity, thumbnailURL string) string {
	if thumbnailURL == "" || h.deps.Storage == nil {
		return ""
	}
	response, err := h.deps.HTTP.Get(ctx, thumbnailURL)
	if err != nil || response.Status < 200 || response.Status >= 300 {
		return ""
	}
	key := storage.Key(identity.Platform, identity.ContentType, identity.ContentID, identity.NormalizedURL, ".jpg")
	url, err := h.deps.Storage.Upload(ctx, key, strings.NewReader(string(response.Body)), "image/jpeg")
	if err != nil {
		return ""
	}
	return url
}

// ErrPostNotFound and ErrPostProtected are the two states a public reader can
// tell apart, and both are terminal.
var (
	ErrPostNotFound  = errors.New("the post was not found")
	ErrPostProtected = errors.New("the post is protected or unavailable")
)

func mediaThumbnail(media []struct {
	ThumbnailURL string `json:"thumbnailUrl"`
	MediaURL     string `json:"media_url_https"`
}) string {
	for _, item := range media {
		if url := firstNonEmpty(item.ThumbnailURL, item.MediaURL); url != "" {
			return url
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if cleaned := strings.TrimSpace(value); cleaned != "" {
			return cleaned
		}
	}
	return ""
}

func firstLine(text string) string {
	line := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	if len([]rune(line)) > 120 {
		return string([]rune(line)[:120])
	}
	return line
}

// storageKey is the shared thumbnail key for these handlers.
func storageKey(identity sourceidentity.SourceIdentity) string {
	return storage.Key(identity.Platform, identity.ContentType, identity.ContentID,
		identity.NormalizedURL, ".jpg")
}
