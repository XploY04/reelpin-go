//go:build integration

package readcache

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/reels"
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
		t.Fatalf("redis is not reachable: %v", err)
	}
	return client, fmt.Sprintf("reelpin:test:%s:%d", t.Name(), time.Now().UnixNano())
}

func TestARealRedisServesHitsAndDropsThemOnABump(t *testing.T) {
	client, prefix := testClient(t)
	cache := New(client, prefix, quiet())
	inner := &countingReels{records: []reels.ReelRecord{{ID: "reel", Title: "Ramen", CreatedAt: savedAt()}}}
	reader := CachedReels(inner, cache)
	ctx := context.Background()

	for range 2 {
		records, err := reader.List(ctx, userA, reels.ListOptions{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 1 || records[0].Title != "Ramen" {
			t.Fatalf("records = %+v", records)
		}
	}
	if inner.lists != 1 {
		t.Fatalf("reads = %d, want the second served from Redis", inner.lists)
	}

	if err := cache.InvalidateUser(ctx, userA); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.List(ctx, userA, reels.ListOptions{Limit: 20}); err != nil {
		t.Fatal(err)
	}
	if inner.lists != 2 {
		t.Errorf("reads = %d after a bump, want a miss", inner.lists)
	}
}
