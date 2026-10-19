package readcache

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// memoryStore is the three commands the cache uses, kept in a map. A non-nil
// err makes every command fail, as an unreachable Redis does.
type memoryStore struct {
	values map[string]string
	ttls   map[string]time.Duration
	err    error
}

func newMemoryStore() *memoryStore {
	return &memoryStore{values: map[string]string{}, ttls: map[string]time.Duration{}}
}

func (m *memoryStore) Get(ctx context.Context, key string) *redis.StringCmd {
	if m.err != nil {
		return redis.NewStringResult("", m.err)
	}
	value, ok := m.values[key]
	if !ok {
		return redis.NewStringResult("", redis.Nil)
	}
	return redis.NewStringResult(value, nil)
}

func (m *memoryStore) Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd {
	if m.err != nil {
		return redis.NewStatusResult("", m.err)
	}
	switch v := value.(type) {
	case []byte:
		m.values[key] = string(v)
	case string:
		m.values[key] = v
	}
	m.ttls[key] = expiration
	return redis.NewStatusResult("OK", nil)
}

func (m *memoryStore) Incr(ctx context.Context, key string) *redis.IntCmd {
	if m.err != nil {
		return redis.NewIntResult(0, m.err)
	}
	next, _ := strconv.ParseInt(m.values[key], 10, 64)
	next++
	m.values[key] = strconv.FormatInt(next, 10)
	return redis.NewIntResult(next, nil)
}

type countingReels struct {
	records []reels.ReelRecord
	err     error
	lists   int
	facets  int
	stats   int
	gets    int
}

func (c *countingReels) List(context.Context, string, reels.ListOptions) ([]reels.ReelRecord, error) {
	c.lists++
	return c.records, c.err
}

func (c *countingReels) Get(context.Context, string, uuid.UUID) (reels.ReelRecord, error) {
	c.gets++
	return reels.ReelRecord{}, nil
}

func (c *countingReels) Facets(context.Context, string) ([]reels.FacetRow, error) {
	c.facets++
	return []reels.FacetRow{{Count: 2}}, nil
}

func (c *countingReels) Stats(context.Context, string) (reels.LibraryStats, error) {
	c.stats++
	return reels.LibraryStats{TotalReels: 3}, nil
}

type countingJobs struct {
	lists int
}

func (c *countingJobs) List(context.Context, string, bool, int) ([]jobs.JobRecord, error) {
	c.lists++
	return []jobs.JobRecord{{ID: "job", Status: jobs.StatusQueued}}, nil
}

func (c *countingJobs) Get(context.Context, string, uuid.UUID) (jobs.JobRecord, error) {
	return jobs.JobRecord{}, nil
}

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "44444444-4444-4444-8444-444444444444"
)

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func savedAt() *time.Time {
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	return &at
}

