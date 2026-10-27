package platform

import (
	"context"
	"testing"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

type stubHandler struct{ name string }

func (s stubHandler) Platform() string { return s.name }
func (s stubHandler) Prepare(context.Context, sourceidentity.SourceIdentity) (Prepared, error) {
	return Prepared{}, nil
}
func (s stubHandler) Download(context.Context, sourceidentity.SourceIdentity, string) ([]ai.Media, error) {
	return nil, nil
}

func TestANamedHandlerIsFound(t *testing.T) {
	registry, err := NewRegistry(stubHandler{"instagram"}, stubHandler{"youtube"})
	if err != nil {
		t.Fatal(err)
	}
	handler, ok := registry.Get("instagram")
	if !ok || handler.Platform() != "instagram" {
		t.Fatalf("get = %v, %v", handler, ok)
	}
	if _, ok := registry.Get("myspace"); ok {
		t.Error("an unregistered platform was matched with no fallback registered")
	}
}

func TestTheFallbackCatchesTheLongTail(t *testing.T) {
	// A generic link's platform is its hostname, so there is one "platform"
	// per website and an exact-match registry can never route one.
	registry, err := NewRegistry(stubHandler{"instagram"}, stubHandler{Fallback})
	if err != nil {
		t.Fatal(err)
	}

	handler, ok := registry.Get("nytimes.com")
	if !ok {
		t.Fatal("a generic link found no handler; it would fail as an unsupported platform")
	}
	if handler.Platform() != Fallback {
		t.Fatalf("handler = %q, want the fallback", handler.Platform())
	}

	// A named handler still wins over the fallback.
	named, _ := registry.Get("instagram")
	if named.Platform() != "instagram" {
		t.Fatalf("the fallback shadowed a named handler: %q", named.Platform())
	}
}

func TestDuplicateRegistrationIsRefused(t *testing.T) {
	if _, err := NewRegistry(stubHandler{"instagram"}, stubHandler{"instagram"}); err == nil {
		t.Fatal("a duplicate platform was accepted; one would silently shadow the other")
	}
	if _, err := NewRegistry(stubHandler{Fallback}, stubHandler{Fallback}); err == nil {
		t.Fatal("two fallbacks were accepted")
	}
}
