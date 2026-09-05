package instagram

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/apify"
	"github.com/XploY04/reelpin-go/internal/cookies"
	"github.com/XploY04/reelpin-go/internal/media"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(body)
}

func TestParsePageReadsAReel(t *testing.T) {
	page := parsePage(fixture(t, "reel.html"))

	if page.Title != "Best cafes in Goa" {
		t.Errorf("title = %q", page.Title)
	}
	if page.Caption != "Three cafes worth the ride & a sunset spot" {
		t.Errorf("caption = %q, want the entities decoded", page.Caption)
	}
	if page.VideoURL != "https://scontent.example.com/reel.mp4" {
		t.Errorf("video url = %q, want the Open Graph one", page.VideoURL)
	}
	if page.ThumbnailURL != "https://scontent.example.com/thumb.jpg" {
		t.Errorf("thumbnail = %q", page.ThumbnailURL)
	}
}

func TestParsePageReadsACarouselInOrder(t *testing.T) {
	page := parsePage(fixture(t, "carousel.html"))

	if page.VideoURL != "" {
		t.Errorf("video url = %q, want none on an image post", page.VideoURL)
	}
	if len(page.ImageURLs) != 3 {
		t.Fatalf("images = %v, want three slides", page.ImageURLs)
	}
	for i, want := range []string{
		"https://scontent.example.com/slide-1.jpg",
		"https://scontent.example.com/slide-2.jpg",
		"https://scontent.example.com/slide-3.jpg",
	} {
		if page.ImageURLs[i] != want {
			t.Errorf("slide %d = %q, want %q", i, page.ImageURLs[i], want)
		}
	}
}

func TestParsePageOfALoginWallHasNoMedia(t *testing.T) {
	page := parsePage(fixture(t, "login_wall.html"))
	if page.VideoURL != "" || len(page.ImageURLs) != 0 {
		t.Fatalf("a login wall produced media: %+v", page)
	}
}

// fakeDownloader records what the ladder asked for.
type fakeDownloader struct {
	attempts []media.DownloadOptions
	results  []error
	index    int
}

func (f *fakeDownloader) Download(_ context.Context, _ string, workDir string, options media.DownloadOptions) (media.Download, error) {
	f.attempts = append(f.attempts, options)
	err := error(nil)
	if f.index < len(f.results) {
		err = f.results[f.index]
	}
	f.index++
	if err != nil {
		return media.Download{}, err
	}
	path := filepath.Join(workDir, "source.mp4")
	if writeErr := os.WriteFile(path, []byte("video"), 0o600); writeErr != nil {
		return media.Download{}, writeErr
	}
	return media.Download{VideoPath: path, Anonymous: options.CookieFile == ""}, nil
}

type fakeUploader struct {
	keys []string
	err  error
}

func (f *fakeUploader) Upload(_ context.Context, key string, body io.Reader, _ string) (string, error) {
	io.Copy(io.Discard, body)
	if f.err != nil {
		return "", f.err
	}
	f.keys = append(f.keys, key)
	return "https://storage.example.com/" + key, nil
}

type harness struct {
	handler    *Handler
	downloader *fakeDownloader
	uploader   *fakeUploader
	jar        *cookies.Jar
	server     *httptest.Server
	audioRuns  int
}

