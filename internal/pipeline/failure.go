package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/XploY04/reelpin-go/internal/ai"
)

// Class decides what happens next. The names match the schema's error_class
// check constraint exactly; a new class is a migration, not a string.
type Class string

const (
	// Transient is a blip: retry with backoff.
	Transient Class = "transient"
	// ProviderExhausted is a provider refusing right now: retry after its
	// push-back and the shared cooldown.
	ProviderExhausted Class = "provider_exhausted"
	// ContentTerminal will never succeed for this content: stop and tell the
	// user with a stable code.
	ContentTerminal Class = "content_terminal"
	// Internal is a bug or broken dependency: bounded retries, then a person.
	Internal Class = "internal"
)

// Failure is what a stage returns when it cannot finish. Code is the stable
// public identifier the app matches on; Message is what a user reads; Err is
// for logs and stage rows only.
type Failure struct {
	Class   Class
	Code    string
	Message string
	Err     error
}

func (f *Failure) Error() string {
	return fmt.Sprintf("%s (%s/%s): %v", f.Message, f.Class, f.Code, f.Err)
}

func (f *Failure) Unwrap() error { return f.Err }

// Retryable says whether another attempt can change the answer.
func (f *Failure) Retryable() bool {
	return f.Class == Transient || f.Class == ProviderExhausted || f.Class == Internal
}

func fail(class Class, code, message string, err error) *Failure {
	return &Failure{Class: class, Code: code, Message: message, Err: err}
}

// Classify turns an arbitrary stage error into a Failure. A stage that knows
// better returns a *Failure directly and short-circuits this.
func Classify(err error) *Failure {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure
	}

	var provider *ai.ProviderError
	if errors.As(err, &provider) {
		switch {
		case provider.StatusCode == http.StatusTooManyRequests,
			provider.StatusCode == http.StatusServiceUnavailable:
			return fail(ProviderExhausted, "provider_exhausted",
				"The AI provider is busy right now.", err)
		case provider.StatusCode >= 500 || provider.StatusCode == 0:
			return fail(Transient, "provider_unavailable",
				"An upstream provider did not respond.", err)
		default:
			// 4xx from the provider is our request being wrong: a bug.
			return fail(Internal, "internal_error",
				"The server could not finish this request.", err)
		}
	}

	if errors.Is(err, ai.ErrInvalidExtraction) || errors.Is(err, ai.ErrEmptyResponse) {
		// The model looked and produced nothing usable. Retrying the same
		// input costs money for the same answer once, then gives up.
		return fail(Transient, "extraction_invalid",
			"The content could not be understood.", err)
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return fail(Transient, "provider_timeout",
			"A processing step took too long.", err)
	}

	return fail(Internal, "internal_error",
		"The server could not finish this request.", err)
}

// Terminal failures stages raise directly.
var (
	errUnsupportedPlatform = fail(ContentTerminal, "unsupported_platform",
		"This source is not supported yet.", errors.New("no handler for platform"))
	errRunExpired = fail(ContentTerminal, "processing_timeout",
		"Processing took too long and was stopped.", errors.New("the run deadline passed"))
)
