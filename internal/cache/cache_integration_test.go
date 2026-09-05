//go:build integration

package cache

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type value struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func testCache(t *testing.T) *Cache {
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
	return New(client, "reelpin:test:"+t.Name())
}

func TestValueIsCachedAndScopedToOneUser(t *testing.T) {
	responseCache := testCache(t)
	ctx := context.Background()

	var loads atomic.Int64
	load := func(context.Context) (value, error) {
		loads.Add(1)
		return value{Name: "tree", Count: 3}, nil
	}

	first, err := GetOrLoad(ctx, responseCache, "user-1", "filters", "platform=instagram", time.Minute, load)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := GetOrLoad(ctx, responseCache, "user-1", "filters", "platform=instagram", time.Minute, load)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Fatalf("cached value differs: %+v vs %+v", first, second)
	}
	if loads.Load() != 1 {
		t.Fatalf("loaded %d times, want one load and one cache hit", loads.Load())
	}

	// Another user must never read the first user's value.
	if _, err := GetOrLoad(ctx, responseCache, "user-2", "filters", "platform=instagram", time.Minute, load); err != nil {
		t.Fatalf("other user: %v", err)
	}
	if loads.Load() != 2 {
		t.Fatalf("loaded %d times, want a separate load per user", loads.Load())
	}

	// A different variant is a different key.
	if _, err := GetOrLoad(ctx, responseCache, "user-1", "filters", "platform=youtube", time.Minute, load); err != nil {
		t.Fatalf("other variant: %v", err)
	}
	if loads.Load() != 3 {
		t.Fatalf("loaded %d times, want a separate load per variant", loads.Load())
	}
}

func TestInvalidateUserDropsOnlyThatUser(t *testing.T) {
	responseCache := testCache(t)
	ctx := context.Background()

	var loads atomic.Int64
	load := func(context.Context) (value, error) {
		loads.Add(1)
		return value{Count: int(loads.Load())}, nil
	}

	if _, err := GetOrLoad(ctx, responseCache, "user-1", "stats", "", time.Minute, load); err != nil {
		t.Fatal(err)
	}
	if _, err := GetOrLoad(ctx, responseCache, "user-2", "stats", "", time.Minute, load); err != nil {
		t.Fatal(err)
	}
	if err := responseCache.InvalidateUser(ctx, "user-1"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}

	before := loads.Load()
	if _, err := GetOrLoad(ctx, responseCache, "user-1", "stats", "", time.Minute, load); err != nil {
		t.Fatal(err)
	}
	if loads.Load() != before+1 {
		t.Error("the invalidated user was served from the cache")
	}

	before = loads.Load()
	if _, err := GetOrLoad(ctx, responseCache, "user-2", "stats", "", time.Minute, load); err != nil {
		t.Fatal(err)
	}
	if loads.Load() != before {
		t.Error("invalidating one user dropped another user's value")
	}
}

func TestConcurrentMissesLoadOnce(t *testing.T) {
	responseCache := testCache(t)
	ctx := context.Background()

	var loads atomic.Int64
	load := func(context.Context) (value, error) {
		loads.Add(1)
		// Long enough that every caller is waiting on the same flight.
		time.Sleep(100 * time.Millisecond)
		return value{Name: "expensive"}, nil
	}

	var wait sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			if _, err := GetOrLoad(ctx, responseCache, "user-1", "stampede", "", time.Minute, load); err != nil {
				t.Errorf("load: %v", err)
			}
		}()
	}
	close(start)
	wait.Wait()

	if loads.Load() != 1 {
		t.Fatalf("a cold cache ran %d loads, want one", loads.Load())
	}
}

func TestRedisFailureFallsBackToTheLoader(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	defer client.Close()
	responseCache := New(client, "reelpin:test:down")

	loaded, err := GetOrLoad(context.Background(), responseCache, "user-1", "stats", "", time.Minute,
		func(context.Context) (value, error) { return value{Name: "from postgres"}, nil })
	if err != nil {
		t.Fatalf("an unreachable redis failed the read: %v", err)
	}
	if loaded.Name != "from postgres" {
		t.Fatalf("value = %+v, want the loaded one", loaded)
	}
}

func TestLoaderErrorIsReturned(t *testing.T) {
	responseCache := testCache(t)
	wanted := errors.New("database is down")

	if _, err := GetOrLoad(context.Background(), responseCache, "user-1", "stats", "", time.Minute,
		func(context.Context) (value, error) { return value{}, wanted }); !errors.Is(err, wanted) {
		t.Fatalf("err = %v, want the loader's error", err)
	}
}

func TestExpiryIsSetWithJitter(t *testing.T) {
	responseCache := testCache(t)
	ctx := context.Background()

	if _, err := GetOrLoad(ctx, responseCache, "user-1", "ttl", "", 10*time.Second,
		func(context.Context) (value, error) { return value{Name: "x"}, nil }); err != nil {
		t.Fatal(err)
	}

	key, err := responseCache.key(ctx, "user-1", "ttl", "")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	ttl, err := responseCache.client.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 0 || ttl > 11*time.Second {
		t.Fatalf("ttl = %s, want the requested ttl plus at most ten percent", ttl)
	}
}
