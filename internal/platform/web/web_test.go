package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/media"
	"github.com/XploY04/reelpin-go/internal/pipeline"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/platform/platformtest"
	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/XploY04/reelpin-go/internal/storage"
)

// Fixture reads a recorded page. Every handler test in this tree serves these
// from a local server, so no test needs a network.
func fixture(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return string(body)
}

// safeClient points the real client at a local server. Loopback is refused in
// production, which is why the seam exists.
// articleCDN is the host the article fixture points its og:image at.
const articleCDN = "https://cdn.example.com"

func safeClient() *safehttp.Client {
	return safehttp.New(safehttp.Config{AllowPrivateAddresses: true})
}

func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func page(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(body))
	}
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}
}

// newHandler wires one handler. A nil uploader is a deployment with no
// storage credential, and the handler is expected to survive it.
func newHandler(uploader storage.Uploader) *Handler {
	client := safeClient()
	limits := providers.NewLimits()
	return New(Deps{
		HTTP:       client,
		Thumbnails: platform.Thumbnails{HTTP: client, Storage: uploader, Limits: limits},
		Limit:      limits,
	})
}

func identityFor(url string) sourceidentity.SourceIdentity {
	return sourceidentity.SourceIdentity{
		NormalizedURL: url,
		OriginalURL:   url,
		Platform:      PlatformName,
		ContentType:   "link",
		ContentID:     "test-content",
	}
}

func TestParseMetadataPrefersOpenGraph(t *testing.T) {
	metadata := ParseMetadata(fixture(t, "article.html"))

	if metadata.Title != "A guide to sourdough" {
		t.Errorf("title = %q, want the Open Graph title over <title>", metadata.Title)
	}
	if !strings.Contains(metadata.Description, "two mistakes") {
		t.Errorf("description = %q", metadata.Description)
	}
	if metadata.ImageURL != "https://cdn.example.com/sourdough.jpg" {
		t.Errorf("image = %q", metadata.ImageURL)
	}
	if metadata.Canonical != "https://example.com/guide/sourdough" {
		t.Errorf("canonical = %q", metadata.Canonical)
	}
	if metadata.SiteName != "Example Kitchen" {
		t.Errorf("site name = %q", metadata.SiteName)
	}
}

func TestParseMetadataFallsBackToTheTitleTag(t *testing.T) {
	metadata := ParseMetadata(`<html><head><title>  Plain &amp; simple  </title></head></html>`)
	if metadata.Title != "Plain & simple" {
		t.Fatalf("title = %q, want the unescaped, trimmed <title>", metadata.Title)
	}
}

func TestMalformedMarkupYieldsSomethingRatherThanPanicking(t *testing.T) {
	// A truncated page is common: a CDN cut the response, a renderer bailed.
	// The parser reads what it can and reports the rest unusable.
	metadata := ParseMetadata(fixture(t, "malformed.html"))
	if metadata.Usable() {
		t.Errorf("a truncated page reported itself usable: %+v", metadata)
	}
}

func TestReadableTextDropsScriptsAndStyles(t *testing.T) {
	text := ReadableText(fixture(t, "article.html"))

	for _, unwanted := range []string{"window.analytics", "font-family", "Enable JavaScript"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("readable text kept %q, which is markup rather than meaning", unwanted)
		}
	}
	if !strings.Contains(text, "flour and water") {
		t.Errorf("readable text lost the article body:\n%s", text)
	}
}

func TestReadableTextIsBounded(t *testing.T) {
	// A page can be megabytes of boilerplate; the extractor pays per token.
	huge := "<p>" + strings.Repeat("word ", MaxPageTextRunes) + "</p>"
	text := ReadableText(huge)
	if runes := len([]rune(text)); runes > MaxPageTextRunes {
		t.Fatalf("readable text is %d runes, cap is %d", runes, MaxPageTextRunes)
	}
}

func TestReadableTextCutsOnARuneBoundary(t *testing.T) {
	// Splitting a multibyte character would hand invalid UTF-8 to a model.
	text := ReadableText("<p>" + strings.Repeat("た", MaxPageTextRunes+500) + "</p>")
	if !utf8Valid(text) {
		t.Fatal("truncation produced invalid UTF-8")
	}
}