// newHarness serves the fixture pages and media from a local server, so no test
// reaches Instagram.
func newHarness(t *testing.T, pageBody string) *harness {
	t.Helper()

	h := &harness{
		downloader: &fakeDownloader{},
		uploader:   &fakeUploader{},
		jar: cookies.New(map[string]string{
			"active": base64.StdEncoding.EncodeToString([]byte(
				"# Netscape HTTP Cookie File\n.instagram.com\tTRUE\t/\tTRUE\t0\tsessionid\tvalue\n")),
		}),
	}

	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".mp4"):
			w.Write([]byte("video-bytes"))
		case strings.HasSuffix(r.URL.Path, ".jpg"):
			w.Write([]byte("image-bytes"))
		default:
			w.Header().Set("Content-Type", "text/html")
			// The fixture carries the CDN host both plainly and JSON-escaped.
			body := strings.NewReplacer(
				"https://scontent.example.com", h.server.URL,
				`https:\/\/scontent.example.com`, strings.ReplaceAll(h.server.URL, "/", `\/`),
			).Replace(pageBody)
			w.Write([]byte(body))
		}
	}))
	t.Cleanup(h.server.Close)

	audio := media.NewFFmpeg(&fakeRunner{onRun: func(command media.Command) (media.Result, error) {
		h.audioRuns++
		// Stand in for ffmpeg writing its output.
		for i, arg := range command.Args {
			if i == len(command.Args)-1 {
				return media.Result{}, os.WriteFile(arg, []byte("audio"), 0o600)
			}
		}
		return media.Result{}, nil
	}})

	h.handler = New(Deps{
		// Loopback is only reachable because this is a test.
		HTTP:       safehttp.New(safehttp.Config{AllowPrivateAddresses: true}),
		Downloader: h.downloader,
		Audio:      audio,
		Apify:      apify.New(apify.Config{}),
		Cookies:    h.jar,
		Storage:    h.uploader,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	return h
}

type fakeRunner struct {
	onRun func(media.Command) (media.Result, error)
}

func (f *fakeRunner) Run(_ context.Context, command media.Command) (media.Result, error) {
	return f.onRun(command)
}

func reelIdentity(server *httptest.Server) sourceidentity.SourceIdentity {
	return sourceidentity.SourceIdentity{
		NormalizedURL: server.URL + "/reel/C8abc123/",
		Platform:      "instagram",
		ContentType:   "reel",
		ContentID:     "C8abc123",
	}
}

func TestPublicPageMediaIsPreferredOverEveryTool(t *testing.T) {
	h := newHarness(t, fixture(t, "reel.html"))

	prepared, err := h.handler.Prepare(context.Background(), reelIdentity(h.server), t.TempDir())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if len(h.downloader.attempts) != 0 {
		t.Errorf("the downloader ran %d times even though the page carried the video", len(h.downloader.attempts))
	}
	if prepared.VideoPath == "" || prepared.AudioPath == "" {
		t.Fatalf("prepared = %+v", prepared)
	}
	if prepared.Caption == "" {
		t.Error("the caption from the page was dropped")
	}
	if prepared.ThumbnailURL == "" || len(h.uploader.keys) != 1 {
		t.Errorf("thumbnail = %q keys = %v", prepared.ThumbnailURL, h.uploader.keys)
	}
	// The key is derived from the identity, so re-processing overwrites.
	if !strings.Contains(h.uploader.keys[0], "C8abc123") {
		t.Errorf("thumbnail key = %q, want it derived from the content id", h.uploader.keys[0])
	}
}

func TestLadderFallsBackToTheDownloaderThenCookies(t *testing.T) {
	h := newHarness(t, fixture(t, "login_wall.html"))
	// Anonymous is refused; the cookie attempt succeeds.
	h.downloader.results = []error{media.ErrLoginRequired, nil}

	prepared, err := h.handler.Prepare(context.Background(), reelIdentity(h.server), t.TempDir())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.VideoPath == "" {
		t.Fatal("no video was produced")
	}

	if len(h.downloader.attempts) != 2 {
		t.Fatalf("the ladder made %d attempts, want anonymous then cookies", len(h.downloader.attempts))
	}
	if h.downloader.attempts[0].CookieFile != "" {
		t.Error("the first attempt used credentials")
	}
	if h.downloader.attempts[1].CookieFile == "" {
		t.Error("the second attempt did not use a cookie slot")
	}

	// The cookie file never outlives the attempt.
	if _, err := os.Stat(h.downloader.attempts[1].CookieFile); !os.IsNotExist(err) {
		t.Error("the cookie file was left on disk")
	}

	report := h.jar.Report()
	if len(report) != 1 || report[0].Successes != 1 {
		t.Errorf("cookie health = %+v, want the slot credited", report)
	}
}

func TestARetiredSlotIsNotTriedAgain(t *testing.T) {
	h := newHarness(t, fixture(t, "login_wall.html"))
	h.downloader.results = []error{
		media.ErrLoginRequired, media.ErrLoginRequired,
	}

	_, err := h.handler.Prepare(context.Background(), reelIdentity(h.server), t.TempDir())
	if err == nil {
		t.Fatal("Prepare succeeded with every path refused")
	}

	for i := 0; i < 2; i++ {
		h.jar.RecordFailure(0)
	}
	if !h.jar.AllExhausted() {
		t.Fatal("a repeatedly refused slot was not retired")
	}
	if len(h.jar.Slots()) != 0 {
		t.Fatal("a retired slot is still offered")
	}
}

func TestADeletedPostStopsTheLadderImmediately(t *testing.T) {
	h := newHarness(t, fixture(t, "login_wall.html"))
	h.downloader.results = []error{media.ErrUnavailable}

	_, err := h.handler.Prepare(context.Background(), reelIdentity(h.server), t.TempDir())
	if !errors.Is(err, media.ErrUnavailable) {
		t.Fatalf("err = %v, want the unavailable post reported", err)
	}
	if len(h.downloader.attempts) != 1 {
		t.Errorf("the ladder made %d attempts on a deleted post, want 1", len(h.downloader.attempts))
	}
}

func TestCarouselDownloadsSlidesAndUsesTheFirstAsTheThumbnail(t *testing.T) {
	h := newHarness(t, fixture(t, "carousel.html"))
	identity := sourceidentity.SourceIdentity{
		NormalizedURL: h.server.URL + "/p/C8xyz999/",
		Platform:      "instagram",
		ContentType:   "post",
		ContentID:     "C8xyz999",
	}

	prepared, err := h.handler.Prepare(context.Background(), identity, t.TempDir())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if len(prepared.ImagePaths) != 3 {
		t.Fatalf("slides = %v", prepared.ImagePaths)
	}
	if prepared.TranscriptSource != "gemini_vision_ocr" {
		t.Errorf("transcript source = %q, want the image path", prepared.TranscriptSource)
	}
	if prepared.ThumbnailPath != prepared.ImagePaths[0] {
		t.Error("the thumbnail is not the first slide, so it would change when a later slide does")
	}
	if len(h.downloader.attempts) != 0 {
		t.Error("an image post ran the video downloader")
	}
}

func TestCarouselIsBounded(t *testing.T) {
	var slides strings.Builder
	slides.WriteString(`<html><head><meta property="og:description" content="many" /></head><body><script>{`)
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&slides, `"display_url":"https:\/\/scontent.example.com\/slide-%d.jpg",`, i)
	}
	slides.WriteString(`}</script></body></html>`)

	h := newHarness(t, slides.String())
	identity := sourceidentity.SourceIdentity{
		NormalizedURL: h.server.URL + "/p/many/", Platform: "instagram", ContentType: "post", ContentID: "many",
	}

	prepared, err := h.handler.Prepare(context.Background(), identity, t.TempDir())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(prepared.ImagePaths) > MaxImages {
		t.Fatalf("downloaded %d slides, want at most %d", len(prepared.ImagePaths), MaxImages)
	}
}

