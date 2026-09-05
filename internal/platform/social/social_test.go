package social

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/apify"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

type fakeUploader struct{ keys []string }

func (f *fakeUploader) Upload(_ context.Context, key string, body io.Reader, _ string) (string, error) {
	io.Copy(io.Discard, body)
	f.keys = append(f.keys, key)
	return "https://storage.example.com/" + key, nil
}

func testClient() *safehttp.Client {
	return safehttp.New(safehttp.Config{AllowPrivateAddresses: true})
}

const postID = "1234567890123456789"

func xIdentity() sourceidentity.SourceIdentity {
	return sourceidentity.SourceIdentity{
		NormalizedURL: "https://x.com/someone/status/" + postID,
		Platform:      "x",
		ContentType:   "post",
		ContentID:     postID,
	}
}

func serveOEmbed(t *testing.T, status int, body string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	previous := oembedEndpoint
	oembedEndpoint = server.URL
	t.Cleanup(func() {
		oembedEndpoint = previous
		server.Close()
	})
}

func TestXReadsThePublicPostText(t *testing.T) {
	serveOEmbed(t, 200, `{
		"provider_name":"Twitter",
		"url":"https://twitter.com/someone/status/`+postID+`",
		"author_name":"Someone",
		"html":"<blockquote><p lang=\"en\">Three cafes worth the ride &amp; a sunset spot<br>Go early</p>&mdash; Someone (@someone)</blockquote>"
	}`)

	handler := NewX(Deps{HTTP: testClient(), Apify: apify.New(apify.Config{}), Logger: quiet()})
	prepared, err := handler.Prepare(context.Background(), xIdentity(), t.TempDir())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if !strings.Contains(prepared.Transcript, "Three cafes worth the ride & a sunset spot") {
		t.Errorf("transcript = %q, want the decoded post text", prepared.Transcript)
	}
	if strings.Contains(prepared.Transcript, "<") || strings.Contains(prepared.Transcript, "@someone)") {
		t.Errorf("transcript = %q, want the markup and attribution stripped", prepared.Transcript)
	}
	if !strings.HasPrefix(prepared.Caption, "Someone: ") {
		t.Errorf("caption = %q, want the author kept", prepared.Caption)
	}
}

func TestXRefusesAResponseAboutAnotherPost(t *testing.T) {
	serveOEmbed(t, 200, `{
		"provider_name":"Twitter",
		"url":"https://x.com/someone/status/9999999999999999999",
		"html":"<blockquote><p>Some other post</p></blockquote>"
	}`)

	handler := NewX(Deps{HTTP: testClient(), Logger: quiet()})
	if _, err := handler.Prepare(context.Background(), xIdentity(), t.TempDir()); !errors.Is(err, ErrPostMismatch) {
		t.Fatalf("err = %v, want ErrPostMismatch", err)
	}
}

func TestXRefusesAnUnexpectedProvider(t *testing.T) {
	serveOEmbed(t, 200, `{"provider_name":"Somewhere Else","url":"https://x.com/someone/status/`+postID+`","html":"<p>x</p>"}`)

	handler := NewX(Deps{HTTP: testClient(), Logger: quiet()})
	if _, err := handler.Prepare(context.Background(), xIdentity(), t.TempDir()); err == nil {
		t.Fatal("a response from another provider was accepted")
	}
}

func TestXMapsStatusesToTerminalFailures(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{status: 404, want: ErrPostNotFound},
		{status: 401, want: ErrPostProtected},
		{status: 403, want: ErrPostProtected},
	}
	for _, tt := range tests {
		serveOEmbed(t, tt.status, `{}`)
		handler := NewX(Deps{HTTP: testClient(), Logger: quiet()})
		if _, err := handler.Prepare(context.Background(), xIdentity(), t.TempDir()); !errors.Is(err, tt.want) {
			t.Errorf("HTTP %d gave %v, want %v", tt.status, err, tt.want)
		}
	}
}

