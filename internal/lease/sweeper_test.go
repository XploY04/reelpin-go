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
	}, followUp: time.Hour, wake: make(chan struct{}, 1)}
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

// A host that comes back within a lease of dying finds its dead run's lease
// still live, and a Redis without persistence has lost the marker that would
// keep the sweeper looking. One more sweep a lease later finds it expired.
func TestAStartupWithNoMarkerSweepsExactlyOnceMoreALeaseLater(t *testing.T) {
	sweeper, calls := countingSweeper(NewInflight(&fakeMarker{}, "reelpin", silent()))
	sweeper.followUp = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	if err := sweeper.Run(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("swept %d times, want the startup sweep and one follow-up", calls.Load())
	}
}

func TestAWakeSweepsWithNothingInFlight(t *testing.T) {
	sweeper, calls := countingSweeper(NewInflight(&fakeMarker{}, "reelpin", silent()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- sweeper.Run(ctx, time.Millisecond) }()

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the startup sweep never ran")
		}
		time.Sleep(time.Millisecond)
	}
	sweeper.Wake()
	for calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("a wake never swept")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
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
