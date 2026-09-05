// Package instagram prepares Instagram reels, posts and carousels.
//
// The fallback ladder matters more than any single step: a public page fetch
// costs nothing, an anonymous download costs little, the configured actor costs
// money, and a cookie slot spends a real account's standing. They are tried in
// that order, and the ladder stops at the first thing that works.
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

	"github.com/XploY04/reelpin-go/internal/apify"
	"github.com/XploY04/reelpin-go/internal/cookies"
	"github.com/XploY04/reelpin-go/internal/media"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/XploY04/reelpin-go/internal/storage"
)

const (
	// MaxImages bounds a carousel: reading twenty slides costs twenty times as
	// much and tells us almost nothing more.
	MaxImages = 8
	// MaxVideoBytes refuses a download that would fill the disk.
	MaxVideoBytes = 200 << 20
)

type Deps struct {
	HTTP       *safehttp.Client
	Downloader media.Downloader
	Audio      *media.FFmpeg
	Apify      *apify.Client
	Cookies    *cookies.Jar
	Storage    storage.Uploader
	Logger     *slog.Logger
}

type Handler struct {
	deps Deps
}

func New(deps Deps) *Handler {
	return &Handler{deps: deps}
}

func (h *Handler) Name() string { return "instagram" }

func (h *Handler) Match(identity sourceidentity.SourceIdentity) bool {
	return identity.Platform == "instagram"
}

func (h *Handler) Capabilities(identity sourceidentity.SourceIdentity) platform.Capabilities {
	switch identity.ContentType {
	case "reel", "video":
		return platform.Capabilities{Video: true, Audio: true, Caption: true}
	case "post":
		// A post may be images or a video; both paths are possible until the
		// page says which.
		return platform.Capabilities{Video: true, Audio: true, Images: true, Caption: true}
	default:
		return platform.Capabilities{Caption: true}
	}
}

func (h *Handler) Normalize(_ context.Context, identity sourceidentity.SourceIdentity) (sourceidentity.SourceIdentity, error) {
	return identity, nil
}

func (h *Handler) Prepare(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) (platform.Prepared, error) {
	page, err := h.fetchPage(ctx, identity.NormalizedURL)
	if err != nil {
		h.deps.Logger.Info("instagram page fetch failed",
			"content_id", identity.ContentID, "error", err)
	}

	// A carousel or an image post is finished by the page alone: the images are
	// right there, and no download tool is needed.
	if len(page.ImageURLs) > 0 && (page.VideoURL == "" || len(page.ImageURLs) > 1) &&
		identity.ContentType != "reel" {
		return h.prepareImages(ctx, identity, workDir, page)
	}

	videoPath, cookieSlot, err := h.downloadVideo(ctx, identity, workDir, page)
	if err != nil {
		return platform.Prepared{}, err
	}

	audioPath, err := h.deps.Audio.ExtractAudio(ctx, videoPath, workDir)
	if err != nil && !errors.Is(err, media.ErrNoAudioStream) {
		return platform.Prepared{}, err
	}

	prepared := platform.Prepared{
		VideoPath:        videoPath,
		AudioPath:        audioPath,
		Caption:          page.Caption,
		Title:            page.Title,
		IngestionMethod:  "instagram_reel_pipeline",
		TranscriptSource: "gemini_audio",
	}
	if audioPath == "" {
		// No sound: the caption is all this reel says.
		prepared.TranscriptSource = ""
	}
	if cookieSlot >= 0 {
		h.deps.Cookies.RecordSuccess(cookieSlot)
	}

	prepared.ThumbnailURL = h.storeThumbnail(ctx, identity, page.ThumbnailURL)
	return prepared, nil
}

