// Package platform is the seam between the pipeline and the places content
// comes from. The pipeline knows nothing about Instagram or YouTube: it asks a
// handler to prepare content and gets back media and text.
package platform

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// ErrNoHandler means nothing claimed this identity. It is terminal: retrying
// will not make a handler appear.
var ErrNoHandler = errors.New("no handler for this source")

// Capabilities tell the pipeline which stages are worth running, so a page with
// no audio never books transcription time.
type Capabilities struct {
	Video   bool
	Audio   bool
	Images  bool
	Caption bool
	// Place means the source carries a location directly, so the pipeline can
	// skip probing for media.
	Place bool
}

// Prepared is what a handler produces: local media paths and whatever text the
// source already carried.
type Prepared struct {
	// VideoPath and AudioPath are files inside the run's own temp directory.
	// The pipeline deletes the directory; a handler never cleans up after
	// itself.
	VideoPath  string
	AudioPath  string
	ImagePaths []string

	Caption    string
	Transcript string
	// TranscriptSource records where a transcript came from when the platform
	// supplied one, so the pipeline does not transcribe again.
	TranscriptSource string
	ThumbnailPath    string
	ThumbnailURL     string
	Title            string
	IngestionMethod  string
}

// Handler prepares one platform's content.
type Handler interface {
	// Name identifies the handler in logs and metrics.
	Name() string
	// Match claims an identity.
	Match(identity sourceidentity.SourceIdentity) bool
	// Capabilities describe what this identity can yield.
	Capabilities(identity sourceidentity.SourceIdentity) Capabilities
	// Normalize is a last chance to correct an identity, for example after a
	// redirect resolved to a canonical post.
	Normalize(ctx context.Context, identity sourceidentity.SourceIdentity) (sourceidentity.SourceIdentity, error)
	// Prepare downloads or fetches into workDir and returns what it found.
	Prepare(ctx context.Context, identity sourceidentity.SourceIdentity, workDir string) (Prepared, error)
}

// Registry holds the handlers this build knows about.
type Registry struct {
	mu       sync.RWMutex
	handlers []Handler
}

func NewRegistry(handlers ...Handler) *Registry {
	registry := &Registry{}
	for _, handler := range handlers {
		registry.Register(handler)
	}
	return registry
}

func (r *Registry) Register(handler Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers = append(r.handlers, handler)
}

// For returns the first handler that claims the identity. Registration order is
// the priority order, so a specific handler registered before a general one
// wins.
func (r *Registry) For(identity sourceidentity.SourceIdentity) (Handler, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, handler := range r.handlers {
		if handler.Match(identity) {
			return handler, nil
		}
	}
	return nil, fmt.Errorf("%w: %s/%s", ErrNoHandler, identity.Platform, identity.ContentType)
}

// Names lists the registered handlers, sorted, for a startup log.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.handlers))
	for _, handler := range r.handlers {
		names = append(names, handler.Name())
	}
	sort.Strings(names)
	return names
}
