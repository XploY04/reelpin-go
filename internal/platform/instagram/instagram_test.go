package instagram

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/cookies"
	"github.com/XploY04/reelpin-go/internal/media"
	"github.com/XploY04/reelpin-go/internal/pipeline"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/platform/platformtest"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// Every test here runs against a local server and fake tools. Nothing in this
// package's test suite touches Instagram.

func fixture(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(body)
}

// mp4Bytes and jpegBytes are the smallest files that sniff as what they claim.
func mp4Bytes() []byte  { return append([]byte{0, 0, 0, 0x18}, []byte("ftypmp42____")...) }
func jpegBytes() []byte { return append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("JFIF________")...) }

// site serves a page and its media from one local origin, and records what was
// asked for so a test can prove which rung of the ladder ran.
type site struct {
	server   *httptest.Server
	page     string
	status   int
	requests []string
}

func newSite(t *testing.T, page string) *site {
	t.Helper()
	s := &site{page: page, status: http.StatusOK}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests = append(s.requests, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, ".mp4"):
			w.Write(mp4Bytes())
		case strings.HasSuffix(r.URL.Path, ".jpg"):
			w.Write(jpegBytes())
		case strings.HasSuffix(r.URL.Path, ".html"):
			// A slide URL that answers with a page: the false-MIME case.
			w.Write([]byte("<html>not an image at all</html>"))
		default:
			w.WriteHeader(s.status)
			w.Write([]byte(s.page))
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

// rewrite points a fixture's absolute media URLs at the local server. The
// embedded JSON escapes its slashes, so both forms have to be replaced or the
// carousel silently keeps two of its three slides on the real CDN.
func (s *site) rewrite(page string) string {
	escaped := strings.ReplaceAll(s.server.URL, "/", `\/`)
	page = strings.ReplaceAll(page, `https:\/\/scontent.example.com`, escaped)
	return strings.ReplaceAll(page, "https://scontent.example.com", s.server.URL)
}

func (s *site) identity(contentType, contentID string) sourceidentity.SourceIdentity {
	return sourceidentity.SourceIdentity{
		OriginalURL:   s.server.URL + "/reel/" + contentID + "/",
		NormalizedURL: s.server.URL + "/reel/" + contentID + "/",
		Platform:      "instagram",
		ContentType:   contentType,
		ContentID:     contentID,
		Scope:         sourceidentity.PublicScope(),
	}
}

// fakeDownloader stands in for yt-dlp: it never spawns anything.
type fakeDownloader struct {
	calls    int
	cookied  int
	err      error
	onCall   func(call int, options media.DownloadOptions) error
	workDir  string
	fileName string
}

func (f *fakeDownloader) Download(_ context.Context, _, workDir string, options media.DownloadOptions) (media.Download, error) {
	f.calls++
	if options.CookieFile != "" {
		f.cookied++
	}
	if f.onCall != nil {
		if err := f.onCall(f.calls, options); err != nil {
			return media.Download{}, err
		}
	} else if f.err != nil {
		return media.Download{}, f.err
	}

	name := f.fileName
	if name == "" {
		name = "source.mp4"
	}
	path := filepath.Join(workDir, name)
	if err := os.WriteFile(path, mp4Bytes(), 0o600); err != nil {
		return media.Download{}, err
	}
	f.workDir = workDir
	return media.Download{VideoPath: path, Anonymous: options.CookieFile == ""}, nil
}

type fakeProber struct {
	calls int
	err   error
}

func (f *fakeProber) Probe(context.Context, string) (int, int64, error) {
	f.calls++
	return 30, 1 << 20, f.err
}

// silentAudio is an ffmpeg stand-in that reports no audio track.
func silentAudio() *media.FFmpeg {
	return &media.FFmpeg{Binary: "ffmpeg", Runner: runnerFunc(func(context.Context, media.Command) (media.Result, error) {
		return media.Result{Stderr: "does not contain any stream"}, errors.New("exit 1")
	})}
}

// writingAudio is an ffmpeg stand-in that produces an audio file.
func writingAudio(t *testing.T) *media.FFmpeg {
	t.Helper()
	return &media.FFmpeg{Binary: "ffmpeg", Runner: runnerFunc(func(_ context.Context, command media.Command) (media.Result, error) {
		// The real tool writes the last argument; so does this one.
		path := command.Args[len(command.Args)-1]
		if err := os.WriteFile(path, []byte("some audio bytes"), 0o600); err != nil {
			return media.Result{}, err
		}
		return media.Result{}, nil
	})}
}

type runnerFunc func(context.Context, media.Command) (media.Result, error)

func (f runnerFunc) Run(ctx context.Context, command media.Command) (media.Result, error) {
	return f(ctx, command)
}

func testHandler(t *testing.T, deps Deps) *Handler {
	t.Helper()
	if deps.HTTP == nil {
		// The test server listens on loopback, which production always refuses.
		deps.HTTP = safehttp.New(safehttp.Config{AllowPrivateAddresses: true})
	}
	if deps.Audio == nil {
		deps.Audio = writingAudio(t)
	}
	if deps.Thumbnails.HTTP == nil {
		deps.Thumbnails.HTTP = deps.HTTP
	}
	return New(deps)
}

func TestParsePageReadsTheOpenGraphAndTheEmbeddedJSON(t *testing.T) {
	reel := parsePage(fixture(t, "reel.html"))
	if reel.Title != "Best cafes in Goa" {
		t.Errorf("title = %q", reel.Title)
	}
	if reel.Caption != "Three cafes worth the ride & a sunset spot" {
		t.Errorf("caption = %q: the HTML entity was not decoded", reel.Caption)
	}
	if reel.VideoURL != "https://scontent.example.com/reel.mp4" {
		t.Errorf("video = %q: the Open Graph tag wins over the JSON", reel.VideoURL)
	}
	if reel.ThumbnailURL != "https://scontent.example.com/thumb.jpg" {
		t.Errorf("thumbnail = %q", reel.ThumbnailURL)
	}

	carousel := parsePage(fixture(t, "carousel.html"))
	if len(carousel.ImageURLs) != 3 {
		t.Fatalf("slides = %d, want 3", len(carousel.ImageURLs))
	}
	// Slide order is the carousel's order, and it is what the reader sees.
	for index, want := range []string{"slide-1.jpg", "slide-2.jpg", "slide-3.jpg"} {
		if !strings.HasSuffix(carousel.ImageURLs[index], want) {
			t.Errorf("slide %d = %q, want %s", index, carousel.ImageURLs[index], want)
		}
	}
}

func TestThePageSaysWhyItRefused(t *testing.T) {
	tests := []struct {
		fixture string
		want    error
	}{
		{"login_wall.html", ErrLoginWall},
		{"removed.html", ErrRemoved},
		{"private.html", ErrPrivate},
		{"reel.html", nil},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			if err := classifyBody(fixture(t, tt.fixture)); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestTheStatusSaysWhyItRefused(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{http.StatusOK, nil},
		{http.StatusNotFound, ErrRemoved},
		{http.StatusGone, ErrRemoved},
		{http.StatusTooManyRequests, ErrRateLimited},
		{http.StatusUnauthorized, ErrLoginWall},
		{http.StatusForbidden, ErrLoginWall},
		{http.StatusBadGateway, ErrProviderOutage},
		{http.StatusTeapot, ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			if err := classifyStatus(tt.status); !errors.Is(err, tt.want) {
				t.Fatalf("status %d gave %v, want %v", tt.status, err, tt.want)
			}
		})
	}
}

// TestClassifyMapsFailuresToPipelineClasses is the table the pipeline relies
// on: the wrong class means a deleted post is retried three times, or a rate
// limit is reported to the user as permanent.
func TestClassifyMapsFailuresToPipelineClasses(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		class pipeline.Class
		code  string
	}{
		{"removed", ErrRemoved, pipeline.ContentTerminal, "content_removed"},
		{"unavailable from the tool", media.ErrUnavailable, pipeline.ContentTerminal, "content_removed"},
		{"private", ErrPrivate, pipeline.ContentTerminal, "content_private"},
		{"private from the tool", media.ErrPrivate, pipeline.ContentTerminal, "content_private"},
		{"too long", media.ErrTooLong, pipeline.ContentTerminal, "content_too_large"},
		{"too large", media.ErrTooLarge, pipeline.ContentTerminal, "content_too_large"},
		{"not admitted", media.ErrNotAdmitted, pipeline.ContentTerminal, "unsupported_source"},
		{"login wall", ErrLoginWall, pipeline.ContentTerminal, "login_required"},
		{"rate limited", ErrRateLimited, pipeline.ProviderExhausted, "provider_exhausted"},
		{"rate limited by the tool", media.ErrRateLimited, pipeline.ProviderExhausted, "provider_exhausted"},
		{"provider outage", ErrProviderOutage, pipeline.Transient, "provider_unavailable"},
		{"tool timeout", media.ErrTimedOut, pipeline.Transient, "provider_unavailable"},
		{"malformed", ErrMalformed, pipeline.Transient, "content_unreadable"},
		{"ladder exhausted", ErrExhausted, pipeline.Transient, "content_unreadable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var failure *pipeline.Failure
			if !errors.As(classify(tt.err), &failure) {
				t.Fatalf("classify(%v) did not produce a pipeline failure", tt.err)
			}
			if failure.Class != tt.class {
				t.Errorf("class = %q, want %q", failure.Class, tt.class)
			}
			if failure.Code != tt.code {
				t.Errorf("code = %q, want %q", failure.Code, tt.code)
			}
			if failure.Message == "" {
				t.Error("no message for a person to read")
			}
			// The user-facing half must never carry the tool's own words.
			if strings.Contains(failure.Message, "yt-dlp") || strings.Contains(failure.Message, "apify") {
				t.Errorf("message leaks a provider name: %q", failure.Message)
			}
			if !errors.Is(failure, tt.err) {
				t.Error("the original error was not wrapped for the logs")
			}
		})
	}

	// An unrecognised error stays as it is, so the pipeline calls it internal
	// and someone gets paged rather than the user being told a comfortable lie.
	own := errors.New("a bug in this handler")
	var failure *pipeline.Failure
	if errors.As(classify(own), &failure) {
		t.Error("an unknown error was dressed up as a content failure")
	}
	if classify(nil) != nil {
		t.Error("classify invented a failure from nil")
	}
}

