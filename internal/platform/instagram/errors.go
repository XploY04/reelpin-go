package instagram

import (
	"context"
	"errors"
	"regexp"

	"github.com/XploY04/reelpin-go/internal/apify"
	"github.com/XploY04/reelpin-go/internal/media"
	"github.com/XploY04/reelpin-go/internal/pipeline"
)

// The failures this handler distinguishes. They are separate values rather
// than one error with a string, because each one leads somewhere different:
// a login wall is worth another rung of the ladder, a removed post is not.
var (
	// ErrLoginWall is Instagram serving the sign-in page instead of content.
	// Anonymous access failed; credentials might still work.
	ErrLoginWall = errors.New("instagram served a login wall")
	// ErrRemoved is content that no longer exists.
	ErrRemoved = errors.New("the post has been removed")
	// ErrPrivate is content behind an account that does not share publicly.
	ErrPrivate = errors.New("the post is private")
	// ErrRateLimited is Instagram or a provider refusing for now.
	ErrRateLimited = errors.New("instagram is rate limiting")
	// ErrProviderOutage is a provider failing in a way that may pass later.
	ErrProviderOutage = errors.New("an instagram provider is unavailable")
	// ErrMalformed is a response that parsed but carried nothing usable. It is
	// separate from an outage: retrying usually returns the same nothing.
	ErrMalformed = errors.New("instagram returned an unusable response")
	// ErrExhausted is every rung of the ladder having been tried.
	ErrExhausted = errors.New("no instagram source could supply this content")
)

// classify maps a handler error onto the pipeline's failure classes. The
// pipeline short-circuits its own Classify when a stage returns a *Failure, so
// this is where Instagram's knowledge of its own failures is spent.
//
// The rule behind the table: content_terminal means another attempt cannot
// change the answer, so the user is told now and nothing is retried.
func classify(err error) error {
	if err == nil {
		return nil
	}

	switch {
	// Terminal: the content itself is the problem.
	case errors.Is(err, ErrRemoved), errors.Is(err, media.ErrUnavailable):
		return terminal("content_removed", "This post is no longer available.", err)
	case errors.Is(err, ErrPrivate), errors.Is(err, media.ErrPrivate):
		return terminal("content_private", "This post is private.", err)
	case errors.Is(err, media.ErrTooLong), errors.Is(err, media.ErrTooLarge):
		return terminal("content_too_large", "This post is longer or larger than we can process.", err)
	case errors.Is(err, media.ErrNotAdmitted):
		return terminal("unsupported_source", "This link cannot be downloaded.", err)

	// A login wall that survived the whole ladder means every credential we
	// have was refused too, so the user cannot be helped by waiting.
	case errors.Is(err, ErrLoginWall):
		return terminal("login_required", "This post cannot be read without signing in to Instagram.", err)

	// Provider exhausted: the platform is refusing right now and the shared
	// cooldown should hold the whole queue back, not just this run.
	case errors.Is(err, ErrRateLimited), errors.Is(err, media.ErrRateLimited),
		errors.Is(err, apify.ErrRateLimited):
		return &pipeline.Failure{
			Class:   pipeline.ProviderExhausted,
			Code:    "provider_exhausted",
			Message: "Instagram is rate limiting us right now.",
			Err:     err,
		}

	// Transient: something outside us broke and may not be broken next time.
	case errors.Is(err, ErrProviderOutage), errors.Is(err, media.ErrTimedOut),
		errors.Is(err, context.DeadlineExceeded):
		return &pipeline.Failure{
			Class:   pipeline.Transient,
			Code:    "provider_unavailable",
			Message: "An Instagram source did not respond.",
			Err:     err,
		}

	// A malformed response and an exhausted ladder both mean we looked
	// everywhere and found nothing usable. One more attempt is cheap enough to
	// be worth it, and the pipeline's attempt cap stops the third.
	case errors.Is(err, ErrMalformed), errors.Is(err, ErrExhausted):
		return &pipeline.Failure{
			Class:   pipeline.Transient,
			Code:    "content_unreadable",
			Message: "This post could not be read.",
			Err:     err,
		}
	}

	// Anything unrecognised is ours to fix, so it stays internal and loud.
	return err
}

// urlPattern matches any absolute URL in a string.
var urlPattern = regexp.MustCompile(`https?://\S+`)

// redact is what goes into a log line instead of an error. A provider's error
// text is outside our control: yt-dlp quotes the URL it was given and an HTTP
// client quotes the one it failed on, so a signed media URL or the user's own
// link would otherwise land in the log. The class of failure is what a person
// reading logs needs; the address is not.
func redact(err error) string {
	if err == nil {
		return ""
	}
	return urlPattern.ReplaceAllString(err.Error(), "[url]")
}

func terminal(code, message string, err error) *pipeline.Failure {
	return &pipeline.Failure{
		Class:   pipeline.ContentTerminal,
		Code:    code,
		Message: message,
		Err:     err,
	}
}
