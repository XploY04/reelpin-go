package social

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/apify"
	"github.com/XploY04/reelpin-go/internal/pipeline"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return string(body)
}

// cdnHosts are the hosts fixtures point at. Every one is rewritten to the test
// server before a fixture is served.
var cdnHosts = []string{
	"https://pbs.example-cdn.com",
	"https://media.example-cdn.com",
	"https://preview.example-cdn.com",
}

// jsonEscaped is how a URL appears inside a JSON string body: the slashes are
// escaped. Real provider payloads use this form, and a rewriter that only
// handles the plain form leaves those URLs pointing at the real CDN, so the
// test quietly reaches the internet and passes for the wrong reason.
func jsonEscaped(rawURL string) string {
	return strings.ReplaceAll(rawURL, "/", `\/`)
}

func rewriteHosts(body, serverURL string) string {
	for _, host := range cdnHosts {
		body = strings.ReplaceAll(body, host, serverURL)
		body = strings.ReplaceAll(body, jsonEscaped(host), jsonEscaped(serverURL))
	}
	return body
}

// site serves fixtures with every CDN host rewritten to itself, so no test
// touches a network.
func site(t *testing.T, handler func(w http.ResponseWriter, r *http.Request) string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := handler(w, r)
		if body != "" {
			io.WriteString(w, rewriteHosts(body, server.URL))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func testDeps() Deps {
	return Deps{
		HTTP:   safehttp.New(safehttp.Config{AllowPrivateAddresses: true}),
		Limit:  providers.NewLimits(),
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

// logged captures what a handler writes, so a test can prove no URL or token
// reaches a log line.
func logged() (Deps, *bytes.Buffer) {
	buffer := &bytes.Buffer{}
	deps := testDeps()
	deps.Logger = slog.New(slog.NewJSONHandler(buffer, nil))
	return deps, buffer
}

// fakeActor stands in for Apify. It records what it was asked for so a test
// can prove the expensive rung was or was not reached.
type fakeActor struct {
	configured map[string]bool
	items      []json.RawMessage
	err        error
	runs       int
}

func (f *fakeActor) Configured(platform string) bool { return f.configured[platform] }

func (f *fakeActor) Run(_ context.Context, _ string, _ any) ([]json.RawMessage, error) {
	f.runs++
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
}

func actorItems(t *testing.T, name, serverURL string) []json.RawMessage {
	t.Helper()
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(rewriteHosts(fixture(t, name), serverURL)), &items); err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return items
}

// recordingUploader stands in for object storage.
type recordingUploader struct {
	uploaded int
	lastKey  string
}

func (r *recordingUploader) Upload(_ context.Context, key string, _ io.Reader, _ string) (string, error) {
	r.uploaded++
	r.lastKey = key
	return "https://storage.example/" + key, nil
}

func failureOf(t *testing.T, err error) *pipeline.Failure {
	t.Helper()
	var failure *pipeline.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want a classified pipeline failure", err)
	}
	return failure
}

func identity(platformName, contentType, contentID, rawURL string) sourceidentity.SourceIdentity {
	return sourceidentity.SourceIdentity{
		OriginalURL:   rawURL,
		NormalizedURL: rawURL,
		Platform:      platformName,
		ContentType:   contentType,
		ContentID:     contentID,
		Scope:         sourceidentity.PublicScope(),
	}
}

func TestTheRewriterCatchesBothURLForms(t *testing.T) {
	// The escaped form is the one that slips through: a fixture is JSON, and
	// JSON escapes its slashes.
	body := `{"a":"https://pbs.example-cdn.com/x.jpg","b":"https:\/\/pbs.example-cdn.com\/y.jpg"}`
	rewritten := rewriteHosts(body, "http://127.0.0.1:1234")

	if strings.Contains(rewritten, "example-cdn.com") {
		t.Fatalf("a CDN host survived the rewrite: %s", rewritten)
	}
}

func TestNoFixtureEscapesToTheInternet(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		body := rewriteHosts(fixture(t, entry.Name()), "http://127.0.0.1:1234")
		// twitter.com appears as post identity, which is parsed rather than
		// fetched; anything else absolute would be a real request.
		for _, host := range []string{"example-cdn.com", "reddit.com", "linkedin.com"} {
			if strings.Contains(body, host) {
				t.Errorf("%s still points at %s after rewriting", entry.Name(), host)
			}
		}
	}
}

func TestTheRegistryRefusesADuplicatePlatform(t *testing.T) {
	deps := testDeps()

	if _, err := platform.NewRegistry(NewX(deps), NewLinkedIn(deps), NewReddit(deps)); err != nil {
		t.Fatalf("three distinct platforms were refused: %v", err)
	}

	// Two handlers claiming one platform would shadow each other silently, so
	// construction fails instead.
	_, err := platform.NewRegistry(NewX(deps), NewReddit(deps), NewX(deps))
	if err == nil {
		t.Fatal("a duplicate registration was accepted")
	}
	if !strings.Contains(err.Error(), XPlatform) {
		t.Errorf("err = %v, want it to name the duplicated platform", err)
	}
}

func TestEveryHandlerReportsItsOwnPlatform(t *testing.T) {
	deps := testDeps()
	for _, tt := range []struct {
		handler platform.Handler
		want    string
	}{
		{NewX(deps), "x"},
		{NewLinkedIn(deps), "linkedin"},
		{NewReddit(deps), "reddit"},
	} {
		if got := tt.handler.Platform(); got != tt.want {
			t.Errorf("Platform() = %q, want %q", got, tt.want)
		}
	}
}

func TestNoHandlerAsksForMedia(t *testing.T) {
	// The download tool admits only the three video hosts it is allowlisted
	// for, so none of these three can produce a media job. Download says so
	// rather than panicking, in case routing ever changes.
	deps := testDeps()
	for _, handler := range []platform.Handler{NewX(deps), NewLinkedIn(deps), NewReddit(deps)} {
		media, err := handler.Download(context.Background(),
			identity(handler.Platform(), "post", "1", "https://example.com/x"), t.TempDir())
		if media != nil {
			t.Errorf("%s returned media", handler.Platform())
		}
		failure := failureOf(t, err)
		if failure.Retryable() {
			t.Errorf("%s made a routing mistake retryable", handler.Platform())
		}
	}
}

func TestClassifyMapsEachFailureToItsClass(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		class     pipeline.Class
		code      string
		retryable bool
	}{
		{"gone", ErrPostNotFound, pipeline.ContentTerminal, "content_unavailable", false},
		{"protected", ErrPostProtected, pipeline.ContentTerminal, "content_private", false},
		{"wrong post", ErrPostMismatch, pipeline.ContentTerminal, "content_unavailable", false},
		{"nothing to read", ErrNoPublicContent, pipeline.ContentTerminal, "page_empty", false},
		{"no credential", ErrNotConfigured, pipeline.Transient, "provider_unavailable", true},
		{"actor throttled", apify.ErrRateLimited, pipeline.ProviderExhausted, "provider_rate_limited", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure := failureOf(t, classify(tt.err))
			if failure.Class != tt.class {
				t.Errorf("class = %v, want %v", failure.Class, tt.class)
			}
			if failure.Code != tt.code {
				t.Errorf("code = %q, want %q", failure.Code, tt.code)
			}
			if failure.Retryable() != tt.retryable {
				t.Errorf("retryable = %v, want %v", failure.Retryable(), tt.retryable)
			}
			if !errors.Is(failure, tt.err) {
				t.Error("the original error was not wrapped, so a log loses the cause")
			}
			if strings.Contains(failure.Message, "actor") || strings.Contains(failure.Message, "apify") {
				t.Errorf("the public message names a provider: %q", failure.Message)
			}
		})
	}
}

