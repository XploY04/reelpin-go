package main

import (
	"fmt"
	"log/slog"

	"github.com/XploY04/reelpin-go/internal/apify"
	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/cookies"
	"github.com/XploY04/reelpin-go/internal/media"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/platform/instagram"
	"github.com/XploY04/reelpin-go/internal/platform/pinterest"
	"github.com/XploY04/reelpin-go/internal/platform/places"
	"github.com/XploY04/reelpin-go/internal/platform/social"
	"github.com/XploY04/reelpin-go/internal/platform/tiktok"
	"github.com/XploY04/reelpin-go/internal/platform/web"
	"github.com/XploY04/reelpin-go/internal/platform/youtube"
	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/spend"
	"github.com/XploY04/reelpin-go/internal/storage"
)

// newRegistry registers every handler this worker can dispatch to. A platform
// the resolver can name with nothing registered for it fails every run it
// routes, so this list is checked against internal/sourceidentity in
// platforms_test.go rather than kept in step by hand.
//
// Nothing here needs a credential to be built. A provider with no token is a
// rung that reports itself unconfigured and a handler that takes its free path
// instead, so one missing token never touches the platforms that do not use it.
func newRegistry(cfg config.Config, usage spend.Recorder, logger *slog.Logger) (*platform.Registry, error) {
	actors, err := apify.ParseActors(cfg.ApifyActors)
	if err != nil {
		return nil, fmt.Errorf("APIFY_ACTORS: %w", err)
	}
	actorRunner := apify.New(apify.Config{Token: cfg.ApifyToken, Actors: actors, Usage: usage})

	client := safehttp.New(safehttp.Config{})
	limits := providers.NewLimits()

	downloader := media.NewYTDLP(nil)
	downloader.Binary = cfg.YTDLPPath
	audio := media.NewFFmpeg(nil)
	audio.Binary = cfg.FFmpegPath

	// An absent uploader is how a handler skips the thumbnail entirely. A
	// configured-looking one would fetch the image first and only then find it
	// has nowhere to put it.
	var thumbnails storage.Uploader
	if cfg.SupabaseURL != "" && cfg.SupabaseServiceKey != "" {
		thumbnails = storage.NewSupabase(cfg.SupabaseURL, cfg.ThumbnailBucket, cfg.SupabaseServiceKey, 0)
	}

	// The three text-first sources share one set of dependencies; each of them
	// reaches a different reader behind it.
	socialDeps := social.Deps{
		HTTP:    client,
		Apify:   actorRunner,
		Storage: thumbnails,
		Limit:   limits,
		Logger:  logger,
	}

	handlers := []platform.Handler{
		instagram.New(instagram.Deps{
			HTTP:       client,
			Downloader: downloader,
			Probe:      downloader,
			Audio:      audio,
			Apify:      actorRunner,
			Cookies:    cookies.New(cfg.InstagramCookies),
			Storage:    thumbnails,
			Limits:     limits,
			Logger:     logger,
		}),
		youtube.New(youtube.Deps{
			HTTP:       client,
			Downloader: downloader,
			Prober:     downloader,
			Audio:      audio,
			Apify:      actorRunner,
			Limit:      limits,
			Logger:     logger,
		}),
		tiktok.New(tiktok.Deps{
			HTTP:       client,
			Downloader: downloader,
			Prober:     downloader,
			Audio:      audio,
			Limit:      limits,
			Logger:     logger,
		}),
		pinterest.New(pinterest.Deps{HTTP: client, Limit: limits, Logger: logger}),
		social.NewX(socialDeps),
		social.NewLinkedIn(socialDeps),
		social.NewReddit(socialDeps),
		// The long tail: every link whose platform is its own hostname.
		web.New(web.Deps{HTTP: client, Limit: limits}),
	}
	handlers = append(handlers,
		places.Handlers(places.Deps{HTTP: client, Limit: limits, Logger: logger})...)

	return platform.NewRegistry(handlers...)
}