func TestXWithNoPublicTextIsTerminal(t *testing.T) {
	serveOEmbed(t, 200, `{"provider_name":"Twitter","url":"https://x.com/someone/status/`+postID+`","html":"<blockquote></blockquote>"}`)

	handler := NewX(Deps{HTTP: testClient(), Logger: quiet()})
	if _, err := handler.Prepare(context.Background(), xIdentity(), t.TempDir()); !errors.Is(err, ErrNoPublicContent) {
		t.Fatalf("err = %v, want ErrNoPublicContent", err)
	}
}

func TestTweetTextHandlesLineBreaks(t *testing.T) {
	got := tweetText(`<blockquote><p lang="en">First line<br />Second line</p>&mdash; A</blockquote>`)
	if got != "First line Second line" {
		t.Fatalf("text = %q", got)
	}
	if tweetText("<blockquote></blockquote>") != "" {
		t.Error("an empty embed produced text")
	}
}

// Reddit

func redditIdentity() sourceidentity.SourceIdentity {
	return sourceidentity.SourceIdentity{
		NormalizedURL: "https://www.reddit.com/comments/1abc234/",
		Platform:      "reddit",
		ContentType:   "post",
		ContentID:     "1abc234",
	}
}

func serveReddit(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	previousToken, previousAPI := redditTokenURL, redditAPIBase
	redditTokenURL = server.URL + "/api/v1/access_token"
	redditAPIBase = server.URL
	t.Cleanup(func() {
		redditTokenURL, redditAPIBase = previousToken, previousAPI
		server.Close()
	})
	return server
}

const redditListing = `[
  {"data":{"children":[{"kind":"t3","data":{
    "title":"Best cafes in Goa",
    "selftext":"Three worth the ride.",
    "preview":{"images":[{"source":{"url":"IMAGE_URL"}}]}
  }}]}},
  {"data":{"children":[
    {"kind":"t1","data":{"body":"Artjuna is the best","stickied":false}},
    {"kind":"t1","data":{"body":"Read the rules","stickied":true}},
    {"kind":"more","data":{"body":"ignored"}}
  ]}}
]`

func TestRedditCombinesTitleBodyAndTopComments(t *testing.T) {
	var tokenCalls int
	server := serveReddit(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "access_token"):
			tokenCalls++
			w.Write([]byte(`{"access_token":"token","expires_in":3600}`))
		case strings.HasSuffix(r.URL.Path, ".jpg"):
			w.Write([]byte("image"))
		default:
			if r.Header.Get("Authorization") != "Bearer token" {
				t.Errorf("the api call carried %q", r.Header.Get("Authorization"))
			}
			w.Write([]byte(redditListing))
		}
	})

	uploader := &fakeUploader{}
	client := NewRedditClient("id", "secret", "reelpin/test", testClient())
	handler := NewReddit(Deps{HTTP: testClient(), Reddit: client, Storage: uploader, Logger: quiet()})

	// Point the image at the local server.
	previous := redditListing
	_ = previous
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "access_token"):
			tokenCalls++
			w.Write([]byte(`{"access_token":"token","expires_in":3600}`))
		case strings.HasSuffix(r.URL.Path, ".jpg"):
			w.Write([]byte("image"))
		default:
			w.Write([]byte(strings.ReplaceAll(redditListing, "IMAGE_URL", server.URL+"/preview.jpg")))
		}
	})

	prepared, err := handler.Prepare(context.Background(), redditIdentity(), t.TempDir())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if prepared.Title != "Best cafes in Goa" {
		t.Errorf("title = %q", prepared.Title)
	}
	if !strings.Contains(prepared.Transcript, "Three worth the ride.") {
		t.Errorf("transcript = %q, want the body", prepared.Transcript)
	}
	if !strings.Contains(prepared.Transcript, "Artjuna is the best") {
		t.Errorf("transcript = %q, want the top comment", prepared.Transcript)
	}
	if strings.Contains(prepared.Transcript, "Read the rules") {
		t.Error("a stickied moderator comment was treated as content")
	}
	if prepared.ThumbnailURL == "" || len(uploader.keys) != 1 {
		t.Errorf("thumbnail = %q keys = %v", prepared.ThumbnailURL, uploader.keys)
	}

	// The token is cached, not minted per request.
	if _, err := handler.Prepare(context.Background(), redditIdentity(), t.TempDir()); err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	if tokenCalls != 1 {
		t.Errorf("the token endpoint was called %d times, want it cached", tokenCalls)
	}
}

