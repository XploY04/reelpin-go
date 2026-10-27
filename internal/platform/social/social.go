// Package social ingests X, LinkedIn and Reddit.
//
// All three are text-first: the post's own words are the content, and any
// picture is supporting. None of them reaches the media queue, because the
// download tool admits only the three video hosts it is allowlisted for, so
// there is nothing for a download or a transcription stage to do. What they
// share instead is a ladder: a free public read first, a paid actor second,
// and a stop the moment the content itself turns out to be the problem.
package social

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"

	"github.com/XploY04/reelpin-go/internal/apify"
	"github.com/XploY04/reelpin-go/internal/pipeline"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/platform/web"
	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
)

// The failures these handlers tell apart. They are separate values because
// each leads somewhere different: a protected post is worth telling the user
// about now, a provider outage is worth another attempt.
var (
	// ErrPostNotFound is a post that no longer exists.
	ErrPostNotFound = errors.New("the post was not found")
	// ErrPostProtected is a post behind an account that does not share
	// publicly.
	ErrPostProtected = errors.New("the post is protected or unavailable")
	// ErrNoPublicContent is a post that loaded and published nothing readable.
	// Another attempt reads the same nothing.
	ErrNoPublicContent = errors.New("the post has no public content")
	// ErrPostMismatch is a provider answering about a different post, which is
	// never safe to save under this identity.
	ErrPostMismatch = errors.New("the provider returned a different post")
	// ErrNotConfigured is a deployment gap rather than a content problem: with
	// no credential there is no way to read this source at all.
	ErrNotConfigured = errors.New("this source has no configured reader")
)

// MaxReplies bounds how much of a thread is read. Past the first few, a thread
// is rarely about the post any more, and every line costs extraction tokens.
const MaxReplies = 10

// ActorRunner is the slice of the Apify client these handlers use. Declared
// here rather than taken as a concrete client, so the paid path is testable
// without a network. *apify.Client satisfies it.
type ActorRunner interface {
	Configured(platform string) bool
	Run(ctx context.Context, platform string, input any) ([]json.RawMessage, error)
}

// Deps are what every handler in this package needs. Each field may be nil:
// a handler that cannot reach its reader says so as a typed failure rather
// than panicking.
type Deps struct {
	HTTP       *safehttp.Client
	Apify      ActorRunner
	Thumbnails platform.Thumbnails
	// Reddit mints the API token Reddit requires from datacenter addresses.
	// Optional: without it the public JSON endpoint is used instead.
	Reddit TokenSource
	Limit  *providers.Limits
	Logger *slog.Logger
}

func (d Deps) limits() *providers.Limits {
	if d.Limit == nil {
		return providers.NewLimits()
	}
	return d.Limit
}

func (d Deps) log(platform string) *slog.Logger {
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return logger.With("platform", platform)
}

// classify maps a handler error onto a pipeline failure class. It adds what
// these three sources know on top of the shared page classifier: a post that
// is gone or protected is the content's own problem and will not change, and
// a paid actor refusing is the whole platform pushing back rather than this
// one run failing.
func classify(err error) error {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, ErrPostNotFound):
		return web.Terminal("content_unavailable", "This post is no longer available.", err)
	case errors.Is(err, ErrPostProtected):
		return web.Terminal("content_private", "This post is not public.", err)
	case errors.Is(err, ErrPostMismatch):
		return web.Terminal("content_unavailable", "This post could not be identified.", err)
	case errors.Is(err, ErrNotConfigured):
		// A missing credential is ours to fix, not the user's to wait out, but
		// it is also not permanent: a deploy can add it, so the run may still
		// succeed later.
		return &pipeline.Failure{
			Class:   pipeline.Transient,
			Code:    "provider_unavailable",
			Message: "This source cannot be read right now.",
			Err:     err,
		}
	case errors.Is(err, apify.ErrRateLimited):
		return &pipeline.Failure{
			Class:   pipeline.ProviderExhausted,
			Code:    "provider_rate_limited",
			Message: "The source is rate limiting us right now.",
			Err:     err,
		}
	case errors.Is(err, ErrNoPublicContent):
		return web.Terminal("page_empty", "This post has nothing to save.", err)
	}

	// Everything else is a page-shaped failure the shared classifier already
	// knows: a status, a size cap, an unsafe address, or a bug that stays
	// internal.
	return web.Classify(err)
}

// urlPattern matches any absolute URL in a string.
var urlPattern = regexp.MustCompile(`https?://\S+`)

// redact is what goes into a log line instead of an error. A provider quotes
// the URL it failed on, and an API error can echo a bearer token back, so the
// class of failure is logged and the address is not.
func redact(err error) string {
	if err == nil {
		return ""
	}
	return urlPattern.ReplaceAllString(err.Error(), "[url]")
}

// statusError turns an HTTP status into the error that says what it means for
// this content. 404 and 410 are gone for good; 401 and 403 are not ours to
// read; everything else is left for the shared classifier to judge.
func statusError(status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == 404, status == 410:
		return ErrPostNotFound
	case status == 401, status == 403:
		return ErrPostProtected
	default:
		return &web.StatusError{Status: status}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if cleaned := strings.TrimSpace(value); cleaned != "" {
			return cleaned
		}
	}
	return ""
}
