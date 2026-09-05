//go:build integration

package ratelimit

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testLimiter(t *testing.T) *Limiter {
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
	// Each test gets its own key space, so a shared Redis is safe.
	return New(client, "reelpin:test:"+t.Name())
}

func TestBucketAllowsTheBurstThenRefuses(t *testing.T) {
	limiter := testLimiter(t)
	policy := Policy{Name: "burst", Requests: 5, Window: time.Minute}
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		decision, err := limiter.Allow(ctx, policy, "user-1")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !decision.Allowed {
			t.Fatalf("call %d was refused with %d tokens left", i, decision.Remaining)
		}
	}

	decision, err := limiter.Allow(ctx, policy, "user-1")
	if err != nil {
		t.Fatalf("sixth call: %v", err)
	}
	if decision.Allowed {
		t.Fatal("the sixth call was allowed through a limit of five")
	}
	if decision.RetryAfter < time.Second {
		t.Errorf("retry_after = %s, want at least a second", decision.RetryAfter)
	}

	// A different subject has its own bucket.
	other, err := limiter.Allow(ctx, policy, "user-2")
	if err != nil {
		t.Fatalf("other subject: %v", err)
	}
	if !other.Allowed {
		t.Fatal("one user's spending refused another user")
	}
}

func TestBucketRefillsOverTime(t *testing.T) {
	limiter := testLimiter(t)
	policy := Policy{Name: "refill", Requests: 60, Window: time.Minute}
	ctx := context.Background()

	now := time.Now()
	frozen := limiter.WithClock(func() time.Time { return now })
	for i := 0; i < 60; i++ {
		if _, err := frozen.Allow(ctx, policy, "user-1"); err != nil {
			t.Fatalf("draining: %v", err)
		}
	}
	decision, err := frozen.Allow(ctx, policy, "user-1")
	if err != nil {
		t.Fatalf("after draining: %v", err)
	}
	if decision.Allowed {
		t.Fatal("the bucket was not empty after spending every token")
	}

	// One token per second refills.
	later := limiter.WithClock(func() time.Time { return now.Add(5 * time.Second) })
	decision, err = later.Allow(ctx, policy, "user-1")
	if err != nil {
		t.Fatalf("after waiting: %v", err)
	}
	if !decision.Allowed {
		t.Fatal("the bucket did not refill")
	}
}

func TestConcurrentCallsSpendEachTokenOnce(t *testing.T) {
	limiter := testLimiter(t)
	policy := Policy{Name: "concurrent", Requests: 10, Window: time.Minute}
	ctx := context.Background()

	const callers = 40
	var wait sync.WaitGroup
	allowed := make(chan bool, callers)
	start := make(chan struct{})

	for i := 0; i < callers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			decision, err := limiter.Allow(ctx, policy, "user-1")
			if err != nil {
				allowed <- false
				return
			}
			allowed <- decision.Allowed
		}()
	}
	close(start)
	wait.Wait()
	close(allowed)

	count := 0
	for ok := range allowed {
		if ok {
			count++
		}
	}
	if count != 10 {
		t.Fatalf("%d of %d concurrent calls were allowed, want exactly 10", count, callers)
	}
}

func TestBucketExpires(t *testing.T) {
	limiter := testLimiter(t)
	policy := Policy{Name: "expiry", Requests: 2, Window: 2 * time.Second}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := limiter.Allow(ctx, policy, "user-1"); err != nil {
			t.Fatalf("draining: %v", err)
		}
	}

	ttl, err := limiter.client.PTTL(ctx, limiter.prefix+":expiry:user-1").Result()
	if err != nil {
		t.Fatalf("reading ttl: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("bucket ttl = %s, want a positive expiry so idle buckets do not accumulate", ttl)
	}
	if ttl > 30*time.Second {
		t.Errorf("bucket ttl = %s, longer than it takes to refill", ttl)
	}
}

func TestUnavailableRedisIsAnError(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	defer client.Close()

	limiter := New(client, "reelpin:test:down")
	if _, err := limiter.Allow(context.Background(), Policy{Name: "x", Requests: 1, Window: time.Minute}, "user-1"); err == nil {
		t.Fatal("an unreachable redis produced no error, so a caller could not fail closed")
	}
}
