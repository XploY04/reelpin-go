// Package youtube ingests YouTube videos and shorts.
//
// The cheap path is tried first: if the platform already publishes a
// transcript, an actor call returns it and the run never downloads a video or
// pays for transcription. Only when there is no transcript does this become
// media work.
package youtube

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/media"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/platform/web"
	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// PlatformName is the source identity platform this handler serves.
const PlatformName = "youtube"

// actorName keys the configured Apify actor.
const actorName = "youtube"

// ActorRunner is the slice of the Apify client this handler uses. Declared
// here rather than taken as a concrete client, so the actor path is testable
// without a network and the handler states exactly what it needs.
// *apify.Client satisfies it.
type ActorRunner interface {
	Configured(platform string) bool
	Run(ctx context.Context, platform string, input any) ([]json.RawMessage, error)
}

// Prober reports what a video is before anything downloads it. *media.YTDLP
// satisfies it; a test supplies the numbers directly so the handler is
// exercised rather than the admission gate.
type Prober interface {
	Probe(ctx context.Context, rawURL string) (durationSeconds int, approxBytes int64, err error)
}

type Deps struct {
	HTTP       *safehttp.Client
	Downloader media.Downloader
	Prober     Prober
	Audio      *media.FFmpeg
	Apify      ActorRunner
	Limit      *providers.Limits
	Logger     *slog.Logger
}

type Handler struct {
	deps Deps
}

func New(deps Deps) *Handler { return &Handler{deps: deps} }

func (h *Handler) Platform() string { return PlatformName }

// actorItem is this package's private view of the actor's dataset row. Other
// handlers call different actors with different shapes; sharing one struct
// would make every one of them wrong.
type actorItem struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Text        string `json:"text"`
	Thumbnail   string `json:"thumbnailUrl"`
	Subtitles   []struct {
		Text string `json:"text"`
		SRT  string `json:"srt"`
	} `json:"subtitles"`
}

func (h *Handler) Prepare(ctx context.Context, identity sourceidentity.SourceIdentity) (platform.Prepared, error) {
	// A published transcript makes the whole media half unnecessary: no
	// download, no audio extraction, no transcription call.
	if prepared, ok := h.prepareFromSubtitles(ctx, identity); ok {
		return prepared, nil
	}

	metadata, err := h.pageMetadata(ctx, identity)
	if err != nil {
		return platform.Prepared{}, err
	}

	// Probe before committing to media: duration and size are known from
	// metadata, so an over-long video costs one probe rather than a download.
	if err := h.probe(ctx, identity); err != nil {
		return platform.Prepared{}, err
	}

	return platform.Prepared{
		Caption:      metadata.Caption(),
		ThumbnailURL: metadata.ImageURL,
		NeedsMedia:   true,
	}, nil
}

// prepareFromSubtitles asks the actor for a published transcript. An actor
// failure is not fatal: the download path is still open, so the error is
// logged and the caller carries on.
func (h *Handler) prepareFromSubtitles(ctx context.Context, identity sourceidentity.SourceIdentity) (platform.Prepared, bool) {
	if h.deps.Apify == nil || !h.deps.Apify.Configured(actorName) {
		return platform.Prepared{}, false
	}

	release, err := h.deps.Limit.AcquireActor(ctx, actorName)
	if err != nil {
		return platform.Prepared{}, false
	}
	defer release()

	items, err := h.deps.Apify.Run(ctx, actorName, map[string]any{
		"startUrls":         []map[string]string{{"url": identity.NormalizedURL}},
		"maxResults":        1,
		"subtitles":         true,
		"downloadSubtitles": true,
	})
	if err != nil {
		h.deps.Logger.Info("youtube actor unavailable",
			"content_id", identity.ContentID, "error", err)
		return platform.Prepared{}, false
	}

	for _, raw := range items {
		var item actorItem
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		transcript := strings.TrimSpace(item.Text)
		for _, subtitle := range item.Subtitles {
			if transcript != "" {
				break
			}
			transcript = strings.TrimSpace(firstOf(subtitle.Text, stripSRT(subtitle.SRT)))
		}
		if transcript == "" {
			continue
		}
		return platform.Prepared{
			Caption:      strings.TrimSpace(item.Title + "\n\n" + item.Description),
			PageText:     transcript,
			ThumbnailURL: item.Thumbnail,
			NeedsMedia:   false,
		}, true
	}
	return platform.Prepared{}, false
}

// pageMetadata reads the watch page for its caption and thumbnail. A failure
// here is not terminal on its own: the video may still be downloadable, so an
// unusable page yields empty metadata rather than an error.
func (h *Handler) pageMetadata(ctx context.Context, identity sourceidentity.SourceIdentity) (web.Metadata, error) {
	release, err := h.deps.Limit.AcquireLightHTTP(ctx)
	if err != nil {
		return web.Metadata{}, err
	}
	defer release()

	metadata, _, err := web.Fetch(ctx, h.deps.HTTP, identity.NormalizedURL)
	if err != nil {
		classified := web.Classify(err)
		// A login wall or a removed video is terminal whatever the downloader
		// would have said; anything softer leaves the download path open.
		if web.IsTerminal(classified) {
			return web.Metadata{}, classified
		}
		h.deps.Logger.Info("youtube page metadata unavailable",
			"content_id", identity.ContentID, "error", err)
		return web.Metadata{}, nil
	}
	return metadata, nil
}

func (h *Handler) probe(ctx context.Context, identity sourceidentity.SourceIdentity) error {
	if h.deps.Prober == nil {
		return nil
	}
	if _, _, err := h.deps.Prober.Probe(ctx, identity.NormalizedURL); err != nil {
		return web.Classify(err)
	}
	return nil
}

func (h *Handler) Download(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) ([]ai.Media, error) {
	download, err := h.deps.Downloader.Download(ctx, identity.NormalizedURL, workDir,
		media.DownloadOptions{MaxBytes: media.MaxMediaBytes})
	if err != nil {
		return nil, web.Classify(err)
	}

	// The file is identified by its own bytes before anything else touches it:
	// a server's content type is a claim, not a fact.
	kind, err := media.SniffKind(download.VideoPath)
	if err != nil || kind != media.KindMP4 {
		return nil, web.Terminal("media_unreadable",
			"The downloaded video could not be read.",
			fmt.Errorf("sniffed %q at %s: %w", kind, filepath.Base(download.VideoPath), err))
	}

	audioPath, err := h.deps.Audio.ExtractAudio(ctx, download.VideoPath, workDir)
	if err != nil {
		return nil, web.Classify(err)
	}
	return []ai.Media{{Path: audioPath, MIMEType: "audio/mpeg"}}, nil
}

// stripSRT removes cue numbers and timings, leaving the spoken words.
func stripSRT(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	lines := []string{}
	for _, line := range strings.Split(value, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.Contains(trimmed, "-->") || isAllDigits(trimmed) {
			continue
		}
		lines = append(lines, trimmed)
	}
	return strings.Join(lines, " ")
}

func isAllDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

func firstOf(values ...string) string {
	for _, value := range values {
		if cleaned := strings.TrimSpace(value); cleaned != "" {
			return cleaned
		}
	}
	return ""
}
