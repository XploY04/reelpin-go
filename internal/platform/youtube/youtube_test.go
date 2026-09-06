package youtube

import (
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

	"github.com/XploY04/reelpin-go/internal/media"
	"github.com/XploY04/reelpin-go/internal/pipeline"
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

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

// fakeProber answers what a real probe would say, without spawning yt-dlp or
// resolving a host. The admission gate has its own tests in internal/media.
type fakeProber struct {
	calls    int
	duration int
	size     int64
	err      error
}

func (f *fakeProber) Probe(context.Context, string) (int, int64, error) {
	f.calls++
	return f.duration, f.size, f.err
}

// fakeDownloader writes a real MP4 header so SniffKind agrees with it.
type fakeDownloader struct {
	err  error
	junk bool
}

func (f *fakeDownloader) Download(_ context.Context, _ string, workDir string, _ media.DownloadOptions) (media.Download, error) {
	if f.err != nil {
		return media.Download{}, f.err
	}
	path := filepath.Join(workDir, "video.mp4")
	body := []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00")
	if f.junk {
		body = []byte("<!doctype html><html>not a video at all</html>")
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return media.Download{}, err
	}
	return media.Download{VideoPath: path, Title: "a video"}, nil
}

func identityFor(url string) sourceidentity.SourceIdentity {
	return sourceidentity.SourceIdentity{
		NormalizedURL: url,
		OriginalURL:   url,
		Platform:      PlatformName,
		ContentType:   "video",
		ContentID:     "abc123",
	}
}

func baseDeps(server *httptest.Server) Deps {
	return Deps{
		HTTP:   safehttp.New(safehttp.Config{AllowPrivateAddresses: true}),
		Limit:  providers.NewLimits(),
		Logger: quiet(),
	}
}

func TestAPublishedTranscriptSkipsTheMediaHalf(t *testing.T) {
	// The whole point of asking the actor first: no download, no audio
	// extraction, no transcription call.
	actor := &fakeActor{items: mustItems(t, []map[string]any{{
		"id":           "abc123",
		"title":        "Making a chair from one board",
		"description":  "Twelve minutes, one board.",
		"text":         "Unrelated recommendations from elsewhere on the page.",
		"thumbnailUrl": "https://i.ytimg.com/vi/abc123/maxres.jpg",
		"subtitles": []map[string]string{
			{"srt": "1\n00:00:01,000 --> 00:00:03,000\nStart with a flat face.\n"},
		},
	}})}

	page := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the watch page was fetched even though a transcript was published")
	})

	deps := baseDeps(page)
	deps.Apify = actor
	probe := &fakeProber{duration: 600, size: 1000000}
	deps.Prober = probe

	prepared, err := New(deps).Prepare(context.Background(), identityFor(page.URL))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.NeedsMedia {
		t.Fatal("a video with a published transcript still asked for media")
	}
	if !strings.Contains(prepared.PageText, "flat face") {
		t.Errorf("page text = %q, want the subtitle text", prepared.PageText)
	}
	if strings.Contains(prepared.PageText, "Unrelated recommendations") {
		t.Errorf("the actor's unrelated text field became the transcript: %q", prepared.PageText)
	}
	if probe.calls != 0 {
		t.Errorf("the prober ran %d times on the cheap path", probe.calls)
	}
}

func TestAnActorResultForAnotherVideoIsIgnored(t *testing.T) {
	actor := &fakeActor{items: mustItems(t, []map[string]any{{
		"id":    "different-video",
		"title": "Someone else's video",
		"subtitles": []map[string]string{
			{"text": "Words from the wrong video."},
		},
	}})}
	deps := baseDeps(nil)
	deps.Apify = actor

	if prepared, ok := New(deps).prepareFromSubtitles(
		context.Background(), identityFor("https://youtu.be/abc123")); ok {
		t.Fatalf("wrong-video actor result was accepted: %+v", prepared)
	}
}

func TestNoTranscriptMeansMediaWork(t *testing.T) {
	page := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(fixture(t, "watch.html")))
	})

	deps := baseDeps(page)
	probe := &fakeProber{duration: 600, size: 1000000}
	deps.Prober = probe

	prepared, err := New(deps).Prepare(context.Background(), identityFor(page.URL))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !prepared.NeedsMedia {
		t.Fatal("a video with no transcript did not ask for media")
	}
	if !strings.Contains(prepared.Caption, "Making a chair") {
		t.Errorf("caption = %q", prepared.Caption)
	}
	if prepared.ThumbnailURL == "" {
		t.Error("no thumbnail from a page that publishes og:image")
	}
	if probe.calls != 1 {
		t.Errorf("prober ran %d times, want one probe before committing to media", probe.calls)
	}
}

