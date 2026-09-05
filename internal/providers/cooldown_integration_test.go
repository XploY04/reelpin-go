//go:build integration

package providers

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testCooldowns(t *testing.T) *Cooldowns {
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
	return NewCooldowns(client, fmt.Sprintf("reelpin:test:%s:%d", t.Name(), time.Now().UnixNano()))
}

func TestACooldownExpiresOnItsOwn(t *testing.T) {
	cooldowns := testCooldowns(t)
	ctx := context.Background()

	if err := cooldowns.Set(ctx, "instagram", "rate_limited", 200*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}

	remaining, err := cooldowns.Remaining(ctx, "instagram")
	if err != nil {
		t.Fatalf("Remaining: %v", err)
	}
	if remaining <= 0 || remaining > 200*time.Millisecond {
		t.Fatalf("remaining = %s", remaining)
	}

	time.Sleep(250 * time.Millisecond)
	remaining, err = cooldowns.Remaining(ctx, "instagram")
	if err != nil {
		t.Fatalf("Remaining after expiry: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("remaining = %s after expiry, want 0", remaining)
	}
}

func TestAShorterCooldownNeverTruncatesALongerOne(t *testing.T) {
	cooldowns := testCooldowns(t)
	ctx := context.Background()

	if err := cooldowns.Set(ctx, "apify", "outage", time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := cooldowns.Set(ctx, "apify", "rate_limited", time.Second); err != nil {
		t.Fatalf("second Set: %v", err)
	}

	remaining, err := cooldowns.Remaining(ctx, "apify")
	if err != nil {
		t.Fatalf("Remaining: %v", err)
	}
	if remaining < 30*time.Minute {
		t.Fatalf("remaining = %s: the longest known push-back must win", remaining)
	}
}

func TestAnUnknownProviderIsNotCoolingDown(t *testing.T) {
	cooldowns := testCooldowns(t)
	remaining, err := cooldowns.Remaining(context.Background(), "never-seen")
	if err != nil || remaining != 0 {
		t.Fatalf("remaining = %s, err = %v; a missing cooldown means proceed", remaining, err)
	}
}

func TestAnUnreachableRedisReturnsAnErrorNotABlock(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	defer client.Close()

	cooldowns := NewCooldowns(client, "reelpin:test:down")
	if _, err := cooldowns.Remaining(context.Background(), "instagram"); err == nil {
		t.Fatal("an unreachable redis produced no error; the caller decides to proceed, not this package")
	}
}
