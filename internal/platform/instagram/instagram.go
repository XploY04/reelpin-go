// Package instagram ingests Instagram reels, posts, carousels and profile
// pages.
//
// The fallback ladder matters more than any single rung: a public page fetch
// costs nothing, an anonymous download costs little, the configured actor costs
// money, and a cookie slot spends a real account's standing. They are tried in
// that order and the ladder stops at the first rung that works. A failure that
// can never succeed — a removed post, a private account — stops the ladder
// immediately rather than spending the expensive rungs on it.
package instagram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/apify"
	"github.com/XploY04/reelpin-go/internal/cookies"
	"github.com/XploY04/reelpin-go/internal/media"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// MaxSlides bounds a carousel. Reading twenty slides costs twenty times as
// much and says very little more than the first eight.
const MaxSlides = 8

// actorName is the Apify actor key and the provider semaphore's name. One
// account, one concurrent run.
const actorName = "instagram"

type Deps struct {
	HTTP       *safehttp.Client
	Downloader media.Downloader
	// Probe reads duration and size before a download, so an over-long post
	// costs a metadata call rather than a transfer. *media.YTDLP satisfies it.
	Probe      Prober
	Audio      *media.FFmpeg
	Apify      *apify.Client
	Cookies    *cookies.Jar
	Thumbnails platform.Thumbnails
	Limits     *providers.Limits
	Logger     *slog.Logger
}

// Prober is the metadata half of the downloader, named separately so a test
// can supply one without a binary.
type Prober interface {
	Probe(ctx context.Context, rawURL string) (durationSeconds int, approxBytes int64, err error)
}

type Handler struct {
	deps Deps
}

func New(deps Deps) *Handler {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deps.Limits == nil {
		deps.Limits = providers.NewLimits()
	}
	return &Handler{deps: deps}
}

var _ platform.Handler = (*Handler)(nil)

func (h *Handler) Platform() string { return "instagram" }

// log never carries a raw URL, a cookie or a signed media link. The content id
// is enough to find the run, and it is already public in the URL the user
// shared.
func (h *Handler) log() *slog.Logger { return h.deps.Logger.With("platform", "instagram") }

func (h *Handler) Prepare(ctx context.Context, identity sourceidentity.SourceIdentity) (platform.Prepared, error) {
	page, err := h.fetchPage(ctx, identity.NormalizedURL)
	if err != nil {
		// A page that refuses is not the end for a reel or a post: the
		// downloader and the actor have their own access. It is the end for a
		// profile page, which has nothing else to read.
		if identity.ContentType == "page" {
			return platform.Prepared{}, classify(err)
		}
		if terminal := terminalNow(err); terminal != nil {
			return platform.Prepared{}, classify(terminal)
		}
		h.log().Info("instagram page fetch failed, continuing on the ladder",
			"content_id", identity.ContentID, "error", redact(err))
	}

	prepared := platform.Prepared{
		Caption:      page.Caption,
		ThumbnailURL: h.deps.Thumbnails.Store(ctx, identity, page.ThumbnailURL),
	}

	switch {
	case identity.ContentType == "page":
		// A profile or any other page: whatever text the page carried is all
		// there is, and there is nothing to download.
		prepared.PageText = strings.TrimSpace(page.Title + "\n" + page.Caption)
		if prepared.PageText == "" {
			return platform.Prepared{}, classify(ErrMalformed)
		}
	default:
		// Reels, videos, posts and carousels all have media worth fetching.
		// Which kind is decided in Download, where the page is read again.
		prepared.NeedsMedia = true
	}

	return prepared, nil
}

func (h *Handler) Download(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) ([]ai.Media, error) {
	// The page is read again rather than carried from Prepare: a resumed run
	// may be on another worker, and signed media URLs go stale quickly.
	page, pageErr := h.fetchPage(ctx, identity.NormalizedURL)
	if pageErr != nil {
		if terminal := terminalNow(pageErr); terminal != nil {
			return nil, classify(terminal)
		}
		h.log().Info("instagram page fetch failed before download",
			"content_id", identity.ContentID, "error", redact(pageErr))
	}

	// A carousel or an image post is finished by the page alone: the images are
	// right there and no download tool is needed.
	if wantsSlides(identity, page) {
		media, err := h.downloadSlides(ctx, identity, workDir, page)
		if err != nil {
			return nil, classify(err)
		}
		return media, nil
	}

	videoPath, err := h.downloadVideo(ctx, identity, workDir, page)
	if err != nil {
		return nil, classify(err)
	}

	audioPath, err := h.deps.Audio.ExtractAudio(ctx, videoPath, workDir)
	if err != nil && !errors.Is(err, media.ErrNoAudioStream) {
		return nil, classify(err)
	}
	if audioPath == "" {
		// A silent reel is normal: the caption and any on-screen text carry it,
		// and the pipeline handles empty media.
		h.log().Info("instagram video has no audio", "content_id", identity.ContentID)
		return nil, nil
	}
	return []ai.Media{{Path: audioPath, MIMEType: "audio/mpeg"}}, nil
}

