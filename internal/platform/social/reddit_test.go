package social

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/pipeline"
	"github.com/XploY04/reelpin-go/internal/platform/platformtest"
	"github.com/XploY04/reelpin-go/internal/reddit"
)

const redditPostID = "1abcxyz"

// fakeTokens stands in for the credential minter, which lives outside this
// package because minting a token is a form POST the safe client does not
// expose.
type fakeTokens struct {
	token string
	err   error
	calls int
}

func (f *fakeTokens) AccessToken(context.Context) (string, error) {
	f.calls++
	return f.token, f.err
}

// serveReddit points both Reddit endpoints at a local server and reports which
// one was asked.
func serveReddit(t *testing.T, body string, status int) *[]string {
	t.Helper()
	asked := &[]string{}

	server := site(t, func(w http.ResponseWriter, r *http.Request) string {
		if strings.HasSuffix(r.URL.Path, ".jpg") {
			return "image-bytes"
		}
		*asked = append(*asked, r.URL.Path+"|"+r.Header.Get("Authorization"))
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return ""
		}
		return body
	})

	previousAPI, previousPublic := redditAPIBase, redditPublicBase
	redditAPIBase, redditPublicBase = server.URL, server.URL
	t.Cleanup(func() { redditAPIBase, redditPublicBase = previousAPI, previousPublic })
	return asked
}