func (h *Handler) prepareImages(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string, page pageContent) (platform.Prepared, error) {
	paths := make([]string, 0, MaxImages)
	for index, imageURL := range page.ImageURLs {
		if index >= MaxImages {
			break
		}
		path := filepath.Join(workDir, fmt.Sprintf("slide-%d.jpg", index))
		if err := h.downloadFile(ctx, imageURL, path); err != nil {
			h.deps.Logger.Info("instagram slide download failed",
				"content_id", identity.ContentID, "error", err)
			continue
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return platform.Prepared{}, fmt.Errorf("no slides could be downloaded")
	}

	prepared := platform.Prepared{
		ImagePaths:       paths,
		Caption:          page.Caption,
		Title:            page.Title,
		IngestionMethod:  "instagram_post_pipeline",
		TranscriptSource: "gemini_vision_ocr",
		ThumbnailPath:    paths[0],
	}
	// The first slide is the stable thumbnail: it is what the app shows, and it
	// does not change when a later slide does.
	prepared.ThumbnailURL = h.storeThumbnailFile(ctx, identity, paths[0])
	return prepared, nil
}

// downloadVideo walks the ladder. Each rung is only tried when the one before
// it could not finish the job.
func (h *Handler) downloadVideo(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string, page pageContent) (string, int, error) {
	if page.VideoURL != "" {
		path := filepath.Join(workDir, "source.mp4")
		if err := h.downloadFile(ctx, page.VideoURL, path); err == nil {
			return path, -1, nil
		}
		h.deps.Logger.Info("instagram public video fetch failed", "content_id", identity.ContentID)
	}

	download, err := h.deps.Downloader.Download(ctx, identity.NormalizedURL, workDir,
		media.DownloadOptions{MaxBytes: MaxVideoBytes})
	if err == nil {
		return download.VideoPath, -1, nil
	}
	if terminal := terminalDownload(err); terminal != nil {
		return "", -1, terminal
	}

	if h.deps.Apify != nil && h.deps.Apify.Configured("instagram") {
		if path, err := h.downloadViaApify(ctx, identity, workDir); err == nil {
			return path, -1, nil
		} else if terminal := terminalDownload(err); terminal != nil {
			return "", -1, terminal
		}
	}

	// Last: a real account's cookies. Each slot is tried once, and a slot that
	// keeps being refused is retired.
	for _, slot := range h.deps.Cookies.Slots() {
		cookieFile, err := slot.WriteFile(workDir)
		if err != nil {
			continue
		}
		download, err := h.deps.Downloader.Download(ctx, identity.NormalizedURL, workDir,
			media.DownloadOptions{CookieFile: cookieFile, MaxBytes: MaxVideoBytes})
		// The file is removed as soon as the attempt ends, whatever happened.
		os.Remove(cookieFile)

		if err == nil {
			return download.VideoPath, slot.Index, nil
		}
		if errors.Is(err, media.ErrLoginRequired) || errors.Is(err, media.ErrRateLimited) {
			h.deps.Cookies.RecordFailure(slot.Index)
			continue
		}
		if terminal := terminalDownload(err); terminal != nil {
			return "", -1, terminal
		}
	}

	return "", -1, fmt.Errorf("every instagram download path failed")
}

func (h *Handler) downloadViaApify(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) (string, error) {
	items, err := h.deps.Apify.Run(ctx, "instagram", map[string]any{
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
			Caption  string `json:"caption"`
		}
		if err := json.Unmarshal(item, &payload); err != nil || payload.VideoURL == "" {
			continue
		}
		path := filepath.Join(workDir, "source.mp4")
		if err := h.downloadFile(ctx, payload.VideoURL, path); err != nil {
			return "", err
		}
		return path, nil
	}
	return "", fmt.Errorf("the actor returned no media")
}

// terminalDownload recognises the failures that will never succeed, so the
// ladder stops instead of spending a cookie slot on a deleted post.
func terminalDownload(err error) error {
	switch {
	case errors.Is(err, media.ErrUnavailable), errors.Is(err, media.ErrPrivate):
		return err
	case errors.Is(err, media.ErrTooLarge):
		return err
	}
	return nil
}

func (h *Handler) downloadFile(ctx context.Context, url, destination string) error {
	response, err := h.deps.HTTP.Get(ctx, url)
	if err != nil {
		return err
	}
	if response.Status < 200 || response.Status >= 300 {
		return fmt.Errorf("the media host returned HTTP %d", response.Status)
	}
	if len(response.Body) == 0 {
		return fmt.Errorf("the media host returned an empty body")
	}
	return os.WriteFile(destination, response.Body, 0o600)
}

func (h *Handler) storeThumbnail(ctx context.Context, identity sourceidentity.SourceIdentity, thumbnailURL string) string {
	if thumbnailURL == "" || h.deps.Storage == nil {
		return ""
	}
	response, err := h.deps.HTTP.Get(ctx, thumbnailURL)
	if err != nil || response.Status < 200 || response.Status >= 300 {
		return ""
	}
	return h.upload(ctx, identity, response.Body)
}

func (h *Handler) storeThumbnailFile(ctx context.Context, identity sourceidentity.SourceIdentity, path string) string {
	if h.deps.Storage == nil {
		return ""
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return h.upload(ctx, identity, body)
}

// upload never fails the run: a missing thumbnail is a cosmetic loss, and the
// reel is worth saving without it.
func (h *Handler) upload(ctx context.Context, identity sourceidentity.SourceIdentity, body []byte) string {
	key := storage.Key(identity.Platform, identity.ContentType, identity.ContentID, identity.NormalizedURL, ".jpg")
	url, err := h.deps.Storage.Upload(ctx, key, strings.NewReader(string(body)), "image/jpeg")
	if err != nil {
		h.deps.Logger.Info("thumbnail upload failed", "content_id", identity.ContentID, "error", err)
		return ""
	}
	return url
}
