package web

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
	// MaxImages bounds an image-only page.
	MaxImages = 4
	// MaxVideoBytes refuses a download that would fill the disk.
	MaxVideoBytes = 300 << 20
)

// placePlatforms carry a location directly. Probing them for video wastes a
// download attempt on a restaurant listing.
var placePlatforms = map[string]bool{
	"google_maps": true,
	"tripadvisor": true,
	"airbnb":      true,
	"zomato":      true,
	"swiggy":      true,
	"makemytrip":  true,
	"booking":     true,
}

// videoPlatforms are the ones worth a download attempt before falling back to
// page metadata.
var videoPlatforms = map[string]bool{
	"youtube":   true,
	"tiktok":    true,
	"pinterest": true,
}

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

func New(deps Deps) *Handler { return &Handler{deps: deps} }

func (h *Handler) Name() string { return "web" }

// Match claims everything the dedicated handlers did not. It is registered
// last, so this is the fallback for any link a user shares.
func (h *Handler) Match(identity sourceidentity.SourceIdentity) bool {
	switch identity.Platform {
	case "instagram", "x", "reddit":
		return false
	case "linkedin":
		// Only a LinkedIn post has a dedicated reader. Profiles, companies and
		// articles publish ordinary metadata and belong here.
		return identity.ContentType != "post"
	}
	return true
}

func (h *Handler) Capabilities(identity sourceidentity.SourceIdentity) platform.Capabilities {
	switch {
	case placePlatforms[identity.Platform]:
		return platform.Capabilities{Place: true, Caption: true, Images: true}
	case videoPlatforms[identity.Platform]:
		return platform.Capabilities{Video: true, Audio: true, Caption: true, Images: true}
	default:
		return platform.Capabilities{Caption: true, Images: true, Video: true}
	}
}

func (h *Handler) Normalize(_ context.Context, identity sourceidentity.SourceIdentity) (sourceidentity.SourceIdentity, error) {
	return identity, nil
}

func (h *Handler) Prepare(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) (platform.Prepared, error) {
	// A curated place link goes straight to metadata: the location is the
	// content, and there is no video to look for.
	if placePlatforms[identity.Platform] {
		return h.preparePlace(ctx, identity, workDir)
	}

	if transcript, prepared, ok, err := h.preparePlatformText(ctx, identity, workDir); err != nil {
		return platform.Prepared{}, err
	} else if ok {
		prepared.Transcript = transcript
		return prepared, nil
	}

	if videoPlatforms[identity.Platform] || identity.ContentType != "link" {
		if prepared, ok, err := h.prepareVideo(ctx, identity, workDir); err != nil {
			return platform.Prepared{}, err
		} else if ok {
			return prepared, nil
		}
	}

	return h.prepareMetadata(ctx, identity, workDir)
}

// preparePlatformText asks the configured actor for a transcript before any
// media is downloaded. Subtitles are cheaper than a download plus a model call.
func (h *Handler) preparePlatformText(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) (string, platform.Prepared, bool, error) {
	if identity.Platform != "youtube" || h.deps.Apify == nil || !h.deps.Apify.Configured("youtube") {
		return "", platform.Prepared{}, false, nil
	}

	items, err := h.deps.Apify.Run(ctx, "youtube", map[string]any{
		"startUrls":         []map[string]string{{"url": identity.NormalizedURL}},
		"maxResults":        1,
		"subtitles":         true,
		"downloadSubtitles": true,
	})
	if err != nil {
		// An actor failure is not fatal: the download path is still open.
		h.deps.Logger.Info("youtube actor unavailable", "content_id", identity.ContentID, "error", err)
		return "", platform.Prepared{}, false, nil
	}

	for _, item := range items {
		var payload struct {
			Title     string `json:"title"`
			Text      string `json:"text"`
			Subtitles []struct {
				Text string `json:"text"`
				SRT  string `json:"srt"`
			} `json:"subtitles"`
			Thumbnail string `json:"thumbnailUrl"`
			Caption   string `json:"description"`
		}
		if err := json.Unmarshal(item, &payload); err != nil {
			continue
		}

		transcript := strings.TrimSpace(payload.Text)
		for _, subtitle := range payload.Subtitles {
			if transcript != "" {
				break
			}
			transcript = strings.TrimSpace(firstOf(subtitle.Text, stripSRT(subtitle.SRT)))
		}
		if transcript == "" {
			continue
		}

		prepared := platform.Prepared{
			Caption:          payload.Caption,
			Title:            payload.Title,
			IngestionMethod:  "youtube_subtitles",
			TranscriptSource: "platform_subtitles",
		}
		prepared.ThumbnailURL = h.storeThumbnail(ctx, identity, payload.Thumbnail)
		return transcript, prepared, true, nil
	}
	return "", platform.Prepared{}, false, nil
}