func TestARedditPostAndItsBestCommentsAreRead(t *testing.T) {
	asked := serveReddit(t, fixture(t, "reddit_listing.json"), http.StatusOK)

	uploader := &platformtest.Uploader{}
	deps := testDeps()
	deps.Thumbnails.Storage = uploader

	prepared, err := NewReddit(deps).Prepare(context.Background(),
		identity("reddit", "post", redditPostID, "https://www.reddit.com/comments/"+redditPostID+"/"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if prepared.NeedsMedia {
		t.Error("a Reddit post asked for media")
	}
	if prepared.Caption != "Best cold brew in North Goa?" {
		t.Errorf("caption = %q", prepared.Caption)
	}
	if !strings.Contains(prepared.PageText, "opens early") {
		t.Error("the post body was dropped")
	}
	if !strings.Contains(prepared.PageText, "Artjuna in Anjuna") {
		t.Error("the comments were dropped")
	}
	if uploader.Uploads != 1 || prepared.ThumbnailURL == "" {
		t.Errorf("uploaded %d previews, thumbnail %q", uploader.Uploads, prepared.ThumbnailURL)
	}

	// Without a credential the public endpoint answers, and no bearer token is
	// invented.
	if len(*asked) != 1 || strings.Contains((*asked)[0], "Bearer") {
		t.Fatalf("requests = %v, want one unauthenticated read", *asked)
	}
	if !strings.HasSuffix(strings.SplitN((*asked)[0], "|", 2)[0], ".json") {
		t.Errorf("the public endpoint was not used: %v", *asked)
	}
}

func TestTheModeratorsPinnedCommentIsNotContent(t *testing.T) {
	serveReddit(t, fixture(t, "reddit_listing.json"), http.StatusOK)

	prepared, err := NewReddit(testDeps()).Prepare(context.Background(),
		identity("reddit", "post", redditPostID, "https://www.reddit.com/comments/"+redditPostID+"/"))
	if err != nil {
		t.Fatal(err)
	}

	// A stickied comment is about the subreddit's rules, not this post.
	if strings.Contains(prepared.PageText, "subreddit rules") {
		t.Error("the pinned moderator comment was saved as content")
	}
	// A "more comments" placeholder is not a comment either.
	comments := strings.SplitN(prepared.PageText, "Top comments:\n", 2)[1]
	if lines := strings.Split(strings.TrimSpace(comments), "\n"); len(lines) != 2 {
		t.Fatalf("comments = %v, want the two real ones", lines)
	}
}

func TestACredentialIsUsedWhenThereIsOne(t *testing.T) {
	asked := serveReddit(t, fixture(t, "reddit_listing.json"), http.StatusOK)

	tokens := &fakeTokens{token: "app-token"}
	deps := testDeps()
	deps.Reddit = tokens

	if _, err := NewReddit(deps).Prepare(context.Background(),
		identity("reddit", "post", redditPostID, "https://www.reddit.com/comments/"+redditPostID+"/")); err != nil {
		t.Fatal(err)
	}

	if tokens.calls != 1 {
		t.Errorf("the token was minted %d times for one read", tokens.calls)
	}
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "Bearer app-token") {
		t.Fatalf("requests = %v, want the authenticated read", *asked)
	}
}

func TestAMissingTokenFallsBackToThePublicEndpoint(t *testing.T) {
	// Reddit throttles datacenter addresses on the public endpoints, so the
	// fallback may well fail; failing to even try would be worse.
	asked := serveReddit(t, fixture(t, "reddit_listing.json"), http.StatusOK)

	deps, logs := logged()
	deps.Reddit = &fakeTokens{err: errors.New("minting https://reddit.example/token failed")}

	prepared, err := NewReddit(deps).Prepare(context.Background(),
		identity("reddit", "post", redditPostID, "https://www.reddit.com/comments/"+redditPostID+"/"))
	if err != nil {
		t.Fatalf("a token failure lost the post: %v", err)
	}
	if prepared.Caption == "" {
		t.Error("nothing was read on the fallback path")
	}
	if len(*asked) != 1 || strings.Contains((*asked)[0], "Bearer") {
		t.Fatalf("requests = %v", *asked)
	}
	if strings.Contains(logs.String(), "http") {
		t.Errorf("the token endpoint URL reached the log: %s", logs.String())
	}
}

func TestUnconfiguredCredentialsReadThePublicEndpoint(t *testing.T) {
	asked := serveReddit(t, fixture(t, "reddit_listing.json"), http.StatusOK)

	deps := testDeps()
	deps.Reddit = reddit.New("", "")

	// A typed nil inside a non-nil interface passes this check and then panics
	// on the call, so the minter returns a plain nil when it has no credential.
	if deps.Reddit != nil {
		t.Fatalf("an unconfigured minter reached the handler as %#v", deps.Reddit)
	}

	prepared, err := NewReddit(deps).Prepare(context.Background(),
		identity("reddit", "post", redditPostID, "https://www.reddit.com/comments/"+redditPostID+"/"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Caption == "" {
		t.Error("nothing was read on the public path")
	}
	if len(*asked) != 1 || strings.Contains((*asked)[0], "Bearer") {
		t.Fatalf("requests = %v, want one unauthenticated read", *asked)
	}
}

func TestRedditStatusesAreToldApart(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		class     pipeline.Class
		code      string
		retryable bool
	}{
		{"removed", http.StatusNotFound, pipeline.ContentTerminal, "content_unavailable", false},
		{"quarantined", http.StatusForbidden, pipeline.ContentTerminal, "content_private", false},
		{"throttled", http.StatusTooManyRequests, pipeline.ProviderExhausted, "provider_rate_limited", true},
		{"reddit down", http.StatusServiceUnavailable, pipeline.Transient, "provider_unavailable", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serveReddit(t, "", tt.status)

			_, err := NewReddit(testDeps()).Prepare(context.Background(),
				identity("reddit", "post", redditPostID, "https://www.reddit.com/comments/"+redditPostID+"/"))

			failure := failureOf(t, err)
			if failure.Class != tt.class {
				t.Errorf("class = %v, want %v", failure.Class, tt.class)
			}
			if failure.Code != tt.code {
				t.Errorf("code = %q, want %q", failure.Code, tt.code)
			}
			if failure.Retryable() != tt.retryable {
				t.Errorf("retryable = %v, want %v", failure.Retryable(), tt.retryable)
			}
		})
	}
}

func TestAnUnparseableListingIsTerminal(t *testing.T) {
	serveReddit(t, `{"error": "not a listing"}`, http.StatusOK)

	_, err := NewReddit(testDeps()).Prepare(context.Background(),
		identity("reddit", "post", redditPostID, "https://www.reddit.com/comments/"+redditPostID+"/"))

	if !errors.Is(err, ErrNoPublicContent) {
		t.Fatalf("err = %v", err)
	}
	if failureOf(t, err).Retryable() {
		t.Error("a malformed listing was made retryable; it parses the same next time")
	}
}

func TestAPostWithNoTextAndNoImageIsTerminal(t *testing.T) {
	serveReddit(t, fixture(t, "reddit_empty.json"), http.StatusOK)

	_, err := NewReddit(testDeps()).Prepare(context.Background(),
		identity("reddit", "post", redditPostID, "https://www.reddit.com/comments/"+redditPostID+"/"))

	failure := failureOf(t, err)
	if failure.Code != "page_empty" || failure.Retryable() {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestAnImagePostKeepsItsImage(t *testing.T) {
	// A link post with no body text is still worth saving when it points at a
	// picture.
	body := `[{"data":{"children":[{"kind":"t3","data":{"title":"A photo","selftext":"",` +
		`"url_overridden_by_dest":"https://preview.example-cdn.com/photo.jpg","preview":{"images":[]}}}]}},` +
		`{"data":{"children":[]}}]`
	serveReddit(t, body, http.StatusOK)

	uploader := &platformtest.Uploader{}
	deps := testDeps()
	deps.Thumbnails.Storage = uploader

	prepared, err := NewReddit(deps).Prepare(context.Background(),
		identity("reddit", "post", redditPostID, "https://www.reddit.com/comments/"+redditPostID+"/"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if uploader.Uploads != 1 {
		t.Errorf("uploaded %d images", uploader.Uploads)
	}
	if prepared.Caption != "A photo" {
		t.Errorf("caption = %q", prepared.Caption)
	}
}

func TestOnlyImageLinksBecomeThumbnails(t *testing.T) {
	for _, tt := range []struct {
		url  string
		want bool
	}{
		{"https://example.com/a.jpg", true},
		{"https://example.com/a.JPEG", true},
		{"https://example.com/a.png", true},
		{"https://example.com/a.webp", true},
		{"https://example.com/article", false},
		{"", false},
	} {
		if got := isImageURL(tt.url); got != tt.want {
			t.Errorf("isImageURL(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}