func TestRedditWithoutCredentialsFailsClearly(t *testing.T) {
	handler := NewReddit(Deps{HTTP: testClient(), Reddit: NewRedditClient("", "", "", testClient()), Logger: quiet()})
	if _, err := handler.Prepare(context.Background(), redditIdentity(), t.TempDir()); err == nil {
		t.Fatal("an unconfigured reader reported success")
	}
}

func TestRedditTokenErrorsDoNotEchoTheBody(t *testing.T) {
	serveReddit(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid client id super-secret"}`))
	})

	client := NewRedditClient("id", "super-secret", "reelpin/test", testClient())
	_, err := client.accessToken(context.Background())
	if err == nil {
		t.Fatal("a rejected token request reported success")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("the error leaks the credentials: %v", err)
	}
}

func TestParseListingHandlesAnEmptyThread(t *testing.T) {
	post, comments, image, err := parseListing([]byte(`[{"data":{"children":[]}},{"data":{"children":[]}}]`))
	if err != nil {
		t.Fatalf("parseListing: %v", err)
	}
	if post.Title != "" || len(comments) != 0 || image != "" {
		t.Fatalf("post=%+v comments=%v image=%q", post, comments, image)
	}
	if _, _, _, err := parseListing([]byte(`{"not":"a listing"}`)); err == nil {
		t.Error("a non-listing response was accepted")
	}
}

// LinkedIn

func linkedInIdentity() sourceidentity.SourceIdentity {
	return sourceidentity.SourceIdentity{
		NormalizedURL: "https://www.linkedin.com/feed/update/urn:li:activity:7123456789012345678/",
		Platform:      "linkedin",
		ContentType:   "post",
		ContentID:     "7123456789012345678",
	}
}

func TestLinkedInCombinesBodyAndComments(t *testing.T) {
	var gotURLs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if urls, ok := body["urls"].([]any); ok {
			for _, entry := range urls {
				gotURLs = append(gotURLs, entry.(string))
			}
		}
		w.Write([]byte(`[{"text":"We shipped the thing","authorName":"Someone","comments":[{"text":"Congrats"}]}]`))
	}))
	defer server.Close()

	apify.SetBaseURLForTest(server.URL)
	client := apify.New(apify.Config{Token: "token", Actors: map[string]string{"linkedin": "apify/linkedin"}})

	handler := NewLinkedIn(Deps{HTTP: testClient(), Apify: client, Logger: quiet()})
	prepared, err := handler.Prepare(context.Background(), linkedInIdentity(), t.TempDir())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if !strings.Contains(prepared.Transcript, "We shipped the thing") ||
		!strings.Contains(prepared.Transcript, "Congrats") {
		t.Errorf("transcript = %q", prepared.Transcript)
	}
	// The urn type must survive: the same post has a different id under each.
	if len(gotURLs) != 1 || !strings.Contains(gotURLs[0], "urn:li:activity:7123456789012345678") {
		t.Errorf("the actor was given %v, want the urn preserved", gotURLs)
	}
}

func TestLinkedInWithoutAnActorIsADeploymentGap(t *testing.T) {
	handler := NewLinkedIn(Deps{HTTP: testClient(), Apify: apify.New(apify.Config{}), Logger: quiet()})
	if _, err := handler.Prepare(context.Background(), linkedInIdentity(), t.TempDir()); !errors.Is(err, ErrLinkedInNotConfigured) {
		t.Fatalf("err = %v, want ErrLinkedInNotConfigured", err)
	}
}

func TestLinkedInOnlyClaimsPosts(t *testing.T) {
	handler := NewLinkedIn(Deps{})
	if !handler.Match(linkedInIdentity()) {
		t.Error("the handler does not claim a post")
	}
	for _, contentType := range []string{"profile", "company", "article", "page"} {
		if handler.Match(sourceidentity.SourceIdentity{Platform: "linkedin", ContentType: contentType}) {
			t.Errorf("the handler claimed a %s page, which has ordinary metadata", contentType)
		}
	}
}
