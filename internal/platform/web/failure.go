package web

import (
	"errors"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/media"
	"github.com/XploY04/reelpin-go/internal/pipeline"
	"github.com/XploY04/reelpin-go/internal/safehttp"
)

// Classify turns a source error into the failure class the pipeline retries
// on. The handler is the only code that knows whether a 404 means "deleted
// forever" or "try again", so it decides here rather than letting the
// pipeline's generic fallback call everything an internal error.
//
// Every light handler in this tree shares it: the question is the same
// whichever page failed.
func Classify(err error) error {
	if err == nil {
		return nil
	}

	// The handler already decided.
	var decided *pipeline.Failure
	if errors.As(err, &decided) {
		return decided
	}

	switch {
	case errors.Is(err, media.ErrLoginRequired):
		return terminal("login_required", "This post is behind a login wall.", err)
	case errors.Is(err, media.ErrPrivate):
		return terminal("content_private", "This post is private.", err)
	case errors.Is(err, media.ErrUnavailable):
		return terminal("content_unavailable", "This post is no longer available.", err)
	case errors.Is(err, media.ErrNotAdmitted):
		return terminal("source_not_supported", "This link cannot be downloaded.", err)
	case errors.Is(err, media.ErrTooLong):
		return terminal("media_too_long", "This video is longer than we can process.", err)
	case errors.Is(err, media.ErrTooLarge):
		return terminal("media_too_large", "This media is larger than we can process.", err)
	case errors.Is(err, media.ErrRateLimited):
		return exhausted("provider_rate_limited", "The source is rate limiting us right now.", err)
	case errors.Is(err, safehttp.ErrUnsafeURL):
		return terminal("source_not_supported", "This link cannot be fetched safely.", err)
	case errors.Is(err, safehttp.ErrTooLarge):
		return terminal("page_too_large", "This page is larger than we can read.", err)
	}

	// A page that answered with a status: 4xx is about this content and will
	// not change, 5xx and 429 are about the server and might.
	var status *StatusError
	if errors.As(err, &status) {
		switch {
		case status.Status == http.StatusTooManyRequests:
			return exhausted("provider_rate_limited", "The source is rate limiting us right now.", err)
		case status.Status == http.StatusNotFound, status.Status == http.StatusGone:
			return terminal("content_unavailable", "This page is no longer available.", err)
		case status.Status == http.StatusUnauthorized, status.Status == http.StatusForbidden:
			return terminal("login_required", "This page is not public.", err)
		case status.Status >= 500:
			return &pipeline.Failure{
				Class:   pipeline.Transient,
				Code:    "provider_unavailable",
				Message: "The source did not respond.",
				Err:     err,
			}
		}
	}

	// Anything else is a network blip or a bug; the pipeline's own classifier
	// gets the last word.
	return err
}

// ErrNothingToRead is a page that loaded and said nothing: no title, no
// description, no image. Another attempt reads the same nothing.
var ErrNothingToRead = errors.New("the page published nothing to extract")

func terminal(code, message string, err error) *pipeline.Failure {
	return &pipeline.Failure{
		Class:   pipeline.ContentTerminal,
		Code:    code,
		Message: message,
		Err:     err,
	}
}

func exhausted(code, message string, err error) *pipeline.Failure {
	return &pipeline.Failure{
		Class:   pipeline.ProviderExhausted,
		Code:    code,
		Message: message,
		Err:     err,
	}
}

// Terminal is the shared constructor for a handler that knows its content will
// never process.
func Terminal(code, message string, err error) *pipeline.Failure {
	return terminal(code, message, err)
}

// IsTerminal reports whether an error is already decided as unretryable. A
// handler uses it to tell "this content is gone" from "this fetch failed",
// when only the first should stop a run that still has another path open.
func IsTerminal(err error) bool {
	var failure *pipeline.Failure
	return errors.As(err, &failure) && !failure.Retryable()
}
