package web

import (
	"context"
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

func TestParseMetadataPrefersOpenGraphThenTheTitleTag(t *testing.T) {
	metadata := ParseMetadata(fixture(t, "article.html"))
	if metadata.Title != "Best cafes in Goa" {
		t.Errorf("title = %q", metadata.Title)
	}
	if !strings.Contains(metadata.Description, "worth the ride") {
		t.Errorf("description = %q", metadata.Description)
	}
	if metadata.SiteName != "A Blog" {
		t.Errorf("site name = %q", metadata.SiteName)
	}
	if !metadata.Usable() {
		t.Error("a page with a title and description is usable")
	}

	// With no Open Graph at all, the plain title still carries the page.
	plain := ParseMetadata(`<html><head><title>Just a &amp; title</title></head></html>`)
	if plain.Title != "Just a & title" {
		t.Errorf("plain title = %q", plain.Title)
	}
}

func TestEmptyPageIsNotUsable(t *testing.T) {
	if ParseMetadata(fixture(t, "empty.html")).Usable() {
		t.Fatal("a page with nothing published was called usable")
	}
}

func TestStripSRTDropsTimingsAndIndices(t *testing.T) {
	subtitle := "1\n00:00:01,000 --> 00:00:03,000\nWelcome to the cafe\n\n2\n00:00:03,000 --> 00:00:05,000\nIt opens at eight\n"
	got := stripSRT(subtitle)
	if strings.Contains(got, "-->") || strings.Contains(got, "00:00") {
		t.Fatalf("timings survived: %q", got)
	}
	if got != "Welcome to the cafe It opens at eight" {
		t.Fatalf("text = %q", got)
	}
}

type fakeDownloader struct {
	err      error
	attempts int
}

func (f *fakeDownloader) Download(_ context.Context, _ string, workDir string, _ media.DownloadOptions) (media.Download, error) {
	f.attempts++
	if f.err != nil {
		return media.Download{}, f.err
	}
	path := filepath.Join(workDir, "source.mp4")
	if err := os.WriteFile(path, []byte("video"), 0o600); err != nil {
		return media.Download{}, err
	}
	return media.Download{VideoPath: path}, nil
}

type fakeUploader struct{ keys []string }

func (f *fakeUploader) Upload(_ context.Context, key string, body io.Reader, _ string) (string, error) {
	io.Copy(io.Discard, body)
	f.keys = append(f.keys, key)
	return "https://storage.example.com/" + key, nil
}

type fakeRunner struct{}

func (fakeRunner) Run(_ context.Context, command media.Command) (media.Result, error) {
	// Stand in for ffmpeg by writing the output file it was asked for.
	return media.Result{}, os.WriteFile(command.Args[len(command.Args)-1], []byte("audio"), 0o600)
}

type harness struct {
	handler    *Handler
	downloader *fakeDownloader
	uploader   *fakeUploader
	server     *httptest.Server
	actorBody  string
	actorCalls int
}

func newHarness(t *testing.T, page string) *harness {
	t.Helper()
	h := &harness{downloader: &fakeDownloader{}, uploader: &fakeUploader{}}

	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/acts/"):
			h.actorCalls++
			body := h.actorBody
			if body == "" {
				body = "[]"
			}
			w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, ".jpg"), strings.HasSuffix(r.URL.Path, ".mp4"):
			w.Write([]byte("bytes"))
		default:
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(strings.ReplaceAll(page, "https://cdn.example.com", h.server.URL)))
		}
	}))
	t.Cleanup(h.server.Close)

	h.handler = New(Deps{
		HTTP:       safehttp.New(safehttp.Config{AllowPrivateAddresses: true}),
		Downloader: h.downloader,
		Audio:      media.NewFFmpeg(fakeRunner{}),
		Apify:      apify.New(apify.Config{}),
		Storage:    h.uploader,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	return h
}

func identityFor(server *httptest.Server, platformName, contentType, contentID string) sourceidentity.SourceIdentity {
	return sourceidentity.SourceIdentity{
		NormalizedURL: server.URL + "/page",
		Platform:      platformName,
		ContentType:   contentType,
		ContentID:     contentID,
	}
}

func TestYouTubeDownloadsAndTranscribes(t *testing.T) {
	h := newHarness(t, fixture(t, "video_page.html"))

	prepared, err := h.handler.Prepare(context.Background(),
		identityFor(h.server, "youtube", "short", "abc123XYZ09"), t.TempDir())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.VideoPath == "" || prepared.AudioPath == "" {
		t.Fatalf("prepared = %+v", prepared)
	}
	if prepared.TranscriptSource != "gemini_audio" {
		t.Errorf("transcript source = %q", prepared.TranscriptSource)
	}
	if prepared.ThumbnailURL == "" {
		t.Error("no thumbnail was stored")
	}
}

func TestYouTubeSubtitlesSkipTheDownloadEntirely(t *testing.T) {
	h := newHarness(t, fixture(t, "video_page.html"))
	h.actorBody = `[{"title":"A talk","description":"about cafes","subtitles":[{"text":"the cafe opens at eight"}]}]`

	// Point the client at the local server and configure the actor.
	h.handler.deps.Apify = apifyForTest(h.server.URL)

	prepared, err := h.handler.Prepare(context.Background(),
		identityFor(h.server, "youtube", "video", "abc123XYZ09"), t.TempDir())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if h.downloader.attempts != 0 {
		t.Errorf("the downloader ran %d times even though subtitles were available", h.downloader.attempts)
	}
	if prepared.Transcript != "the cafe opens at eight" {
		t.Errorf("transcript = %q", prepared.Transcript)
	}
	if prepared.TranscriptSource != "platform_subtitles" {
		t.Errorf("transcript source = %q", prepared.TranscriptSource)
	}
}

func TestAFailedActorStillDownloads(t *testing.T) {
	h := newHarness(t, fixture(t, "video_page.html"))
	h.actorBody = `[]`
	h.handler.deps.Apify = apifyForTest(h.server.URL)

	prepared, err := h.handler.Prepare(context.Background(),
		identityFor(h.server, "youtube", "video", "abc"), t.TempDir())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if h.downloader.attempts != 1 {
		t.Errorf("download attempts = %d, want the fallback used", h.downloader.attempts)
	}
	if prepared.VideoPath == "" {
		t.Error("no video was produced")
	}
}

func TestAVideoRefusalFallsBackToMetadata(t *testing.T) {
	h := newHarness(t, fixture(t, "article.html"))
	h.downloader.err = errors.New("no video could be found in this page")

	prepared, err := h.handler.Prepare(context.Background(),
		identityFor(h.server, "pinterest", "pin", "1234567890"), t.TempDir())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.VideoPath != "" {
		t.Error("a video appeared from a failed download")
	}
	if !strings.Contains(prepared.Caption, "worth the ride") {
		t.Errorf("caption = %q, want the page description", prepared.Caption)
	}
	if len(prepared.ImagePaths) != 1 {
		t.Errorf("images = %v, want the page image read for text", prepared.ImagePaths)
	}
	if prepared.TranscriptSource != "page_metadata" {
		t.Errorf("transcript source = %q", prepared.TranscriptSource)
	}
}

func TestADeletedVideoIsTerminal(t *testing.T) {
	h := newHarness(t, fixture(t, "article.html"))
	h.downloader.err = media.ErrUnavailable

	_, err := h.handler.Prepare(context.Background(),
		identityFor(h.server, "tiktok", "video", "123"), t.TempDir())
	if !errors.Is(err, media.ErrUnavailable) {
		t.Fatalf("err = %v, want the unavailable video reported rather than saved as a page", err)
	}
}

func TestPlaceLinksSkipTheVideoAttempt(t *testing.T) {
	h := newHarness(t, fixture(t, "place.html"))

	prepared, err := h.handler.Prepare(context.Background(),
		identityFor(h.server, "zomato", "link", "abc"), t.TempDir())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if h.downloader.attempts != 0 {
		t.Errorf("a place link ran the video downloader %d times", h.downloader.attempts)
	}
	if prepared.IngestionMethod != "place_metadata" {
		t.Errorf("ingestion method = %q", prepared.IngestionMethod)
	}
	if !strings.Contains(prepared.Caption, "Anjuna") {
		t.Errorf("caption = %q, want the place description", prepared.Caption)
	}
}

func TestGenericPageWithNothingPublishedIsTerminal(t *testing.T) {
	h := newHarness(t, fixture(t, "empty.html"))
	h.downloader.err = errors.New("no video")

	_, err := h.handler.Prepare(context.Background(),
		identityFor(h.server, "someblog.com", "link", ""), t.TempDir())
	if !errors.Is(err, ErrNothingToSave) {
		t.Fatalf("err = %v, want ErrNothingToSave", err)
	}
}

func TestMatchLeavesTheDedicatedPlatformsAlone(t *testing.T) {
	handler := New(Deps{})
	for _, platformName := range []string{"instagram", "x", "reddit"} {
		if handler.Match(sourceidentity.SourceIdentity{Platform: platformName}) {
			t.Errorf("the web handler claimed %s", platformName)
		}
	}
	// A LinkedIn post has its own reader; every other LinkedIn page publishes
	// ordinary metadata and belongs here.
	if handler.Match(sourceidentity.SourceIdentity{Platform: "linkedin", ContentType: "post"}) {
		t.Error("the web handler claimed a linkedin post")
	}
	if !handler.Match(sourceidentity.SourceIdentity{Platform: "linkedin", ContentType: "profile"}) {
		t.Error("the web handler did not claim a linkedin profile")
	}
	for _, platformName := range []string{"youtube", "tiktok", "pinterest", "zomato", "someblog.com"} {
		if !handler.Match(sourceidentity.SourceIdentity{Platform: platformName}) {
			t.Errorf("the web handler did not claim %s", platformName)
		}
	}

	place := handler.Capabilities(sourceidentity.SourceIdentity{Platform: "airbnb"})
	if !place.Place || place.Video {
		t.Errorf("place capabilities = %+v, want no video probing", place)
	}
}

// apifyForTest points the client at a local server with an actor configured.
func apifyForTest(serverURL string) *apify.Client {
	apify.SetBaseURLForTest(serverURL + "/v2")
	return apify.New(apify.Config{Token: "token", Actors: map[string]string{"youtube": "apify/youtube"}})
}