func TestAFailedThumbnailUploadDoesNotFailTheRun(t *testing.T) {
	h := newHarness(t, fixture(t, "reel.html"))
	h.uploader.err = errors.New("storage is down")

	prepared, err := h.handler.Prepare(context.Background(), reelIdentity(h.server), t.TempDir())
	if err != nil {
		t.Fatalf("a storage outage failed the run: %v", err)
	}
	if prepared.ThumbnailURL != "" {
		t.Errorf("thumbnail url = %q, want it left empty", prepared.ThumbnailURL)
	}
	if prepared.VideoPath == "" {
		t.Error("the video was lost with the thumbnail")
	}
}

func TestMatchAndCapabilities(t *testing.T) {
	handler := New(Deps{})
	if !handler.Match(sourceidentity.SourceIdentity{Platform: "instagram"}) {
		t.Error("the handler does not claim instagram")
	}
	if handler.Match(sourceidentity.SourceIdentity{Platform: "youtube"}) {
		t.Error("the handler claimed another platform")
	}

	reel := handler.Capabilities(sourceidentity.SourceIdentity{ContentType: "reel"})
	if !reel.Audio || reel.Images {
		t.Errorf("reel capabilities = %+v", reel)
	}
	post := handler.Capabilities(sourceidentity.SourceIdentity{ContentType: "post"})
	if !post.Images || !post.Audio {
		t.Errorf("post capabilities = %+v, want both paths open until the page says which", post)
	}
}