func TestASecondReadIsServedFromTheCache(t *testing.T) {
	store := newMemoryStore()
	inner := &countingReels{records: []reels.ReelRecord{{ID: "reel", Title: "Ramen", CreatedAt: savedAt()}}}
	reader := CachedReels(inner, New(store, "reelpin", quiet()))
	ctx := context.Background()

	first, err := reader.List(ctx, userA, reels.ListOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	second, err := reader.List(ctx, userA, reels.ListOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}

	if inner.lists != 1 {
		t.Fatalf("the database was read %d times, want 1", inner.lists)
	}
	if len(second) != 1 || second[0].Title != first[0].Title || !second[0].CreatedAt.Equal(*first[0].CreatedAt) {
		t.Errorf("cached page = %+v, want what the database answered", second)
	}
	for key, ttl := range store.ttls {
		if ttl != TTL {
			t.Errorf("%s was cached for %s, want %s", key, ttl, TTL)
		}
	}
}

func TestEveryLibraryReadIsCached(t *testing.T) {
	inner := &countingReels{}
	reader := CachedReels(inner, New(newMemoryStore(), "reelpin", quiet()))
	ctx := context.Background()

	for range 2 {
		if _, err := reader.Facets(ctx, userA); err != nil {
			t.Fatal(err)
		}
		if _, err := reader.Stats(ctx, userA); err != nil {
			t.Fatal(err)
		}
		if _, err := reader.Get(ctx, userA, uuid.New()); err != nil {
			t.Fatal(err)
		}
	}
	if inner.facets != 1 || inner.stats != 1 {
		t.Errorf("facets read %d times, stats %d, want once each", inner.facets, inner.stats)
	}
	// One reel carries the transcript and is opened from a fresh list.
	if inner.gets != 2 {
		t.Errorf("a single reel was read %d times, want every time", inner.gets)
	}
}

func TestTheJobListIsCachedPerQuery(t *testing.T) {
	inner := &countingJobs{}
	reader := CachedJobs(inner, New(newMemoryStore(), "reelpin", quiet()))
	ctx := context.Background()

	for range 2 {
		if _, err := reader.List(ctx, userA, true, 20); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reader.List(ctx, userA, false, 20); err != nil {
		t.Fatal(err)
	}
	if inner.lists != 2 {
		t.Errorf("the job list was read %d times, want 2: one per distinct query", inner.lists)
	}
}

func TestDifferentPagesAndUsersNeverShareAnEntry(t *testing.T) {
	inner := &countingReels{}
	reader := CachedReels(inner, New(newMemoryStore(), "reelpin", quiet()))
	ctx := context.Background()

	cursor := &reels.Cursor{SavedAt: *savedAt(), ID: "reel"}
	for _, call := range []struct {
		user    string
		options reels.ListOptions
	}{
		{userA, reels.ListOptions{Limit: 20}},
		{userA, reels.ListOptions{Limit: 20, After: cursor}},
		{userA, reels.ListOptions{Limit: 20, Category: "Food"}},
		{userB, reels.ListOptions{Limit: 20}},
	} {
		if _, err := reader.List(ctx, call.user, call.options); err != nil {
			t.Fatal(err)
		}
	}
	if inner.lists != 4 {
		t.Errorf("four distinct reads reached the database %d times", inner.lists)
	}
}

func TestInvalidatingAUserMissesOnlyTheirEntries(t *testing.T) {
	store := newMemoryStore()
	cache := New(store, "reelpin", quiet())
	inner := &countingReels{}
	reader := CachedReels(inner, cache)
	ctx := context.Background()

	reader.List(ctx, userA, reels.ListOptions{Limit: 20})
	reader.List(ctx, userB, reels.ListOptions{Limit: 20})
	if err := cache.InvalidateUser(ctx, userA); err != nil {
		t.Fatal(err)
	}
	reader.List(ctx, userA, reels.ListOptions{Limit: 20})
	reader.List(ctx, userB, reels.ListOptions{Limit: 20})

	if inner.lists != 3 {
		t.Errorf("reads after invalidating one user = %d, want 3: only that user misses", inner.lists)
	}
	for key := range store.values {
		if strings.Contains(key, userA) || strings.Contains(key, userB) {
			t.Errorf("key %q carries a raw user id", key)
		}
	}
}

func TestAFailedReadIsNotCached(t *testing.T) {
	inner := &countingReels{err: errors.New("database down")}
	reader := CachedReels(inner, New(newMemoryStore(), "reelpin", quiet()))
	ctx := context.Background()

	for range 2 {
		if _, err := reader.List(ctx, userA, reels.ListOptions{Limit: 20}); err == nil {
			t.Fatal("the reader's error was swallowed")
		}
	}
	if inner.lists != 2 {
		t.Errorf("a failure was served from the cache: reads = %d, want 2", inner.lists)
	}
}

func TestAnUnreachableRedisReadsThrough(t *testing.T) {
	store := newMemoryStore()
	store.err = errors.New("connection refused")
	cache := New(store, "reelpin", quiet())
	inner := &countingReels{records: []reels.ReelRecord{{ID: "reel"}}}
	reader := CachedReels(inner, cache)
	ctx := context.Background()

	for range 2 {
		records, err := reader.List(ctx, userA, reels.ListOptions{Limit: 20})
		if err != nil {
			t.Fatalf("a Redis failure reached the caller: %v", err)
		}
		if len(records) != 1 {
			t.Fatalf("records = %v", records)
		}
	}
	if inner.lists != 2 {
		t.Errorf("reads = %d, want every one to reach the database", inner.lists)
	}
	if err := cache.InvalidateUser(ctx, userA); err == nil {
		t.Error("a failed bump reported success")
	}
}

func TestWithoutRedisTheReadersAreUnwrapped(t *testing.T) {
	inner := &countingReels{}
	if reader := CachedReels(inner, New(nil, "reelpin", quiet())); reader != reels.ReelReader(inner) {
		t.Error("no Redis still wrapped the reel reader")
	}
	jobsInner := &countingJobs{}
	if reader := CachedJobs(jobsInner, nil); reader != jobs.JobReader(jobsInner) {
		t.Error("no Redis still wrapped the job reader")
	}
	var cache *Cache
	if err := cache.InvalidateUser(context.Background(), userA); err != nil {
		t.Errorf("a nil cache failed to invalidate: %v", err)
	}
}
