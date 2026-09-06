package youtube

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/platform/web"
	"github.com/XploY04/reelpin-go/internal/providers"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

// TestALightPageDoesNotWaitOnAMediaProbe is the handler-level half of the
// media/light split. The queue keeps the two workloads on separate consumers;
// this proves the handlers themselves do not hold a shared slot across the
// expensive part, so a slow video cannot stall an article even inside one
// process.
func TestALightPageDoesNotWaitOnAMediaProbe(t *testing.T) {
	limits := providers.NewLimits()
	client := safehttp.New(safehttp.Config{AllowPrivateAddresses: true})

	articlePage := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<html><head><meta property="og:title" content="A short article">
			<meta property="og:description" content="Read in a minute."></head><body>
			<p>The whole point is that this finishes first.</p></body></html>`))
	})
	videoPage := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(fixture(t, "watch.html")))
	})

	// The video's probe blocks until the article is done.
	articleDone := make(chan struct{})
	blocking := &blockingProber{until: articleDone}

	videoHandler := New(Deps{
		HTTP: client, Limit: limits, Logger: quiet(), Prober: blocking,
	})
	articleHandler := web.New(web.Deps{HTTP: client, Limit: limits})

	var wait sync.WaitGroup
	wait.Add(1)
	var videoErr error
	go func() {
		defer wait.Done()
		_, videoErr = videoHandler.Prepare(context.Background(), identityFor(videoPage.URL))
	}()

	// Give the video a moment to reach its probe and block there.
	time.Sleep(50 * time.Millisecond)

	finished := make(chan error, 1)
	go func() {
		_, err := articleHandler.Prepare(context.Background(), sourceidentity.SourceIdentity{
			NormalizedURL: articlePage.URL,
			OriginalURL:   articlePage.URL,
			Platform:      web.PlatformName,
			ContentType:   "link",
		})
		finished <- err
	}()

	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("the article failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(articleDone)
		wait.Wait()
		t.Fatal("the article waited on the video's probe")
	}

	close(articleDone)
	wait.Wait()
	if videoErr != nil {
		t.Fatalf("the video failed: %v", videoErr)
	}
}

// blockingProber stands in for a slow provider call.
type blockingProber struct{ until chan struct{} }

func (b *blockingProber) Probe(ctx context.Context, _ string) (int, int64, error) {
	select {
	case <-b.until:
	case <-ctx.Done():
		return 0, 0, ctx.Err()
	}
	return 60, 1 << 20, nil
}
