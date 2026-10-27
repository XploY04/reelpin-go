//go:build integration

package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
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
	// A fresh key space per test and per run: a leftover bucket would silently
	// change what these tests measure.
	return client, fmt.Sprintf("reelpin:test:%s:%d", t.Name(), time.Now().UnixNano())
}

func testLimiter(t *testing.T) *Limiter {
	t.Helper()
	client, prefix := testClient(t)
	return New(client, prefix, NewHasher("test-salt"))
}

func TestBucketAllowsTheBurstThenRefuses(t *testing.T) {
	limiter := testLimiter(t)
	policy := Policy{Name: "burst", Requests: 5, Window: time.Minute}
	ctx := context.Background()

	// The boundary matters on both sides: the fifth call is the last allowed,
	// the sixth is the first refused.
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

	key := limiter.prefix + ":expiry:" + limiter.Hash("user-1")
	ttl, err := limiter.client.PTTL(ctx, key).Result()
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

func TestKeysNeverCarryTheRawSubject(t *testing.T) {
	limiter := testLimiter(t)
	ctx := context.Background()

	// A user id and a client IP: the two subjects the plan says must never
	// appear raw in Redis.
	subjects := []string{"11111111-1111-4111-8111-111111111111", "203.0.113.7"}
	for _, subject := range subjects {
		if _, err := limiter.Allow(ctx, Submission, subject); err != nil {
			t.Fatalf("spending for %q: %v", subject, err)
		}
	}

	var cursor uint64
	seen := 0
	for {
		keys, next, err := limiter.client.Scan(ctx, cursor, limiter.prefix+"*", 100).Result()
		if err != nil {
			t.Fatalf("scanning: %v", err)
		}
		for _, key := range keys {
			seen++
			for _, subject := range subjects {
				if strings.Contains(key, subject) {
					t.Errorf("key %q carries the raw subject %q", key, subject)
				}
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	if seen == 0 {
		t.Fatal("no keys were written, so nothing was actually checked")
	}
}

func TestARestartLosesOnlyDisposableState(t *testing.T) {
	limiter := testLimiter(t)
	policy := Policy{Name: "restart", Requests: 2, Window: time.Hour}
	ctx := context.Background()

	// Drain the bucket, then lose every key: the stand-in for a Redis restart.
	for i := 0; i < 2; i++ {
		if _, err := limiter.Allow(ctx, policy, "user-1"); err != nil {
			t.Fatalf("draining: %v", err)
		}
	}
	if decision, _ := limiter.Allow(ctx, policy, "user-1"); decision.Allowed {
		t.Fatal("the bucket was not drained")
	}

	var cursor uint64
	for {
		keys, next, err := limiter.client.Scan(ctx, cursor, limiter.prefix+"*", 100).Result()
		if err != nil {
			t.Fatalf("scanning: %v", err)
		}
		if len(keys) > 0 {
			if err := limiter.client.Del(ctx, keys...).Err(); err != nil {
				t.Fatalf("simulating the restart: %v", err)
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}

	// The limiter keeps working from an empty store: windows reset, nothing
	// breaks, and nothing durable was here to lose.
	decision, err := limiter.Allow(ctx, policy, "user-1")
	if err != nil {
		t.Fatalf("after the restart: %v", err)
	}
	if !decision.Allowed {
		t.Fatal("a fresh store refused the first request")
	}
}

func TestUnavailableRedisIsATypedError(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	defer client.Close()

	limiter := New(client, "reelpin:test:down", NewHasher("s"))
	_, err := limiter.Allow(context.Background(), Submission, "user-1")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable so a submission handler can fail closed and a read handler can fail open", err)
	}
}
