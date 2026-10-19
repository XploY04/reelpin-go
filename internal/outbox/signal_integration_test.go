//go:build integration

package outbox

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestARealRedisCarriesOneMarkPerWrite(t *testing.T) {
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
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("redis is not reachable: %v", err)
	}

	signal := NewSignal(client, fmt.Sprintf("reelpin:test:%d", time.Now().UnixNano()), quiet())
	if dirty, err := signal.take(ctx); err != nil || dirty {
		t.Fatalf("fresh signal: dirty=%v err=%v", dirty, err)
	}
	signal.Notify(ctx)
	if dirty, err := signal.take(ctx); err != nil || !dirty {
		t.Fatalf("after a write: dirty=%v err=%v", dirty, err)
	}
	if dirty, err := signal.take(ctx); err != nil || dirty {
		t.Fatalf("a taken mark was seen twice: dirty=%v err=%v", dirty, err)
	}
}