// wantsSlides decides between the image path and the video path. A reel is
// always video; anything else with more images than video is slides.
func wantsSlides(identity sourceidentity.SourceIdentity, page pageContent) bool {
	if identity.ContentType == "reel" || identity.ContentType == "video" {
		return false
	}
	return len(page.ImageURLs) > 0 && (page.VideoURL == "" || len(page.ImageURLs) > 1)
}

func (h *Handler) downloadSlides(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string, page pageContent) ([]ai.Media, error) {
	slides := make([]ai.Media, 0, MaxSlides)
	for index, imageURL := range page.ImageURLs {
		if index >= MaxSlides {
			break
		}
		path := filepath.Join(workDir, fmt.Sprintf("slide-%d.img", index))
		if err := h.fetchFile(ctx, imageURL, path, media.MaxImageBytes); err != nil {
			h.log().Info("instagram slide download failed",
				"content_id", identity.ContentID, "slide", index, "error", redact(err))
			continue
		}

		// The host's Content-Type is a claim; the file's own bytes decide what
		// is handed to a model.
		kind, err := media.SniffKind(path)
		if err != nil || !imageKinds[kind] {
			h.log().Info("instagram slide is not an image",
				"content_id", identity.ContentID, "slide", index, "kind", string(kind))
			os.Remove(path)
			continue
		}
		slides = append(slides, ai.Media{Path: path, MIMEType: "image/" + string(kind)})
	}
	if len(slides) == 0 {
		return nil, ErrMalformed
	}
	h.log().Info("instagram slides ready",
		"content_id", identity.ContentID, "slides", len(slides), "source", "page")
	return slides, nil
}

var imageKinds = map[media.Kind]bool{
	media.KindJPEG: true,
	media.KindPNG:  true,
	media.KindWebP: true,
	media.KindGIF:  true,
}

// downloadVideo walks the ladder. Each rung is tried only when the one before
// could not finish the job, and a terminal failure stops the walk entirely.
func (h *Handler) downloadVideo(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string, page pageContent) (string, error) {
	path := filepath.Join(workDir, "source.mp4")

	// Rung one: the page handed us a media URL. Free, and no tool involved.
	if page.VideoURL != "" {
		if err := h.fetchFile(ctx, page.VideoURL, path, media.MaxMediaBytes); err == nil {
			if kind, err := media.SniffKind(path); err == nil && kind == media.KindMP4 {
				h.log().Info("instagram video ready", "content_id", identity.ContentID, "source", "page")
				return path, nil
			}
			os.Remove(path)
		}
		h.log().Info("instagram public media fetch failed", "content_id", identity.ContentID)
	}

	// Rung two: an anonymous download. Probed first, so an over-long or
	// over-large post costs a metadata call rather than a download.
	if downloaded, err := h.ytdlp(ctx, identity, workDir, ""); err == nil {
		h.log().Info("instagram video ready", "content_id", identity.ContentID, "source", "ytdlp_anonymous")
		return downloaded, nil
	} else if terminal := terminalNow(err); terminal != nil {
		return "", terminal
	} else {
		h.log().Info("instagram anonymous download failed",
			"content_id", identity.ContentID, "error", redact(err))
	}

	// Rung three: the configured actor. This one costs money, so it is only
	// reached when the free rungs are exhausted.
	if h.deps.Apify != nil && h.deps.Apify.Configured(actorName) {
		if downloaded, err := h.downloadViaActor(ctx, identity, workDir); err == nil {
			h.log().Info("instagram video ready", "content_id", identity.ContentID, "source", "apify")
			return downloaded, nil
		} else if terminal := terminalNow(err); terminal != nil {
			return "", terminal
		} else {
			h.log().Info("instagram actor download failed", "content_id", identity.ContentID, "error", redact(err))
		}
	}

	// Rung four: a real account's cookies. Each slot is tried once, and a slot
	// the platform keeps refusing is retired by the jar.
	for _, slot := range h.slots() {
		cookieFile, err := slot.WriteFile(workDir)
		if err != nil {
			continue
		}
		downloaded, err := h.ytdlp(ctx, identity, workDir, cookieFile)
		// The cookie file goes as soon as the attempt ends, whatever happened.
		os.Remove(cookieFile)

		if err == nil {
			h.deps.Cookies.RecordSuccess(slot.Index)
			h.log().Info("instagram video ready",
				"content_id", identity.ContentID, "source", "ytdlp_cookies", "slot", slot.Index)
			return downloaded, nil
		}
		if errors.Is(err, media.ErrLoginRequired) || errors.Is(err, media.ErrRateLimited) {
			h.deps.Cookies.RecordFailure(slot.Index)
			continue
		}
		if terminal := terminalNow(err); terminal != nil {
			return "", terminal
		}
		h.log().Info("instagram cookie slot failed",
			"content_id", identity.ContentID, "slot", slot.Index, "error", redact(err))
	}

	h.log().Info("every instagram download rung failed", "content_id", identity.ContentID)
	return "", ErrExhausted
}

