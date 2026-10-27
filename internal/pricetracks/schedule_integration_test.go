//go:build integration

package pricetracks

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestTheRedisClaimIsHeldByOneCallerUntilReleased(t *testing.T) {
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
		t.Fatalf("redis is not reachable: %v", err)
	}
	key := fmt.Sprintf("reelpin:test:%s:%d", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() { client.Del(ctx, key) })

	claims := NewRedisClaims(client)
	for i, want := range []bool{true, false} {
		claimed, err := claims.Claim(ctx, key, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if claimed != want {
			t.Fatalf("claim %d = %v, want %v", i+1, claimed, want)
		}
	}
	if ttl := client.TTL(ctx, key).Val(); ttl <= 0 || ttl > time.Minute {
		t.Fatalf("ttl = %s: a claim that never expires blocks every later schedule", ttl)
	}

	if err := claims.Release(ctx, key); err != nil {
		t.Fatal(err)
	}
	if claimed, err := claims.Claim(ctx, key, time.Minute); err != nil || !claimed {
		t.Fatalf("claim after release = %v, %v", claimed, err)
	}
}
