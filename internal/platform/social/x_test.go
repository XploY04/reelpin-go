package social

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/apify"
	"github.com/XploY04/reelpin-go/internal/pipeline"
)

const xPostID = "1811111111111111111"

// servePost points the oEmbed endpoint at a local server for the length of one
// test.
func servePost(t *testing.T, body string, status int) {
	t.Helper()
	server := site(t, func(w http.ResponseWriter, _ *http.Request) string {
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return ""
		}
		return body
	})

	previous := oembedEndpoint
	oembedEndpoint = server.URL
	t.Cleanup(func() { oembedEndpoint = previous })
}

func xPost() (string, string) {
	return xPostID, "https://x.com/goacafes/status/" + xPostID
}

func TestAnXPostIsReadFromTheFreeEndpoint(t *testing.T) {
	servePost(t, fixture(t, "x_oembed.json"), http.StatusOK)
	id, rawURL := xPost()

	actor := &fakeActor{}
	deps := testDeps()
	deps.Apify = actor

	prepared, err := NewX(deps).Prepare(context.Background(), identity("x", "post", id, rawURL))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if prepared.NeedsMedia {
		t.Error("an X post asked for media; its words are the content")
	}
	if !strings.Contains(prepared.PageText, "cold brew") {
		t.Errorf("page text = %q", prepared.PageText)
	}
	if !strings.HasPrefix(prepared.Caption, "Goa Cafes: ") {
		t.Errorf("caption = %q, want the author attributed", prepared.Caption)
	}
	// The line break in the post survives as a space, and the attribution
	// footer the endpoint appends does not.
	if strings.Contains(prepared.PageText, "July 11") {
		t.Error("the oEmbed attribution line leaked into the post text")
	}
	if actor.runs != 0 {
		t.Error("the paid actor ran when the free endpoint had already answered")
	}
}

func TestTheActorAddsRepliesAndAPreview(t *testing.T) {
	server := site(t, func(w http.ResponseWriter, r *http.Request) string {
		if strings.Contains(r.URL.Path, "preview.jpg") {
			return "not-really-a-jpeg-but-bytes"
		}
		return fixture(t, "x_oembed.json")
	})
	previous := oembedEndpoint
	oembedEndpoint = server.URL
	t.Cleanup(func() { oembedEndpoint = previous })

	id, rawURL := xPost()
	uploader := &recordingUploader{}
	deps := testDeps()
	deps.Apify = &fakeActor{
		configured: map[string]bool{xActor: true},
		items:      actorItems(t, "x_actor.json", server.URL),
	}
	deps.Storage = uploader

	prepared, err := NewX(deps).Prepare(context.Background(), identity("x", "post", id, rawURL))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if !strings.Contains(prepared.PageText, "Replies:") {
		t.Fatalf("page text has no replies: %q", prepared.PageText)
	}
	if !strings.Contains(prepared.PageText, "pancakes") {
		t.Error("a reply is missing")
	}
	// The post's own row is the preview, not a reply.
	if strings.Contains(prepared.PageText, "Artjuna does the best cold brew in Anjuna.\nArtjuna") {
		t.Error("the post itself was added to its own replies")
	}
	if uploader.uploaded != 1 {
		t.Errorf("uploaded %d previews, want the one the actor found", uploader.uploaded)
	}
	if prepared.ThumbnailURL == "" {
		t.Error("no thumbnail was stored")
	}
}

func TestABlankReplyIsNotAReply(t *testing.T) {
	server := site(t, func(http.ResponseWriter, *http.Request) string {
		return fixture(t, "x_oembed.json")
	})
	previous := oembedEndpoint
	oembedEndpoint = server.URL
	t.Cleanup(func() { oembedEndpoint = previous })

	id, rawURL := xPost()
	deps := testDeps()
	deps.Apify = &fakeActor{
		configured: map[string]bool{xActor: true},
		items:      actorItems(t, "x_actor.json", server.URL),
	}

	prepared, err := NewX(deps).Prepare(context.Background(), identity("x", "post", id, rawURL))
	if err != nil {
		t.Fatal(err)
	}
	replies := strings.SplitN(prepared.PageText, "Replies:\n", 2)[1]
	if lines := strings.Split(strings.TrimSpace(replies), "\n"); len(lines) != 2 {
		t.Fatalf("replies = %q, want the two with words in them", lines)
	}
}

