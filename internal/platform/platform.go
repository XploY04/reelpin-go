// Package platform is the seam between the pipeline and the sources it
// ingests. One handler per platform, behind one interface, so adding a source
// is one new package and one registration. The concrete handlers arrive with
// their own tasks; the pipeline is built and tested against this seam.
package platform

import (
	"context"
	"fmt"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// Prepared is what stage one produces: everything cheap to know about the
// content before any provider-costing work.
type Prepared struct {
	// Caption is the author's own text, when the platform exposes it.
	Caption string
	// PageText is extracted text for light content that needs no media work.
	PageText string
	// ThumbnailURL is a stable preview image reference, when one exists.
	ThumbnailURL string
	// NeedsMedia says the download and transcribe stages have work to do.
	// Routing to the media queue was decided at enqueue from platform and
	// source metadata; this flag is the handler confirming it after looking.
	NeedsMedia bool
}

// Handler ingests one platform.
type Handler interface {
	// Platform is the source identity platform this handler serves.
	Platform() string
	// Prepare inspects the content and returns the cheap half.
	Prepare(ctx context.Context, identity sourceidentity.SourceIdentity) (Prepared, error)
	// Download fetches media into workDir and returns the files for
	// transcription, in the order they should be heard or read.
	Download(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) ([]ai.Media, error)
}

// Registry holds one handler per platform. Duplicate registration is a
// programming error and fails construction rather than shadowing silently.
type Registry struct {
	handlers map[string]Handler
}

func NewRegistry(handlers ...Handler) (*Registry, error) {
	registry := &Registry{handlers: map[string]Handler{}}
	for _, handler := range handlers {
		name := handler.Platform()
		if _, duplicate := registry.handlers[name]; duplicate {
			return nil, fmt.Errorf("platform %q is registered twice", name)
		}
		registry.handlers[name] = handler
	}
	return registry, nil
}

// Get returns the handler for a platform. A missing handler is not an error
// here: the pipeline classifies it as terminal for the run, with its own code.
func (r *Registry) Get(platform string) (Handler, bool) {
	handler, ok := r.handlers[platform]
	return handler, ok
}