func (h *Handler) slots() []cookies.Slot {
	if h.deps.Cookies == nil {
		return nil
	}
	return h.deps.Cookies.Slots()
}

// ytdlp probes then downloads. The probe is what keeps a two-hour video from
// ever being fetched.
func (h *Handler) ytdlp(ctx context.Context, identity sourceidentity.SourceIdentity, workDir, cookieFile string) (string, error) {
	if h.deps.Probe != nil {
		if _, _, err := h.deps.Probe.Probe(ctx, identity.NormalizedURL); err != nil {
			return "", err
		}
	}
	if h.deps.Downloader == nil {
		return "", ErrExhausted
	}

	download, err := h.deps.Downloader.Download(ctx, identity.NormalizedURL, workDir,
		media.DownloadOptions{CookieFile: cookieFile, MaxBytes: media.MaxMediaBytes})
	if err != nil {
		return "", err
	}
	if kind, err := media.SniffKind(download.VideoPath); err != nil || kind != media.KindMP4 {
		return "", fmt.Errorf("%w: the download is not a video", ErrMalformed)
	}
	return download.VideoPath, nil
}

func (h *Handler) downloadViaActor(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) (string, error) {
	release, err := h.deps.Limits.AcquireActor(ctx, actorName)
	if err != nil {
		return "", err
	}
	defer release()

	items, err := h.deps.Apify.Run(ctx, actorName, map[string]any{
		"directUrls":    []string{identity.NormalizedURL},
		"resultsLimit":  1,
		"addParentData": false,
	})
	if err != nil {
		return "", err
	}

	for _, item := range items {
		var payload struct {
			VideoURL string `json:"videoUrl"`
		}
		if err := json.Unmarshal(item, &payload); err != nil || payload.VideoURL == "" {
			continue
		}
		path := filepath.Join(workDir, "source.mp4")
		if err := h.fetchFile(ctx, payload.VideoURL, path, media.MaxMediaBytes); err != nil {
			return "", err
		}
		if kind, err := media.SniffKind(path); err != nil || kind != media.KindMP4 {
			os.Remove(path)
			return "", fmt.Errorf("%w: the actor's media is not a video", ErrMalformed)
		}
		return path, nil
	}
	return "", fmt.Errorf("%w: the actor returned no media", ErrMalformed)
}

// fetchFile pulls one file through the safe client, which does its own address
// and size checking. The cap here is the caller's, on top of safehttp's own.
func (h *Handler) fetchFile(ctx context.Context, url, destination string, maxBytes int) error {
	release, err := h.deps.Limits.AcquireLightHTTP(ctx)
	if err != nil {
		return err
	}
	defer release()

	response, err := h.deps.HTTP.Get(ctx, url)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProviderOutage, err)
	}
	if err := classifyStatus(response.Status); err != nil {
		return err
	}
	if len(response.Body) == 0 {
		return fmt.Errorf("%w: the media host returned an empty body", ErrMalformed)
	}
	if len(response.Body) > maxBytes {
		return media.ErrTooLarge
	}
	return os.WriteFile(destination, response.Body, 0o600)
}

// terminalNow recognises the failures that will never succeed, so the ladder
// stops instead of spending an actor run or a cookie slot on a deleted post.
func terminalNow(err error) error {
	switch {
	case errors.Is(err, ErrRemoved), errors.Is(err, media.ErrUnavailable):
		return ErrRemoved
	case errors.Is(err, ErrPrivate), errors.Is(err, media.ErrPrivate):
		return ErrPrivate
	case errors.Is(err, media.ErrTooLong), errors.Is(err, media.ErrTooLarge):
		return err
	case errors.Is(err, media.ErrNotAdmitted):
		return err
	}
	return nil
}
