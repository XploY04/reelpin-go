package web

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// TestSmokeRealPage fetches one real URL and records what it cost. It is off
// unless REELPIN_SMOKE_URL names a page, because a test that reaches the
// network is a test that fails for reasons unrelated to this code.
//
//	REELPIN_SMOKE_URL=https://example.com/article go test -tags= -run Smoke ./internal/platform/web
func TestSmokeRealPage(t *testing.T) {
	target := os.Getenv("REELPIN_SMOKE_URL")
	if target == "" {
		t.Skip("REELPIN_SMOKE_URL is not set")
	}

	handler := New(Deps{
		HTTP:  safehttp.New(safehttp.Config{}),
		Limit: providers.NewLimits(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	started := time.Now()
	prepared, err := handler.Prepare(ctx, sourceidentity.SourceIdentity{
		NormalizedURL: target,
		OriginalURL:   target,
		Platform:      PlatformName,
		ContentType:   "link",
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Prepare(%s): %v (after %s)", target, err, elapsed)
	}

	// The numbers are the point of a smoke run: one fetch, how long, how much.
	t.Logf("smoke result: provider_calls=1 duration=%s caption_bytes=%d text_bytes=%d needs_media=%v",
		elapsed, len(prepared.Caption), len(prepared.PageText), prepared.NeedsMedia)
}
