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

	first := New(client, nil, prefix, "worker-a", []string{"reelpin.jobs.instagram"})
	second := New(client, nil, prefix, "worker-b", []string{"reelpin.jobs.web"})
	first.beat(ctx)
	second.beat(ctx)

	live, err := Live(ctx, client, prefix)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if live != 2 {
		t.Fatalf("live workers = %d, want 2", live)
	}

	// A heartbeat carries an expiry, so a worker that stops disappears on its
	// own rather than being counted forever.
	ttl, err := client.PTTL(ctx, first.key()).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 0 || ttl > TTL {
		t.Fatalf("heartbeat ttl = %s, want at most %s", ttl, TTL)
	}

	// A clean shutdown drops the count immediately.
	first.clear()
	live, err = Live(ctx, client, prefix)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if live != 1 {
		t.Fatalf("live workers = %d after one shut down, want 1", live)
	}
}

func TestNoRedisMeansNoHeartbeat(t *testing.T) {
	reporter := New(nil, nil, "reelpin:test", "worker-a", nil)
	// Beating without Redis must be a no-op, not a panic: the worker still runs.
	reporter.beat(context.Background())
	reporter.clear()

	if _, err := Live(context.Background(), nil, "reelpin:test"); err == nil {
		t.Fatal("counting workers without redis reported success")
	}
}
