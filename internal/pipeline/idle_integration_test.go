//go:build integration

package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/outbox"
	"github.com/redis/go-redis/v9"
)

type recordingCache struct {
	mu    sync.Mutex
	users map[string]int
}

func (r *recordingCache) InvalidateUser(_ context.Context, userID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[userID]++
	return nil
}

func (r *recordingCache) count(userID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.users[userID]
}

// countingFlags counts how often the outbox was marked dirty.
type countingFlags struct {
	mu   sync.Mutex
	sets int
}

func (c *countingFlags) Set(context.Context, string, any, time.Duration) *redis.StatusCmd {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets++
	return redis.NewStatusResult("OK", nil)
}

func (c *countingFlags) GetDel(context.Context, string) *redis.StringCmd {
	return redis.NewStringResult("", redis.Nil)
}

func (c *countingFlags) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sets
}

func withHints(h *harness) (*recordingCache, *countingFlags) {
	cache := &recordingCache{users: map[string]int{}}
	flags := &countingFlags{}
	h.pipeline.deps.Cache = cache
	h.pipeline.deps.Outbox = outbox.NewSignal(flags, "reelpin", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	return cache, flags
}

func TestACompletedRunDropsEverySubscribersCacheAndWakesTheDispatcher(t *testing.T) {
	h := newHarness(t, userA, userB)
	cache, flags := withHints(h)

	if _, err := h.handle(t); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if status := h.runStatus(t); status != "completed" {
		t.Fatalf("run status = %q", status)
	}

	// Progress commits and the persist each change what both users see.
	for _, user := range []string{userA, userB} {
		if cache.count(user) < 2 {
			t.Errorf("user %s invalidated %d times, want every progress commit and the completion", user, cache.count(user))
		}
	}
	// The persist wrote the notification and index events.
	if flags.count() == 0 {
		t.Error("the persist wrote outbox rows and never woke the dispatcher")
	}
}

func TestAScheduledRetryDropsTheCacheAndWakesTheDispatcher(t *testing.T) {
	h := newHarness(t, userA)
	h.extractor.err = errors.New("provider keeps failing")
	cache, flags := withHints(h)

	if _, err := h.handle(t); err == nil {
		t.Fatal("the failing stage succeeded")
	}
	if status := h.runStatus(t); status != "retry_scheduled" {
		t.Fatalf("run status = %q, want retry_scheduled", status)
	}
	if cache.count(userA) == 0 {
		t.Error("the retry changed the job and left the cache alone")
	}
	if flags.count() == 0 {
		t.Error("the retry wrote a future outbox row and never told the dispatcher")
	}
}
