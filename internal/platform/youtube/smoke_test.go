package youtube

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/media"
	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// TestSmokeRealVideo prepares one real video and records what it cost. It is
// off unless REELPIN_SMOKE_YOUTUBE_URL names one, and it needs yt-dlp on the
// path: a test that spends provider calls must be asked for.
//
//	REELPIN_SMOKE_YOUTUBE_URL=https://youtu.be/... go test -run Smoke ./internal/platform/youtube
func TestSmokeRealVideo(t *testing.T) {
	target := os.Getenv("REELPIN_SMOKE_YOUTUBE_URL")
	if target == "" {
		t.Skip("REELPIN_SMOKE_YOUTUBE_URL is not set")
	}

	handler := New(Deps{
		HTTP:   safehttp.New(safehttp.Config{}),
		Prober: media.NewYTDLP(nil),
		Limit:  providers.NewLimits(),
		Logger: quiet(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	started := time.Now()
	prepared, err := handler.Prepare(ctx, sourceidentity.SourceIdentity{
		NormalizedURL: target,
		OriginalURL:   target,
		Platform:      PlatformName,
		ContentType:   "video",
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Prepare(%s): %v (after %s)", target, err, elapsed)
	}

	t.Logf("smoke result: duration=%s caption_bytes=%d transcript_bytes=%d needs_media=%v",
		elapsed, len(prepared.Caption), len(prepared.PageText), prepared.NeedsMedia)
}
