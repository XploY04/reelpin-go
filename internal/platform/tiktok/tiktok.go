// Package tiktok ingests TikTok videos.
//
// TikTok publishes no transcript, so every post is media work: the caption
// comes from the page, the words come from the audio.
package tiktok

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/media"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/platform/web"
	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// PlatformName is the source identity platform this handler serves.
const PlatformName = "tiktok"

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
	Limit      *providers.Limits
	Logger     *slog.Logger
}

type Handler struct {
	deps Deps
}

func New(deps Deps) *Handler { return &Handler{deps: deps} }

func (h *Handler) Platform() string { return PlatformName }

func (h *Handler) Prepare(ctx context.Context, identity sourceidentity.SourceIdentity) (platform.Prepared, error) {
	metadata, err := h.pageMetadata(ctx, identity)
	if err != nil {
		return platform.Prepared{}, err
	}

	// Probe before committing: an over-long or oversized post is refused for
	// the cost of one metadata call rather than a download.
	if h.deps.Prober != nil {
		duration, _, err := h.deps.Prober.Probe(ctx, identity.NormalizedURL)
		if err != nil {
			return platform.Prepared{}, web.Classify(err)
		}
		// A post the probe reports as having no duration carries no audio to
		// transcribe. Its caption is still worth saving, so it becomes light
		// work rather than a failed download.
		if duration == 0 {
			return platform.Prepared{
				Caption:      metadata.Caption(),
				PageText:     metadata.Caption(),
				ThumbnailURL: metadata.ImageURL,
				NeedsMedia:   false,
			}, nil
		}
	}

	return platform.Prepared{
		Caption:      metadata.Caption(),
		ThumbnailURL: metadata.ImageURL,
		NeedsMedia:   true,
	}, nil
}

func (h *Handler) pageMetadata(ctx context.Context, identity sourceidentity.SourceIdentity) (web.Metadata, error) {
	release, err := h.deps.Limit.AcquireLightHTTP(ctx)
	if err != nil {
		return web.Metadata{}, err
	}
	defer release()

	metadata, _, err := web.Fetch(ctx, h.deps.HTTP, identity.NormalizedURL)
	if err != nil {
		classified := web.Classify(err)
		if web.IsTerminal(classified) {
			return web.Metadata{}, classified
		}
		// TikTok serves a wall to plenty of clients while the video itself is
		// still downloadable; a soft page failure must not end the run.
		h.deps.Logger.Info("tiktok page metadata unavailable",
			"content_id", identity.ContentID, "error", err)
		return web.Metadata{}, nil
	}
	return metadata, nil
}

func (h *Handler) Download(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) ([]ai.Media, error) {
	download, err := h.deps.Downloader.Download(ctx, identity.NormalizedURL, workDir,
		media.DownloadOptions{MaxBytes: media.MaxMediaBytes})
	if err != nil {
		return nil, web.Classify(err)
	}

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