func TestAnUnrecognisedErrorStaysTheCallersProblem(t *testing.T) {
	// The pipeline's own classifier gets the last word on anything this
	// package does not recognise, so a bug stays loud instead of being
	// mislabelled as a content problem.
	mystery := errors.New("something we did not anticipate")
	if got := classify(mystery); !errors.Is(got, mystery) {
		t.Fatalf("classify(%v) = %v", mystery, got)
	}
	var failure *pipeline.Failure
	if errors.As(classify(mystery), &failure) {
		t.Fatal("an unrecognised error was given a class it does not deserve")
	}
}

func TestRedactRemovesEveryURL(t *testing.T) {
	err := errors.New("GET https://oauth.reddit.com/comments/abc?token=secret failed")
	redacted := redact(err)

	if strings.Contains(redacted, "http") || strings.Contains(redacted, "secret") {
		t.Fatalf("redact left an address behind: %q", redacted)
	}
	if redact(nil) != "" {
		t.Error("redact(nil) should be empty")
	}
}

func TestStatusErrorsSplitTerminalFromRetryable(t *testing.T) {
	for status, want := range map[int]error{
		404: ErrPostNotFound,
		410: ErrPostNotFound,
		401: ErrPostProtected,
		403: ErrPostProtected,
	} {
		if got := statusError(status); !errors.Is(got, want) {
			t.Errorf("status %d = %v, want %v", status, got, want)
		}
	}
	if statusError(200) != nil {
		t.Error("a 200 was treated as an error")
	}

	// A 429 or a 500 is about the server, so the shared classifier decides.
	failure := failureOf(t, classify(statusError(429)))
	if failure.Class != pipeline.ProviderExhausted {
		t.Errorf("429 class = %v, want provider exhausted", failure.Class)
	}
	failure = failureOf(t, classify(statusError(503)))
	if failure.Class != pipeline.Transient {
		t.Errorf("503 class = %v, want transient", failure.Class)
	}
}
