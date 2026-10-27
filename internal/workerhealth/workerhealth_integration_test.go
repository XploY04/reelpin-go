//go:build integration

package workerhealth

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testClient(t *testing.T) (*redis.Client, string) {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL is not set")
	}
	options, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parsing TEST_REDIS_URL: %v", err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { client.Close() })

	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis is not reachable: %v", err)
	}
	return client, fmt.Sprintf("reelpin:test:%s:%d", t.Name(), time.Now().UnixNano())
}

func TestHeartbeatsCountLiveWorkersAndExpire(t *testing.T) {
	client, prefix := testClient(t)
	ctx := context.Background()

	first := New(client, prefix, "worker-a", []string{"reelpin.processing.media"})
	second := New(client, prefix, "worker-b", []string{"reelpin.processing.light"})
	first.beat(ctx)
	second.beat(ctx)

	live, err := Live(ctx, client, prefix)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if live != 2 {
		t.Fatalf("live workers = %d, want 2", live)
	}

	// A heartbeat expires at the stale threshold, so a worker that stops is
	// forgotten on its own rather than counted forever.
	ttl, err := client.PTTL(ctx, first.key()).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 0 || ttl > Stale {
		t.Fatalf("heartbeat ttl = %s, want at most the %s stale threshold", ttl, Stale)
	}
}

func TestAStoppedWorkerDisappearsImmediately(t *testing.T) {
	client, prefix := testClient(t)
	ctx, cancel := context.WithCancel(context.Background())

	reporter := New(client, prefix, "worker-a", nil)
	done := make(chan struct{})
	go func() {
		reporter.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		live, err := Live(context.Background(), client, prefix)
		if err == nil && live == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A clean shutdown clears the key, not waits out 90 seconds of staleness.
	cancel()
	<-done

	live, err := Live(context.Background(), client, prefix)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if live != 0 {
		t.Fatalf("live workers = %d after shutdown, want 0", live)
	}
}

func TestTheIntervalAndStaleThresholdAreThePlans(t *testing.T) {
	if Interval != 15*time.Second {
		t.Errorf("interval = %s, want the plan's 15s", Interval)
	}
	if Stale != 90*time.Second {
		t.Errorf("stale = %s, want the plan's 90s", Stale)
	}
}
