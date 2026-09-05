// Package pipeline runs one global processing run from a shared link to a saved
// reel. It is platform-neutral: everything that touches a provider, a model or
// storage is an interface, and the stages themselves only move data.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// FailureClass decides what happens next: try again, wait for a provider, or
// stop for good.
type FailureClass int

const (
	// Transient covers a timeout or a blip. Retry with backoff.
	Transient FailureClass = iota
	// ProviderExhausted means the provider is refusing right now. Retry after a
	// cooldown that applies to the whole platform, not just this run.
	ProviderExhausted
	// ContentTerminal means this content will never process: a deleted post, a
	// page with nothing in it. Stop, and record it for the user.
	ContentTerminal
	// Internal is a bug or a broken dependency. Retry a bounded number of times,
	// then dead-letter for a person.
	Internal
)

// String is the label a metric or a log carries. It never includes the error
// text, which can quote a provider.
func (c FailureClass) String() string {
	switch c {
	case Transient:
		return "transient"
	case ProviderExhausted:
		return "provider_exhausted"
	case ContentTerminal:
		return "content_terminal"
	case Internal:
		return "internal"
	default:
		return "unknown"
	}
}

// ErrForeignEnvironment is a misconfiguration, not a content problem: two
// deployments are sharing a broker. It is terminal because retrying sends the
// same message back to the same wrong worker.
func ErrForeignEnvironment(runEnvironment, workerEnvironment string) *Failure {
	return newFailure(ContentTerminal, "foreign_environment",
		"This job belongs to another environment.",
		fmt.Errorf("run belongs to %q but this worker serves %q: the deployments are sharing a broker",
			runEnvironment, workerEnvironment))
}

// Failure is what a stage returns when it cannot finish.
type Failure struct {
	Class FailureClass
	// Code is the stable identifier the app already knows.
	Code string
	// Message is what the user reads. It never contains a provider's words.
	Message string
	Err     error
}

func (f *Failure) Error() string {
	if f.Err != nil {
		return f.Code + ": " + f.Err.Error()
	}
	return f.Code
}

func (f *Failure) Unwrap() error { return f.Err }

// Terminal reports whether the run should stop rather than retry.
func (f *Failure) Terminal() bool { return f.Class == ContentTerminal }

func newFailure(class FailureClass, code, message string, err error) *Failure {
	return &Failure{Class: class, Code: code, Message: message, Err: err}
}

// Classify turns an arbitrary error into a decision. Anything unrecognised is
// Internal: a bounded retry then a dead letter, never an infinite loop.
func Classify(err error) *Failure {
	if err == nil {
		return nil
	}

	var failure *Failure
	if errors.As(err, &failure) {
		return failure
	}

	switch {
	case errors.Is(err, context.Canceled):
		return newFailure(Transient, "internal_error", "The server could not finish this request.", err)
	case errors.Is(err, context.DeadlineExceeded):
		return newFailure(Transient, "provider_timeout",
			"An upstream provider timed out while processing this request.", err)
	}

	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "rate limit"), strings.Contains(message, "429"),
		strings.Contains(message, "quota"):
		return newFailure(ProviderExhausted, "rate_limit",
			"The source platform is rate limiting requests right now.", err)
	case strings.Contains(message, "timeout"), strings.Contains(message, "deadline"):
		return newFailure(Transient, "provider_timeout",
			"An upstream provider timed out while processing this request.", err)
	}

	return newFailure(Internal, "internal_error", "The server could not finish this request.", err)
}

// The failures a stage raises deliberately. Each maps onto a code the app
// already renders.
func PostNotFound(err error) *Failure {
	return newFailure(ContentTerminal, "post_not_found", "This post was not found.", err)
}

func ProtectedOrUnavailable(err error) *Failure {
	return newFailure(ContentTerminal, "protected_or_unavailable",
		"This post is protected or unavailable.", err)
}

func UnsupportedPostType(err error) *Failure {
	return newFailure(ContentTerminal, "unsupported_post_type",
		"This shared post type is not supported yet.", err)
}

func EmptyPostContent(err error) *Failure {
	return newFailure(ContentTerminal, "empty_post_content",
		"This post has no readable content to save.", err)
}

func NoAudio(err error) *Failure {
	return newFailure(ContentTerminal, "no_audio", "This video does not include an audio track.", err)
}

func TranscriptUnavailable(err error) *Failure {
	return newFailure(ContentTerminal, "transcript_unavailable",
		"A transcript was not available for this media.", err)
}

func RateLimited(err error) *Failure {
	return newFailure(ProviderExhausted, "rate_limit",
		"The source platform is rate limiting requests right now.", err)
}

func AuthFailure(err error) *Failure {
	return newFailure(ProviderExhausted, "auth_failure",
		"The source platform requires a fresh authenticated session.", err)
}
