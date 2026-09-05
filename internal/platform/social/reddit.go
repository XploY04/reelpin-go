package social

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// Reddit endpoints. They are variables so tests can serve them locally.
var (
	redditTokenURL = "https://www.reddit.com/api/v1/access_token"
	redditAPIBase  = "https://oauth.reddit.com"
)

// MaxComments bounds how much of a thread is read. The top few carry the
// correction or the tip; the rest is noise.
const MaxComments = 8

// RedditClient holds the application token. Reddit refuses datacenter IPs on
// the public endpoints, so everything goes through the authenticated API, and
// the token is cached rather than minted per request.
type RedditClient struct {
	clientID     string
	clientSecret string
	userAgent    string
	http         *safehttp.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func NewRedditClient(clientID, clientSecret, userAgent string, client *safehttp.Client) *RedditClient {
	if strings.TrimSpace(userAgent) == "" {
		userAgent = "reelpin/1.0"
	}
	return &RedditClient{
		clientID:     clientID,
		clientSecret: clientSecret,
		userAgent:    userAgent,
		http:         client,
	}
}

func (c *RedditClient) Configured() bool {
	return strings.TrimSpace(c.clientID) != "" && strings.TrimSpace(c.clientSecret) != ""
}

// accessToken returns a cached token, refreshing it a minute before it lapses.
func (c *RedditClient) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Before(c.expiresAt.Add(-time.Minute)) {
		return c.token, nil
	}
	if !c.Configured() {
		return "", fmt.Errorf("reddit credentials are not configured")
	}

	form := url.Values{"grant_type": {"client_credentials"}}.Encode()
	response, err := c.http.PostForm(ctx, redditTokenURL, form, map[string]string{
		"Authorization": "Basic " + base64.StdEncoding.EncodeToString(
			[]byte(c.clientID+":"+c.clientSecret)),
		"User-Agent": c.userAgent,
	})
	if err != nil {
		return "", fmt.Errorf("requesting a reddit token: %w", err)
	}
	if response.Status < 200 || response.Status >= 300 {
		// The body can echo the credentials back; only the status is reported.
		return "", fmt.Errorf("the reddit token endpoint returned HTTP %d", response.Status)
	}

	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil || payload.AccessToken == "" {
		return "", fmt.Errorf("the reddit token response was not usable")
	}

	c.token = payload.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)
	return c.token, nil
}

// ResolveShareLink turns /r/<sub>/s/<code> into its canonical permalink. It
// satisfies sourceidentity.RedditResolver, which is why identity resolution
// never has to know about OAuth.
func (c *RedditClient) ResolveShareLink(ctx context.Context, rawURL string) (string, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return "", err
	}

	apiURL := strings.Replace(rawURL, "https://www.reddit.com", redditAPIBase, 1)
	apiURL = strings.Replace(apiURL, "https://reddit.com", redditAPIBase, 1)

	response, err := c.http.GetWithHeaders(ctx, apiURL, map[string]string{
		"Authorization": "Bearer " + token,
		"User-Agent":    c.userAgent,
	})
	if err != nil {
		return "", err
	}
	// The API answers a share link with a redirect the client already followed,
	// so the final URL is the canonical permalink.
	if response.FinalURL == "" {
		return "", fmt.Errorf("the share link did not resolve")
	}
	return strings.Replace(response.FinalURL, redditAPIBase, "https://www.reddit.com", 1), nil
}

// RedditHandler reads a post and its top comments.
type RedditHandler struct {
	deps Deps
}

func NewReddit(deps Deps) *RedditHandler { return &RedditHandler{deps: deps} }

func (h *RedditHandler) Name() string { return "reddit" }

func (h *RedditHandler) Match(identity sourceidentity.SourceIdentity) bool {
	return identity.Platform == "reddit"
}

func (h *RedditHandler) Capabilities(sourceidentity.SourceIdentity) platform.Capabilities {
	return platform.Capabilities{Caption: true, Images: true}
}

func (h *RedditHandler) Normalize(_ context.Context, identity sourceidentity.SourceIdentity) (sourceidentity.SourceIdentity, error) {
	return identity, nil
}

func (h *RedditHandler) Prepare(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) (platform.Prepared, error) {
	if h.deps.Reddit == nil || !h.deps.Reddit.Configured() {
		return platform.Prepared{}, fmt.Errorf("reddit credentials are not configured")
	}

	token, err := h.deps.Reddit.accessToken(ctx)
	if err != nil {
		return platform.Prepared{}, err
	}

	endpoint := fmt.Sprintf("%s/comments/%s?%s", redditAPIBase, identity.ContentID,
		url.Values{"limit": {fmt.Sprint(MaxComments)}, "depth": {"1"}, "raw_json": {"1"}}.Encode())

	response, err := h.deps.Reddit.http.GetWithHeaders(ctx, endpoint, map[string]string{
		"Authorization": "Bearer " + token,
		"User-Agent":    h.deps.Reddit.userAgent,
	})
	if err != nil {
		return platform.Prepared{}, err
	}
	switch {
	case response.Status == 404:
		return platform.Prepared{}, ErrPostNotFound
	case response.Status == 401, response.Status == 403:
		return platform.Prepared{}, ErrPostProtected
	case response.Status < 200 || response.Status >= 300:
		return platform.Prepared{}, fmt.Errorf("the reddit api returned HTTP %d", response.Status)
	}

	post, comments, imageURL, err := parseListing(response.Body)
	if err != nil {
		return platform.Prepared{}, err
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
		return platform.Prepared{}, ErrNoPublicContent
	}

	prepared := platform.Prepared{
		Title:            post.Title,
		Caption:          post.Title,
		Transcript:       strings.Join(parts, "\n\n"),
		IngestionMethod:  "reddit_api",
		TranscriptSource: "reddit_post_text",
	}
	if imageURL != "" {
		prepared.ThumbnailURL = h.storeThumbnail(ctx, identity, imageURL)
	}
	return prepared, nil
}

type redditPost struct {
	Title    string
	SelfText string
}

// parseListing reads the two-element listing Reddit returns: the post, then its
// comments.
func parseListing(body []byte) (redditPost, []string, string, error) {
	var listing []struct {
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
	if err := json.Unmarshal(body, &listing); err != nil || len(listing) == 0 {
		return redditPost{}, nil, "", fmt.Errorf("the reddit response was not a listing")
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
			if child.Kind != "t1" || child.Data.Stickied {
				// Skip the moderator's pinned rules comment.
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

func (h *RedditHandler) storeThumbnail(ctx context.Context, identity sourceidentity.SourceIdentity, imageURL string) string {
	if h.deps.Storage == nil {
		return ""
	}
	response, err := h.deps.HTTP.Get(ctx, imageURL)
	if err != nil || response.Status < 200 || response.Status >= 300 {
		return ""
	}
	key := storageKey(identity)
	url, err := h.deps.Storage.Upload(ctx, key, strings.NewReader(string(response.Body)), "image/jpeg")
	if err != nil {
		return ""
	}
	return url
}