func TestAnOverLongVideoIsRefusedBeforeDownloading(t *testing.T) {
	page := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(fixture(t, "watch.html")))
	})

	deps := baseDeps(page)
	// Probe reports a four-hour video: the cap refuses it for the price of the
	// probe, never a download.
	deps.Prober = &fakeProber{err: media.ErrTooLong}

	_, err := New(deps).Prepare(context.Background(), identityFor(page.URL))
	assertFailure(t, err, "media_too_long", false)
}

func TestAThrottledSourceIsRetryable(t *testing.T) {
	page := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	deps := baseDeps(page)
	deps.Prober = &fakeProber{duration: 60, size: 1000}

	// A throttled page is soft: the download path is still open, so Prepare
	// carries on and still asks for media.
	prepared, err := New(deps).Prepare(context.Background(), identityFor(page.URL))
	if err != nil {
		t.Fatalf("a throttled page ended the run: %v", err)
	}
	if !prepared.NeedsMedia {
		t.Error("a throttled page should not stop the media path")
	}
}

func TestARemovedVideoIsTerminal(t *testing.T) {
	page := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	_, err := New(baseDeps(page)).Prepare(context.Background(), identityFor(page.URL))
	assertFailure(t, err, "content_unavailable", false)
}

func TestDownloadProducesAudioForTranscription(t *testing.T) {
	deps := baseDeps(nil)
	deps.Downloader = &fakeDownloader{}
	deps.Audio = media.NewFFmpeg(&fakeAudioRunner{dir: t.TempDir()})

	workDir := t.TempDir()
	audio, err := New(deps).Download(context.Background(), identityFor("https://youtu.be/abc123"), workDir)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(audio) != 1 || !strings.HasPrefix(audio[0].MIMEType, "audio/") {
		t.Fatalf("media = %+v, want one audio file", audio)
	}
}

func TestADownloadThatIsNotAVideoIsTerminal(t *testing.T) {
	// A login wall served as HTML with a .mp4 name is the classic case: the
	// bytes decide, not the extension.
	deps := baseDeps(nil)
	deps.Downloader = &fakeDownloader{junk: true}
	deps.Audio = media.NewFFmpeg(&fakeAudioRunner{dir: t.TempDir()})

	_, err := New(deps).Download(context.Background(), identityFor("https://youtu.be/abc123"), t.TempDir())
	assertFailure(t, err, "media_unreadable", false)
}

func TestADownloadRefusalIsClassified(t *testing.T) {
	deps := baseDeps(nil)
	deps.Downloader = &fakeDownloader{err: media.ErrLoginRequired}

	_, err := New(deps).Download(context.Background(), identityFor("https://youtu.be/abc123"), t.TempDir())
	assertFailure(t, err, "login_required", false)
}

func TestStripSRTKeepsOnlyTheWords(t *testing.T) {
	srt := "1\n00:00:01,000 --> 00:00:03,000\nStart with a flat face.\n\n2\n00:00:03,000 --> 00:00:06,000\nThen joint the edge.\n"
	got := stripSRT(srt)
	if strings.Contains(got, "-->") || strings.Contains(got, "00:00") {
		t.Fatalf("timings survived: %q", got)
	}
	if !strings.Contains(got, "Start with a flat face.") || !strings.Contains(got, "joint the edge") {
		t.Fatalf("words lost: %q", got)
	}
}

// fakeAudioRunner stands in for ffmpeg by writing the file it claims to make.
type fakeAudioRunner struct{ dir string }

func (f *fakeAudioRunner) Run(_ context.Context, command media.Command) (media.Result, error) {
	for index, arg := range command.Args {
		if strings.HasSuffix(arg, ".mp3") || (index == len(command.Args)-1 && strings.Contains(arg, "audio")) {
			os.WriteFile(arg, []byte("ID3fakeaudio"), 0o600)
		}
	}
	return media.Result{}, nil
}

// fakeActor stands in for the Apify client. The handler declares only the two
// methods it uses, so the actor path is tested with no network at all.
type fakeActor struct {
	items []json.RawMessage
	err   error
	calls int
}

func (f *fakeActor) Configured(string) bool { return true }

func (f *fakeActor) Run(context.Context, string, any) ([]json.RawMessage, error) {
	f.calls++
	return f.items, f.err
}

func mustItems(t *testing.T, rows []map[string]any) []json.RawMessage {
	t.Helper()
	items := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, encoded)
	}
	return items
}

func assertFailure(t *testing.T, err error, wantCode string, retryable bool) {
	t.Helper()
	var failure *pipeline.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want a classified failure", err)
	}
	if failure.Code != wantCode {
		t.Errorf("code = %q, want %q", failure.Code, wantCode)
	}
	if failure.Retryable() != retryable {
		t.Errorf("retryable = %v, want %v", failure.Retryable(), retryable)
	}
}