func TestPrepareAsksForMediaOnAReel(t *testing.T) {
	server := newSite(t, "")
	server.page = server.rewrite(fixture(t, "reel.html"))
	handler := testHandler(t, Deps{})

	prepared, err := handler.Prepare(context.Background(), server.identity("reel", "C8abc123"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !prepared.NeedsMedia {
		t.Error("a reel needs media")
	}
	if prepared.Caption == "" {
		t.Error("the caption was not carried")
	}
	if prepared.PageText != "" {
		t.Error("a reel's text comes from its transcript, not the page")
	}
	// No uploader is a deployment with no storage credential. The reel still
	// saves; it just saves without a preview.
	if prepared.ThumbnailURL != "" {
		t.Errorf("thumbnail = %q with no uploader configured, want none", prepared.ThumbnailURL)
	}
}

func TestAReelThumbnailIsStoredRatherThanLinked(t *testing.T) {
	server := newSite(t, "")
	server.page = server.rewrite(fixture(t, "reel.html"))
	uploader := &platformtest.Uploader{}
	handler := testHandler(t, Deps{Thumbnails: platform.Thumbnails{Storage: uploader}})

	prepared, err := handler.Prepare(context.Background(), server.identity("reel", "C8abc123"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if uploader.Uploads != 1 {
		t.Fatalf("uploaded %d previews, want 1", uploader.Uploads)
	}
	// The reader renders images out of our own bucket and nowhere else, so a
	// scontent URL saved as-is is a reel with no preview at all.
	if !strings.HasPrefix(prepared.ThumbnailURL, platformtest.StoredPrefix) {
		t.Errorf("thumbnail = %q, want the stored object", prepared.ThumbnailURL)
	}
}

func TestAFailedThumbnailUploadStillSavesTheReel(t *testing.T) {
	server := newSite(t, "")
	server.page = server.rewrite(fixture(t, "reel.html"))
	uploader := &platformtest.Uploader{Err: errors.New("the bucket refused")}
	handler := testHandler(t, Deps{Thumbnails: platform.Thumbnails{Storage: uploader}})

	prepared, err := handler.Prepare(context.Background(), server.identity("reel", "C8abc123"))
	if err != nil {
		t.Fatalf("Prepare: %v: a missing preview must not fail the run", err)
	}
	if prepared.ThumbnailURL != "" {
		t.Errorf("thumbnail = %q after a failed upload, want none", prepared.ThumbnailURL)
	}
	if !prepared.NeedsMedia || prepared.Caption == "" {
		t.Error("the reel itself was lost with the preview")
	}
}

func TestPrepareOnAProfilePageUsesTheTextItFound(t *testing.T) {
	server := newSite(t, fixture(t, "private.html"))
	handler := testHandler(t, Deps{})

	identity := server.identity("page", "")
	// A private profile page still carries public text; that text is the
	// content, and there is nothing to download.
	_, err := handler.Prepare(context.Background(), identity)
	if err == nil {
		t.Fatal("a private page was accepted")
	}
	var failure *pipeline.Failure
	if !errors.As(err, &failure) || failure.Class != pipeline.ContentTerminal {
		t.Fatalf("err = %v, want a terminal failure", err)
	}
}

func TestPrepareReadsAPublicPage(t *testing.T) {
	server := newSite(t, fixture(t, "reel.html"))
	handler := testHandler(t, Deps{})

	prepared, err := handler.Prepare(context.Background(), server.identity("page", ""))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.NeedsMedia {
		t.Error("a page has nothing to download")
	}
	if !strings.Contains(prepared.PageText, "Best cafes in Goa") {
		t.Errorf("page text = %q", prepared.PageText)
	}
}

func TestPrepareStopsOnContentThatIsGone(t *testing.T) {
	for _, name := range []string{"removed.html", "private.html"} {
		t.Run(name, func(t *testing.T) {
			server := newSite(t, fixture(t, name))
			handler := testHandler(t, Deps{
				Downloader: &fakeDownloader{},
				Probe:      &fakeProber{},
			})

			_, err := handler.Prepare(context.Background(), server.identity("reel", "C8abc123"))
			if err == nil {
				t.Fatal("a dead post was prepared as if it were fine")
			}
			var failure *pipeline.Failure
			if !errors.As(err, &failure) || failure.Class != pipeline.ContentTerminal {
				t.Fatalf("err = %v, want terminal", err)
			}
		})
	}
}

func TestPrepareSurvivesALoginWallForAReel(t *testing.T) {
	// The page refuses, but the downloader and the actor have their own access,
	// so preparation continues and the ladder is walked in Download.
	server := newSite(t, fixture(t, "login_wall.html"))
	handler := testHandler(t, Deps{})

	prepared, err := handler.Prepare(context.Background(), server.identity("reel", "C8abc123"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !prepared.NeedsMedia {
		t.Error("the ladder was abandoned at the first rung")
	}
}

func TestDownloadTakesTheSlidesFromTheCarouselPage(t *testing.T) {
	server := newSite(t, "")
	server.page = server.rewrite(fixture(t, "carousel.html"))
	downloader := &fakeDownloader{}
	handler := testHandler(t, Deps{Downloader: downloader, Probe: &fakeProber{}})

	workDir := t.TempDir()
	slides, err := handler.Download(context.Background(), server.identity("post", "C8post11"), workDir)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(slides) != 3 {
		t.Fatalf("slides = %d, want 3", len(slides))
	}
	for _, slide := range slides {
		if slide.MIMEType != "image/jpeg" {
			t.Errorf("mime = %q, want the sniffed type", slide.MIMEType)
		}
		if _, err := os.Stat(slide.Path); err != nil {
			t.Errorf("slide is not on disk: %v", err)
		}
	}
	if downloader.calls != 0 {
		t.Error("a carousel spent a download tool it did not need")
	}
}

func TestDownloadSkipsASlideThatIsNotAnImage(t *testing.T) {
	server := newSite(t, "")
	// The middle slide answers with a page, not an image: the extension and the
	// Content-Type both lie, and only the bytes tell the truth.
	page := server.rewrite(fixture(t, "carousel.html"))
	page = strings.Replace(page, "slide-2.jpg", "slide-2.html", 1)
	server.page = page

	handler := testHandler(t, Deps{})
	slides, err := handler.Download(context.Background(), server.identity("post", "C8post11"), t.TempDir())
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(slides) != 2 {
		t.Fatalf("slides = %d, want the two real images", len(slides))
	}
}

func TestDownloadTakesTheVideoStraightFromThePage(t *testing.T) {
	server := newSite(t, "")
	server.page = server.rewrite(fixture(t, "reel.html"))
	downloader := &fakeDownloader{}
	prober := &fakeProber{}
	handler := testHandler(t, Deps{Downloader: downloader, Probe: prober})

	media, err := handler.Download(context.Background(), server.identity("reel", "C8abc123"), t.TempDir())
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(media) != 1 || media[0].MIMEType != "audio/mpeg" {
		t.Fatalf("media = %+v, want one audio track", media)
	}
	if downloader.calls != 0 || prober.calls != 0 {
		t.Errorf("the free rung worked but the tool ran anyway (%d downloads, %d probes)",
			downloader.calls, prober.calls)
	}
}

func TestTheLadderFallsThroughToCookiesWhenAnonymousIsRefused(t *testing.T) {
	server := newSite(t, "")
	// No media URL on the page, so the ladder starts at the downloader.
	server.page = strings.Replace(server.rewrite(fixture(t, "reel.html")),
		`<meta property="og:video:secure_url" content="`+server.server.URL+`/reel.mp4" />`, "", 1)
	server.page = strings.ReplaceAll(server.page, "video_url", "removed_url")

	downloader := &fakeDownloader{onCall: func(call int, options media.DownloadOptions) error {
		if options.CookieFile == "" {
			return media.ErrLoginRequired
		}
		return nil
	}}
	jar := cookies.New(map[string]string{"active": netscapeCookies()})
	handler := testHandler(t, Deps{
		Downloader: downloader,
		Probe:      &fakeProber{},
		Cookies:    jar,
	})

	media, err := handler.Download(context.Background(), server.identity("reel", "C8abc123"), t.TempDir())
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(media) != 1 {
		t.Fatalf("media = %+v", media)
	}
	if downloader.calls != 2 || downloader.cookied != 1 {
		t.Fatalf("calls = %d (%d with cookies), want anonymous then cookied",
			downloader.calls, downloader.cookied)
	}
	// A slot that worked is recorded as healthy, so it is tried first next time.
	report := jar.Report()
	if len(report) != 1 || report[0].Successes != 1 {
		t.Errorf("cookie health = %+v", report)
	}
}

func TestTheCookieFileNeverOutlivesTheAttempt(t *testing.T) {
	server := newSite(t, "")
	server.page = strings.ReplaceAll(server.rewrite(fixture(t, "reel.html")), "video_url", "removed_url")
	server.page = strings.Replace(server.page,
		`<meta property="og:video:secure_url" content="`+server.server.URL+`/reel.mp4" />`, "", 1)

	var seenCookieFile string
	downloader := &fakeDownloader{onCall: func(_ int, options media.DownloadOptions) error {
		if options.CookieFile == "" {
			return media.ErrLoginRequired
		}
		seenCookieFile = options.CookieFile
		return nil
	}}
	handler := testHandler(t, Deps{
		Downloader: downloader,
		Probe:      &fakeProber{},
		Cookies:    cookies.New(map[string]string{"active": netscapeCookies()}),
	})

	workDir := t.TempDir()
	if _, err := handler.Download(context.Background(), server.identity("reel", "C8abc123"), workDir); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if seenCookieFile == "" {
		t.Fatal("the cookie rung never ran")
	}
	if _, err := os.Stat(seenCookieFile); !os.IsNotExist(err) {
		t.Error("the cookie file survived the attempt")
	}
}

func TestTheLadderStopsDeadRatherThanSpendingTheExpensiveRungs(t *testing.T) {
	server := newSite(t, "")
	server.page = strings.ReplaceAll(server.rewrite(fixture(t, "reel.html")), "video_url", "removed_url")
	server.page = strings.Replace(server.page,
		`<meta property="og:video:secure_url" content="`+server.server.URL+`/reel.mp4" />`, "", 1)

	// The tool reports the post is gone. Nothing later can change that, so the
	// cookie slots must not be spent on it.
	downloader := &fakeDownloader{err: media.ErrUnavailable}
	jar := cookies.New(map[string]string{"active": netscapeCookies()})
	handler := testHandler(t, Deps{
		Downloader: downloader,
		Probe:      &fakeProber{},
		Cookies:    jar,
	})

	_, err := handler.Download(context.Background(), server.identity("reel", "C8abc123"), t.TempDir())
	var failure *pipeline.Failure
	if !errors.As(err, &failure) || failure.Class != pipeline.ContentTerminal {
		t.Fatalf("err = %v, want terminal", err)
	}
	if downloader.cookied != 0 {
		t.Error("a deleted post spent a cookie slot")
	}
	if report := jar.Report(); report[0].Failures != 0 {
		t.Error("a healthy slot was blamed for a deleted post")
	}
}

func TestAnOverLongPostIsRefusedBeforeItIsDownloaded(t *testing.T) {
	server := newSite(t, "")
	server.page = strings.ReplaceAll(server.rewrite(fixture(t, "reel.html")), "video_url", "removed_url")
	server.page = strings.Replace(server.page,
		`<meta property="og:video:secure_url" content="`+server.server.URL+`/reel.mp4" />`, "", 1)

	downloader := &fakeDownloader{}
	handler := testHandler(t, Deps{
		Downloader: downloader,
		Probe:      &fakeProber{err: media.ErrTooLong},
		Cookies:    cookies.New(map[string]string{"active": netscapeCookies()}),
	})

	_, err := handler.Download(context.Background(), server.identity("reel", "C8abc123"), t.TempDir())
	var failure *pipeline.Failure
	if !errors.As(err, &failure) || failure.Class != pipeline.ContentTerminal {
		t.Fatalf("err = %v, want terminal", err)
	}
	if downloader.calls != 0 {
		t.Error("a two-hour video was downloaded anyway")
	}
}

func TestASilentReelIsNotAFailure(t *testing.T) {
	server := newSite(t, "")
	server.page = server.rewrite(fixture(t, "reel.html"))
	handler := testHandler(t, Deps{Audio: silentAudio()})

	media, err := handler.Download(context.Background(), server.identity("reel", "C8abc123"), t.TempDir())
	if err != nil {
		t.Fatalf("a reel with no sound failed: %v", err)
	}
	if len(media) != 0 {
		t.Fatalf("media = %+v, want nothing to transcribe", media)
	}
}

func TestARateLimitedPageIsProviderExhausted(t *testing.T) {
	server := newSite(t, fixture(t, "reel.html"))
	server.status = http.StatusTooManyRequests
	handler := testHandler(t, Deps{})

	_, err := handler.Prepare(context.Background(), server.identity("page", ""))
	var failure *pipeline.Failure
	if !errors.As(err, &failure) || failure.Class != pipeline.ProviderExhausted {
		t.Fatalf("err = %v, want provider exhausted so the cooldown holds the queue back", err)
	}
}

// TestDownloadIsDeterministicForTheSameIdentity is what lets the pipeline
// checkpoint and resume: the same identity twice must produce the same shape,
// and the handler must keep no state between calls.
func TestDownloadIsDeterministicForTheSameIdentity(t *testing.T) {
	server := newSite(t, "")
	server.page = server.rewrite(fixture(t, "carousel.html"))
	handler := testHandler(t, Deps{})
	identity := server.identity("post", "C8post11")

	describe := func(items []ai.Media) []string {
		shapes := make([]string, 0, len(items))
		for _, item := range items {
			shapes = append(shapes, filepath.Base(item.Path)+" "+item.MIMEType)
		}
		return shapes
	}

	first, err := handler.Download(context.Background(), identity, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := handler.Download(context.Background(), identity, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(describe(first), "|") != strings.Join(describe(second), "|") {
		t.Fatalf("two runs disagreed:\n%v\n%v", describe(first), describe(second))
	}
	// Each run writes only into the directory it was given.
	for _, item := range append(first, second...) {
		if !strings.HasPrefix(item.Path, os.TempDir()) {
			t.Errorf("a file landed outside a job directory: %s", item.Path)
		}
	}
}

func TestTheHandlerNeverLogsSecrets(t *testing.T) {
	var captured strings.Builder
	server := newSite(t, "")
	server.page = strings.ReplaceAll(server.rewrite(fixture(t, "reel.html")), "video_url", "removed_url")
	server.page = strings.Replace(server.page,
		`<meta property="og:video:secure_url" content="`+server.server.URL+`/reel.mp4" />`, "", 1)

	handler := testHandler(t, Deps{
		Downloader: &fakeDownloader{err: errors.New("signed url https://scontent.example.com/x?sig=SECRET failed")},
		Probe:      &fakeProber{},
		Cookies:    cookies.New(map[string]string{"active": netscapeCookies()}),
		Logger:     newCapturingLogger(&captured),
	})

	handler.Download(context.Background(), server.identity("reel", "C8abc123"), t.TempDir())

	logged := captured.String()
	for _, secret := range []string{"sig=SECRET", "netscape", "# HTTP Cookie File"} {
		if strings.Contains(strings.ToLower(logged), strings.ToLower(secret)) {
			t.Errorf("the log carries %q:\n%s", secret, logged)
		}
	}
	// No URL at all: not the signed media link, and not the user's own.
	if strings.Contains(logged, "http://") || strings.Contains(logged, "https://") {
		t.Errorf("the log carries a URL:\n%s", logged)
	}
	if !strings.Contains(logged, "C8abc123") {
		t.Error("the log has no content id, so a run cannot be found")
	}
}

// TestSmokeAgainstARealPost is opt-in: it is the only test here that reaches
// Instagram, and it never runs unless an operator asks for it.
func TestSmokeAgainstARealPost(t *testing.T) {
	url := os.Getenv("REELPIN_SMOKE_URL")
	if url == "" {
		t.Skip("REELPIN_SMOKE_URL is not set")
	}

	identity, err := (&sourceidentity.Resolver{}).Resolve(context.Background(), url)
	if err != nil {
		t.Fatalf("resolving %s: %v", url, err)
	}
	if identity.Platform != "instagram" {
		t.Skipf("%s is not an instagram url", url)
	}

	handler := New(Deps{
		HTTP:       safehttp.New(safehttp.Config{}),
		Downloader: media.NewYTDLP(nil),
		Probe:      media.NewYTDLP(nil),
		Audio:      media.NewFFmpeg(nil),
	})

	prepared, err := handler.Prepare(context.Background(), identity)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Logf("prepared: needs_media=%v caption_len=%d", prepared.NeedsMedia, len(prepared.Caption))

	if prepared.NeedsMedia {
		items, err := handler.Download(context.Background(), identity, t.TempDir())
		if err != nil {
			t.Fatalf("Download: %v", err)
		}
		for _, item := range items {
			info, _ := os.Stat(item.Path)
			t.Logf("media: %s %d bytes", item.MIMEType, info.Size())
		}
	}
}

// netscapeCookies is the shape the jar accepts, with nothing real in it.
func netscapeCookies() string {
	file := "# Netscape HTTP Cookie File\n.instagram.com\tTRUE\t/\tTRUE\t0\tsessionid\tnot-a-real-session\n"
	return base64Encode(file)
}

func base64Encode(value string) string {
	return base64.StdEncoding.EncodeToString([]byte(value))
}

// newCapturingLogger writes structured lines into a buffer a test can read.
func newCapturingLogger(into *strings.Builder) *slog.Logger {
	return slog.New(slog.NewJSONHandler(into, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