func utf8Valid(value string) bool {
	for _, r := range value {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestPrepareReadsAnArticleAsLightWork(t *testing.T) {
	server := serve(t, page(fixture(t, "article.html")))

	prepared, err := newHandler(nil).Prepare(context.Background(), identityFor(server.URL))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.NeedsMedia {
		t.Error("a generic page asked for media; this service has no download path for one")
	}
	if !strings.Contains(prepared.Caption, "A guide to sourdough") {
		t.Errorf("caption = %q", prepared.Caption)
	}
	if !strings.Contains(prepared.PageText, "flour and water") {
		t.Errorf("page text = %q", prepared.PageText)
	}
	// No uploader is a deployment with no storage credential. The article
	// still saves; it just saves without a preview.
	if prepared.ThumbnailURL != "" {
		t.Errorf("thumbnail = %q with no uploader configured, want none", prepared.ThumbnailURL)
	}
}

func TestAnArticleThumbnailIsStoredRatherThanLinked(t *testing.T) {
	server := platformtest.Site(t, fixture(t, "article.html"), articleCDN)
	uploader := &platformtest.Uploader{}

	prepared, err := newHandler(uploader).Prepare(context.Background(), identityFor(server.URL))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if uploader.Uploads != 1 {
		t.Fatalf("uploaded %d previews, want 1", uploader.Uploads)
	}
	// The reader renders images out of our own bucket and nowhere else, so a
	// publisher's CDN URL saved as-is is a link with no preview at all.
	if !strings.HasPrefix(prepared.ThumbnailURL, platformtest.StoredPrefix) {
		t.Errorf("thumbnail = %q, want the stored object", prepared.ThumbnailURL)
	}
}

func TestAFailedThumbnailUploadStillSavesTheArticle(t *testing.T) {
	server := platformtest.Site(t, fixture(t, "article.html"), articleCDN)
	uploader := &platformtest.Uploader{Err: errors.New("the bucket refused")}

	prepared, err := newHandler(uploader).Prepare(context.Background(), identityFor(server.URL))
	if err != nil {
		t.Fatalf("Prepare: %v: a missing preview must not fail the run", err)
	}
	if prepared.ThumbnailURL != "" {
		t.Errorf("thumbnail = %q after a failed upload, want none", prepared.ThumbnailURL)
	}
	if !strings.Contains(prepared.PageText, "flour and water") {
		t.Error("the article itself was lost with the preview")
	}
}

func TestPrepareRejectsAPageWithNothingOnIt(t *testing.T) {
	server := serve(t, page(fixture(t, "empty.html")))

	_, err := newHandler(nil).Prepare(context.Background(), identityFor(server.URL))
	if err == nil {
		t.Fatal("an empty page prepared successfully")
	}
	assertTerminal(t, err, "page_empty")
}

func TestPrepareClassifiesTheStatusItGot(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		wantCode  string
		retryable bool
	}{
		{"gone", http.StatusNotFound, "content_unavailable", false},
		{"private", http.StatusForbidden, "login_required", false},
		{"throttled", http.StatusTooManyRequests, "provider_rate_limited", true},
		{"broken", http.StatusBadGateway, "provider_unavailable", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := serve(t, status(tt.status))

			_, err := newHandler(nil).Prepare(context.Background(), identityFor(server.URL))
			if err == nil {
				t.Fatalf("status %d prepared successfully", tt.status)
			}

			var failure *pipeline.Failure
			if !errors.As(err, &failure) {
				t.Fatalf("err = %v, want a classified failure", err)
			}
			if failure.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", failure.Code, tt.wantCode)
			}
			if failure.Retryable() != tt.retryable {
				t.Errorf("retryable = %v, want %v", failure.Retryable(), tt.retryable)
			}
		})
	}
}

func TestDownloadIsRefusedRatherThanPanicking(t *testing.T) {
	// Prepare never asks for media, so this is unreachable today. If routing
	// ever changes, one job fails rather than the worker crashing.
	_, err := New(Deps{}).Download(context.Background(), identityFor("https://example.com"), t.TempDir())
	assertTerminal(t, err, "source_not_supported")
}

func TestClassifyMapsTheDownloaderSentinels(t *testing.T) {
	tests := []struct {
		err       error
		wantCode  string
		retryable bool
	}{
		{media.ErrLoginRequired, "login_required", false},
		{media.ErrPrivate, "content_private", false},
		{media.ErrUnavailable, "content_unavailable", false},
		{media.ErrNotAdmitted, "source_not_supported", false},
		{media.ErrTooLong, "media_too_long", false},
		{media.ErrTooLarge, "media_too_large", false},
		{media.ErrRateLimited, "provider_rate_limited", true},
		{safehttp.ErrUnsafeURL, "source_not_supported", false},
	}

	for _, tt := range tests {
		t.Run(tt.wantCode, func(t *testing.T) {
			var failure *pipeline.Failure
			if !errors.As(Classify(tt.err), &failure) {
				t.Fatalf("%v was not classified", tt.err)
			}
			if failure.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", failure.Code, tt.wantCode)
			}
			if failure.Retryable() != tt.retryable {
				t.Errorf("retryable = %v, want %v", failure.Retryable(), tt.retryable)
			}
		})
	}
}

func TestClassifyLeavesAnUnknownErrorToThePipeline(t *testing.T) {
	// The pipeline's own classifier has the last word on anything a handler
	// does not recognise; swallowing it here would hide a bug as terminal.
	unknown := errors.New("something new")
	if got := Classify(unknown); !errors.Is(got, unknown) {
		t.Fatalf("Classify(%v) = %v, want the error unchanged", unknown, got)
	}
}

func TestClassifyKeepsAnAlreadyDecidedFailure(t *testing.T) {
	decided := Terminal("already_decided", "The handler knew.", errors.New("cause"))
	var failure *pipeline.Failure
	if !errors.As(Classify(decided), &failure) || failure.Code != "already_decided" {
		t.Fatalf("Classify reclassified a decided failure: %v", failure)
	}
}

func assertTerminal(t *testing.T, err error, wantCode string) {
	t.Helper()
	var failure *pipeline.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want a classified failure", err)
	}
	if failure.Code != wantCode {
		t.Errorf("code = %q, want %q", failure.Code, wantCode)
	}
	if failure.Retryable() {
		t.Errorf("%q is retryable; another attempt reads the same page", failure.Code)
	}
}
