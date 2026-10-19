package lease

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeMarker is the in-flight key in memory. A non-nil err fails every
// command, as an unreachable Redis does.
type fakeMarker struct {
	mu     sync.Mutex
	exists bool
	ttl    time.Duration
	err    error
}

func (f *fakeMarker) Set(_ context.Context, _ string, _ any, expiration time.Duration) *redis.StatusCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return redis.NewStatusResult("", f.err)
	}
	f.exists = true
	f.ttl = expiration
	return redis.NewStatusResult("OK", nil)
}

func (f *fakeMarker) Exists(context.Context, ...string) *redis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return redis.NewIntResult(0, f.err)
	}
	if f.exists {
		return redis.NewIntResult(1, nil)
	}
	return redis.NewIntResult(0, nil)
}

func silent() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func countingSweeper(inflight *Inflight) (*Sweeper, *atomic.Int64) {
	calls := &atomic.Int64{}
	s := &Sweeper{inflight: inflight, logger: silent(), sweep: func(context.Context) (int, error) {
		calls.Add(1)
		return 0, nil
	}}
	return s, calls
}

func TestAMarkOutlivesTheLeaseItCovers(t *testing.T) {
	marker := &fakeMarker{}
	NewInflight(marker, "reelpin", silent()).mark(context.Background())

	// The sweeper has to still be looking when a dead worker's lease expires.
	if marker.ttl != 2*Duration {
		t.Errorf("marker ttl = %s, want %s", marker.ttl, 2*Duration)
	}
}

func TestWithNothingInFlightTheSweeperOnlySweepsAtStartup(t *testing.T) {
	sweeper, calls := countingSweeper(NewInflight(&fakeMarker{}, "reelpin", silent()))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	if err := sweeper.Run(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Errorf("swept %d times with no lease in flight, want only the startup sweep", calls.Load())
	}
}

func TestALeaseInFlightKeepsTheSweeperSweeping(t *testing.T) {
	marker := &fakeMarker{}
	inflight := NewInflight(marker, "reelpin", silent())
	inflight.mark(context.Background())
	sweeper, calls := countingSweeper(inflight)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	if err := sweeper.Run(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if calls.Load() < 3 {
		t.Errorf("swept %d times while a lease was in flight, want every tick", calls.Load())
	}
}

func TestAnUnreadableMarkerSweepsAnyway(t *testing.T) {
	sweeper, _ := countingSweeper(NewInflight(&fakeMarker{err: errors.New("connection refused")}, "reelpin", silent()))
	if !sweeper.due(context.Background()) {
		t.Error("an unreadable marker skipped the sweep; a dead worker's run would never resume")
	}
}

func TestWithoutRedisTheSweeperSweepsEveryTick(t *testing.T) {
	sweeper, _ := countingSweeper(NewInflight(nil, "reelpin", silent()))
	if !sweeper.due(context.Background()) {
		t.Error("no Redis skipped the sweep")
	}
	// A nil marker is what the pipeline holds without Redis.
	var inflight *Inflight
	inflight.mark(context.Background())
}
