package tiktok

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

func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func baseDeps() Deps {
	return Deps{
		HTTP:   safehttp.New(safehttp.Config{AllowPrivateAddresses: true}),
		Limit:  providers.NewLimits(),
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

func identityFor(url string) sourceidentity.SourceIdentity {
	return sourceidentity.SourceIdentity{
		NormalizedURL: url,
		OriginalURL:   url,
		Platform:      PlatformName,
		ContentType:   "video",
		ContentID:     "7123456789",
	}
}

type fakeProber struct {
	duration int
	size     int64
	err      error
}

func (f *fakeProber) Probe(context.Context, string) (int, int64, error) {
	return f.duration, f.size, f.err
}

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
		body = []byte("<!doctype html><html>a login wall</html>")
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return media.Download{}, err
	}
	return media.Download{VideoPath: path}, nil
}

type fakeAudioRunner struct{}

func (fakeAudioRunner) Run(_ context.Context, command media.Command) (media.Result, error) {
	for _, arg := range command.Args {
		if strings.HasSuffix(arg, ".mp3") {
			os.WriteFile(arg, []byte("ID3fakeaudio"), 0o600)
		}
	}
	return media.Result{}, nil
}

func TestAPostWithAudioIsMediaWork(t *testing.T) {
	page := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(fixture(t, "video.html")))
	})

	deps := baseDeps()
	deps.Prober = &fakeProber{duration: 42, size: 4 << 20}

	prepared, err := New(deps).Prepare(context.Background(), identityFor(page.URL))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !prepared.NeedsMedia {
		t.Fatal("a TikTok video did not ask for media; there is no other source of words")
	}
	if !strings.Contains(prepared.Caption, "omelette") {
		t.Errorf("caption = %q", prepared.Caption)
	}
}

func TestAPostWithNoDurationStaysLight(t *testing.T) {
	// A still or a broken post has nothing to transcribe. Its caption is still
	// worth saving, so it becomes light work rather than a failed download.
	page := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(fixture(t, "video.html")))
	})

	deps := baseDeps()
	deps.Prober = &fakeProber{duration: 0}

	prepared, err := New(deps).Prepare(context.Background(), identityFor(page.URL))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.NeedsMedia {
		t.Fatal("a post with no duration asked for a download")
	}
	if prepared.PageText == "" {
		t.Error("light work with no text to extract from")
	}
}

func TestALoginWallDoesNotEndTheRun(t *testing.T) {
	// TikTok walls plenty of clients while the video is still downloadable.
	page := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fixture(t, "wall.html")))
	})

	deps := baseDeps()
	deps.Prober = &fakeProber{duration: 30, size: 1 << 20}

	prepared, err := New(deps).Prepare(context.Background(), identityFor(page.URL))
	if err != nil {
		t.Fatalf("a wall page ended the run: %v", err)
	}
	if !prepared.NeedsMedia {
		t.Error("the download path was closed by a page wall")
	}
}

func TestARemovedPostIsTerminal(t *testing.T) {
	page := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	_, err := New(baseDeps()).Prepare(context.Background(), identityFor(page.URL))
	assertFailure(t, err, "content_unavailable", false)
}

func TestAThrottledPageIsRetryableWhenTheProbeAlsoFails(t *testing.T) {
	page := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	deps := baseDeps()
	deps.Prober = &fakeProber{err: media.ErrRateLimited}

	_, err := New(deps).Prepare(context.Background(), identityFor(page.URL))
	assertFailure(t, err, "provider_rate_limited", true)
}

func TestAnOversizedPostIsRefusedBeforeDownloading(t *testing.T) {
	page := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(fixture(t, "video.html")))
	})

	deps := baseDeps()
	deps.Prober = &fakeProber{err: media.ErrTooLarge}

	_, err := New(deps).Prepare(context.Background(), identityFor(page.URL))
	assertFailure(t, err, "media_too_large", false)
}

func TestDownloadProducesAudio(t *testing.T) {
	deps := baseDeps()
	deps.Downloader = &fakeDownloader{}
	deps.Audio = media.NewFFmpeg(fakeAudioRunner{})

	audio, err := New(deps).Download(context.Background(),
		identityFor("https://www.tiktok.com/@a/video/7123456789"), t.TempDir())
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(audio) != 1 || !strings.HasPrefix(audio[0].MIMEType, "audio/") {
		t.Fatalf("media = %+v", audio)
	}
}

func TestADownloadThatIsNotAVideoIsTerminal(t *testing.T) {
	deps := baseDeps()
	deps.Downloader = &fakeDownloader{junk: true}
	deps.Audio = media.NewFFmpeg(fakeAudioRunner{})

	_, err := New(deps).Download(context.Background(),
		identityFor("https://www.tiktok.com/@a/video/7123456789"), t.TempDir())
	assertFailure(t, err, "media_unreadable", false)
}

func TestAPrivatePostIsTerminal(t *testing.T) {
	deps := baseDeps()
	deps.Downloader = &fakeDownloader{err: media.ErrPrivate}

	_, err := New(deps).Download(context.Background(),
		identityFor("https://www.tiktok.com/@a/video/7123456789"), t.TempDir())
	assertFailure(t, err, "content_private", false)
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
