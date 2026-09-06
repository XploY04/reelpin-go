package social

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// The smoke runs reach the real platforms and are off unless their URL is set:
// a test that touches the network fails for reasons unrelated to this code.
//
//	REELPIN_SMOKE_X_URL=https://x.com/user/status/123 \
//	  go test -run Smoke ./internal/platform/social
//
// Each records the numbers a smoke run exists for: how long it took and how
// much came back.
func TestSmokeRealXPost(t *testing.T) {
	smoke(t, "REELPIN_SMOKE_X_URL", NewX(smokeDeps()))
}

func TestSmokeRealLinkedInPage(t *testing.T) {
	smoke(t, "REELPIN_SMOKE_LINKEDIN_URL", NewLinkedIn(smokeDeps()))
}

func TestSmokeRealRedditPost(t *testing.T) {
	smoke(t, "REELPIN_SMOKE_REDDIT_URL", NewReddit(smokeDeps()))
}

func smokeDeps() Deps {
	return Deps{
		HTTP:  safehttp.New(safehttp.Config{}),
		Limit: providers.NewLimits(),
	}
}

func smoke(t *testing.T, variable string, handler platform.Handler) {
	t.Helper()
	target := os.Getenv(variable)
	if target == "" {
		t.Skipf("%s is not set", variable)
	}

	identity, err := sourceidentity.Resolve(target)
	if err != nil {
		t.Fatalf("%s is not a URL this service ingests: %v", variable, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	started := time.Now()
	prepared, err := handler.Prepare(ctx, identity)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Prepare(%s): %v (after %s)", handler.Platform(), err, elapsed)
	}

	t.Logf("smoke result: platform=%s duration=%s caption_bytes=%d text_bytes=%d needs_media=%v",
		handler.Platform(), elapsed, len(prepared.Caption), len(prepared.PageText), prepared.NeedsMedia)
}
