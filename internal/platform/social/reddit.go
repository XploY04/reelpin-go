package social

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/platform/web"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// RedditPlatform is the source identity platform this handler serves.
const RedditPlatform = "reddit"

// Reddit endpoints. They are variables so a test can serve them locally.
var (
	redditAPIBase    = "https://oauth.reddit.com"
	redditPublicBase = "https://www.reddit.com"
)

// userAgent is required: Reddit refuses requests without one, and a shared
// browser string is refused faster than an honest one.
const userAgent = "reelpin/1.0"

// TokenSource mints the application token Reddit wants from datacenter
// addresses. It is an interface because minting one is a form POST, which the
// safe client does not expose, so the concrete client lives outside this
// package. A handler with no token source falls back to the public endpoint.
type TokenSource interface {
	// AccessToken returns a cached application token, refreshing it when it is
	// close to lapsing.
	AccessToken(ctx context.Context) (string, error)
}

// RedditHandler reads a post and the top of its discussion.
//
// It prefers the authenticated API, because Reddit throttles datacenter
// addresses hard on the public endpoints, and falls back to the public JSON
// view when no credential is configured. Both are the same listing shape, so
// there is one parser and one set of rules about what counts as content.
type RedditHandler struct {
	deps Deps
}

func NewReddit(deps Deps) *RedditHandler { return &RedditHandler{deps: deps} }

var _ platform.Handler = (*RedditHandler)(nil)

func (h *RedditHandler) Platform() string { return RedditPlatform }

func (h *RedditHandler) Prepare(ctx context.Context, identity sourceidentity.SourceIdentity) (platform.Prepared, error) {
	body, source, err := h.fetchListing(ctx, identity)
	if err != nil {
		return platform.Prepared{}, classify(err)
	}

	post, comments, imageURL, err := parseListing(body)
	if err != nil {
		return platform.Prepared{}, classify(err)
	}

	parts := []string{}
	if post.Title != "" {
		parts = append(parts, post.Title)
	}
	if post.SelfText != "" {
		parts = append(parts, post.SelfText)
	}
	if len(comments) > 0 {
		parts = append(parts, "Top comments:\n"+strings.Join(comments, "\n"))
	}
	if len(parts) == 0 && imageURL == "" {
		return platform.Prepared{}, classify(ErrNoPublicContent)
	}

	h.deps.log(RedditPlatform).Info("reddit post ready",
		"content_id", identity.ContentID, "comments", len(comments), "source", source)

	return platform.Prepared{
		Caption:      post.Title,
		PageText:     strings.Join(parts, "\n\n"),
		ThumbnailURL: h.deps.storeThumbnail(ctx, identity, imageURL),
		NeedsMedia:   false,
	}, nil
}

// Download is never reached: a Reddit post is read, not downloaded. Hosted
// video would need the download tool, which admits only the three video hosts
// it is allowlisted for.
func (h *RedditHandler) Download(context.Context, sourceidentity.SourceIdentity, string) ([]ai.Media, error) {
	return nil, web.Terminal("source_not_supported",
		"A Reddit post has no downloadable media.",
		fmt.Errorf("the reddit handler is light-only"))
}

// fetchListing reads the post and its comments, naming which endpoint answered
// so the log records how a result was obtained.
func (h *RedditHandler) fetchListing(ctx context.Context, identity sourceidentity.SourceIdentity) ([]byte, string, error) {
	release, err := h.deps.limits().AcquireLightHTTP(ctx)
	if err != nil {
		return nil, "", err
	}
	defer release()

	query := url.Values{
		"limit":    {fmt.Sprint(MaxComments)},
		"depth":    {"1"},
		"raw_json": {"1"},
	}.Encode()

	headers := http.Header{}
	headers.Set("User-Agent", userAgent)

	endpoint := fmt.Sprintf("%s/comments/%s.json?%s", redditPublicBase, identity.ContentID, query)
	source := "reddit_public"

	// The authenticated API first when a credential exists: the public
	// endpoints refuse datacenter addresses often enough that relying on them
	// would make production flaky.
	if h.deps.Reddit != nil {
		token, err := h.deps.Reddit.AccessToken(ctx)
		if err != nil {
			h.deps.log(RedditPlatform).Info("reddit token unavailable, using the public endpoint",
				"content_id", identity.ContentID, "error", redact(err))
		} else if token != "" {
			headers.Set("Authorization", "Bearer "+token)
			endpoint = fmt.Sprintf("%s/comments/%s?%s", redditAPIBase, identity.ContentID, query)
			source = "reddit_api"
		}
	}

	response, err := h.deps.HTTP.GetWithHeaders(ctx, endpoint, headers)
	if err != nil {
		return nil, source, err
	}
	if err := statusError(response.Status); err != nil {
		return nil, source, err
	}
	return response.Body, source, nil
}

type redditPost struct {
	Title    string
	SelfText string
}

// redditListing is the two-element listing Reddit returns: the post, then its
// comments.
type redditListing []struct {
	Data struct {
		Children []struct {
			Kind string `json:"kind"`
			Data struct {
				Title     string `json:"title"`
				SelfText  string `json:"selftext"`
				Body      string `json:"body"`
				URL       string `json:"url_overridden_by_dest"`
				Thumbnail string `json:"thumbnail"`
				Preview   struct {
					Images []struct {
						Source struct {
							URL string `json:"url"`
						} `json:"source"`
					} `json:"images"`
				} `json:"preview"`
				Stickied bool `json:"stickied"`
			} `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

func parseListing(body []byte) (redditPost, []string, string, error) {
	var listing redditListing
	if err := json.Unmarshal(body, &listing); err != nil || len(listing) == 0 {
		return redditPost{}, nil, "", fmt.Errorf(
			"%w: the response was not a reddit listing", ErrNoPublicContent)
	}

	post := redditPost{}
	imageURL := ""
	if len(listing[0].Data.Children) > 0 {
		data := listing[0].Data.Children[0].Data
		post.Title = strings.TrimSpace(data.Title)
		post.SelfText = strings.TrimSpace(data.SelfText)

		if len(data.Preview.Images) > 0 {
			imageURL = data.Preview.Images[0].Source.URL
		}
		if imageURL == "" && isImageURL(data.URL) {
			imageURL = data.URL
		}
	}

	comments := []string{}
	if len(listing) > 1 {
		for _, child := range listing[1].Data.Children {
			// t1 is a comment; anything else is a "load more" placeholder. A
			// stickied comment is the moderator's pinned rules, which is about
			// the subreddit rather than the post.
			if child.Kind != "t1" || child.Data.Stickied {
				continue
			}
			if text := strings.TrimSpace(child.Data.Body); text != "" && len(comments) < MaxComments {
				comments = append(comments, text)
			}
		}
	}
	return post, comments, imageURL, nil
}

func isImageURL(value string) bool {
	lowered := strings.ToLower(value)
	for _, extension := range []string{".jpg", ".jpeg", ".png", ".webp"} {
		if strings.HasSuffix(lowered, extension) {
			return true
		}
	}
	return false
}