func TestAResponseAboutAnotherPostIsRefused(t *testing.T) {
	// Saving it would file someone else's post under this identity.
	servePost(t, fixture(t, "x_oembed_other_post.json"), http.StatusOK)
	id, rawURL := xPost()

	_, err := NewX(testDeps()).Prepare(context.Background(), identity("x", "post", id, rawURL))
	if !errors.Is(err, ErrPostMismatch) {
		t.Fatalf("err = %v, want a mismatch", err)
	}
	if failureOf(t, err).Retryable() {
		t.Error("a mismatch was made retryable; the answer will not change")
	}
}

func TestAResponseFromAnotherProviderIsRefused(t *testing.T) {
	servePost(t, `{"provider_name":"Facebook","url":"https://x.com/a/status/1811111111111111111","html":"<p>hi</p>"}`, http.StatusOK)
	id, rawURL := xPost()

	_, err := NewX(testDeps()).Prepare(context.Background(), identity("x", "post", id, rawURL))
	if !errors.Is(err, ErrPostMismatch) {
		t.Fatalf("err = %v, want a mismatch", err)
	}
}

func TestXTerminalAndRetryableStatusesAreToldApart(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		class     pipeline.Class
		code      string
		retryable bool
	}{
		{"deleted", http.StatusNotFound, pipeline.ContentTerminal, "content_unavailable", false},
		{"protected", http.StatusForbidden, pipeline.ContentTerminal, "content_private", false},
		{"throttled", http.StatusTooManyRequests, pipeline.ProviderExhausted, "provider_rate_limited", true},
		{"endpoint down", http.StatusBadGateway, pipeline.Transient, "provider_unavailable", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			servePost(t, "", tt.status)
			id, rawURL := xPost()

			_, err := NewX(testDeps()).Prepare(context.Background(), identity("x", "post", id, rawURL))
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

func TestAPostWithNoWordsIsTerminal(t *testing.T) {
	servePost(t, `{"provider_name":"Twitter","url":"https://x.com/a/status/1811111111111111111","html":"<blockquote></blockquote>"}`, http.StatusOK)
	id, rawURL := xPost()

	_, err := NewX(testDeps()).Prepare(context.Background(), identity("x", "post", id, rawURL))
	if !errors.Is(err, ErrNoPublicContent) {
		t.Fatalf("err = %v", err)
	}
	if failureOf(t, err).Retryable() {
		t.Error("an empty post was made retryable; it reads the same nothing next time")
	}
}

func TestAFailingActorDoesNotFailTheRun(t *testing.T) {
	// The post's words are already in hand, so enrichment is a bonus.
	servePost(t, fixture(t, "x_oembed.json"), http.StatusOK)
	id, rawURL := xPost()

	deps, logs := logged()
	deps.Apify = &fakeActor{
		configured: map[string]bool{xActor: true},
		err:        apify.ErrRateLimited,
	}

	prepared, err := NewX(deps).Prepare(context.Background(), identity("x", "post", id, rawURL))
	if err != nil {
		t.Fatalf("a throttled actor failed a run that already had its content: %v", err)
	}
	if !strings.Contains(prepared.PageText, "cold brew") {
		t.Error("the post text was lost when the actor failed")
	}
	if strings.Contains(logs.String(), "http") {
		t.Errorf("a URL reached the log: %s", logs.String())
	}
}

func TestTweetTextKeepsTheWordsAndDropsTheMarkup(t *testing.T) {
	text := tweetText(`<blockquote><p lang="en">First line<br>Second &amp; last</p>&mdash; someone</blockquote>`)
	if !strings.Contains(text, "First line") || !strings.Contains(text, "Second & last") {
		t.Fatalf("text = %q", text)
	}
	if strings.Contains(text, "<") || strings.Contains(text, "mdash") {
		t.Errorf("markup survived: %q", text)
	}
	if tweetText("no paragraph here") != "" {
		t.Error("text was invented from a body with no paragraph")
	}
}