// prepareVideo downloads and extracts audio. A refusal is not fatal here: the
// page's own metadata is still worth saving.
func (h *Handler) prepareVideo(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) (platform.Prepared, bool, error) {
	options := media.DownloadOptions{MaxBytes: MaxVideoBytes}
	download, err := h.deps.Downloader.Download(ctx, identity.NormalizedURL, workDir, options)

	if err != nil && identity.Platform == "tiktok" && h.deps.Cookies != nil {
		// TikTok is the one platform here that sometimes needs a session.
		for _, slot := range h.deps.Cookies.Slots() {
			cookieFile, writeErr := slot.WriteFile(workDir)
			if writeErr != nil {
				continue
			}
			options.CookieFile = cookieFile
			download, err = h.deps.Downloader.Download(ctx, identity.NormalizedURL, workDir, options)
			os.Remove(cookieFile)
			if err == nil {
				h.deps.Cookies.RecordSuccess(slot.Index)
				break
			}
			if errors.Is(err, media.ErrLoginRequired) || errors.Is(err, media.ErrRateLimited) {
				h.deps.Cookies.RecordFailure(slot.Index)
			}
		}
	}

	if err != nil {
		switch {
		case errors.Is(err, media.ErrUnavailable), errors.Is(err, media.ErrPrivate):
			// The post is gone: metadata will not rescue it.
			return platform.Prepared{}, false, err
		}
		h.deps.Logger.Info("video download failed, falling back to metadata",
			"platform", identity.Platform, "content_id", identity.ContentID)
		return platform.Prepared{}, false, nil
	}

	audioPath, err := h.deps.Audio.ExtractAudio(ctx, download.VideoPath, workDir)
	if err != nil && !errors.Is(err, media.ErrNoAudioStream) {
		return platform.Prepared{}, false, err
	}

	metadata, metadataErr := fetchMetadata(ctx, h.deps.HTTP, identity.NormalizedURL)
	if metadataErr != nil {
		h.deps.Logger.Info("page metadata unavailable", "content_id", identity.ContentID)
	}

	prepared := platform.Prepared{
		VideoPath:        download.VideoPath,
		AudioPath:        audioPath,
		Caption:          metadata.Description,
		Title:            firstOf(metadata.Title, download.Title),
		IngestionMethod:  identity.Platform + "_video_pipeline",
		TranscriptSource: "gemini_audio",
	}
	if audioPath == "" {
		prepared.TranscriptSource = ""
	}
	prepared.ThumbnailURL = h.storeThumbnail(ctx, identity, metadata.ImageURL)
	return prepared, true, nil
}

// prepareMetadata is the last resort and the whole story for an article or a
// product page.
func (h *Handler) prepareMetadata(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) (platform.Prepared, error) {
	metadata, err := fetchMetadata(ctx, h.deps.HTTP, identity.NormalizedURL)
	if err != nil {
		return platform.Prepared{}, err
	}
	if !metadata.Usable() {
		// No video, no title, no description, no image: nothing to save.
		return platform.Prepared{}, ErrNothingToSave
	}

	prepared := platform.Prepared{
		Caption:          metadata.Text(),
		Title:            metadata.Title,
		IngestionMethod:  "web_metadata",
		TranscriptSource: "page_metadata",
	}

	// An image-led page is worth reading: a menu or a poster carries its text
	// in the picture.
	if metadata.ImageURL != "" {
		path := filepath.Join(workDir, "page-image.jpg")
		if err := h.downloadFile(ctx, metadata.ImageURL, path); err == nil {
			prepared.ImagePaths = []string{path}
			prepared.ThumbnailPath = path
		}
	}
	prepared.ThumbnailURL = h.storeThumbnail(ctx, identity, metadata.ImageURL)
	return prepared, nil
}

func (h *Handler) preparePlace(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) (platform.Prepared, error) {
	prepared, err := h.prepareMetadata(ctx, identity, workDir)
	if err != nil {
		return platform.Prepared{}, err
	}
	prepared.IngestionMethod = "place_metadata"
	return prepared, nil
}

// ErrNothingToSave is a page that published nothing at all.
var ErrNothingToSave = errors.New("the page has no title, description, image or video")

func (h *Handler) downloadFile(ctx context.Context, url, destination string) error {
	response, err := h.deps.HTTP.Get(ctx, url)
	if err != nil {
		return err
	}
	if response.Status < 200 || response.Status >= 300 {
		return fmt.Errorf("the host returned HTTP %d", response.Status)
	}
	if len(response.Body) == 0 {
		return fmt.Errorf("the host returned an empty body")
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

	key := storage.Key(identity.Platform, identity.ContentType, identity.ContentID, identity.NormalizedURL, ".jpg")
	url, err := h.deps.Storage.Upload(ctx, key, strings.NewReader(string(response.Body)), "image/jpeg")
	if err != nil {
		// A missing thumbnail is cosmetic; the save is not.
		h.deps.Logger.Info("thumbnail upload failed", "content_id", identity.ContentID, "error", err)
		return ""
	}
	return url
}

// stripSRT turns subtitle cues into plain text: the timings are noise to an
// extractor.
func stripSRT(subtitle string) string {
	if strings.TrimSpace(subtitle) == "" {
		return ""
	}
	lines := []string{}
	for _, line := range strings.Split(subtitle, "\n") {
		cleaned := strings.TrimSpace(line)
		if cleaned == "" || strings.Contains(cleaned, "-->") {
			continue
		}
		if _, err := parseInt(cleaned); err == nil {
			continue
		}
		lines = append(lines, cleaned)
	}
	return strings.Join(lines, " ")
}

func parseInt(value string) (int, error) {
	number := 0
	if value == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		number = number*10 + int(r-'0')
	}
	return number, nil
}
