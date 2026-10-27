package outbox

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

// fakeFlags is the dirty flag in memory. A non-nil err fails every command,
// as an unreachable Redis does.
type fakeFlags struct {
	mu    sync.Mutex
	dirty bool
	err   error
}

func (f *fakeFlags) Set(context.Context, string, any, time.Duration) *redis.StatusCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return redis.NewStatusResult("", f.err)
	}
	f.dirty = true
	return redis.NewStatusResult("OK", nil)
}

func (f *fakeFlags) GetDel(context.Context, string) *redis.StringCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return redis.NewStringResult("", f.err)
	}
	if !f.dirty {
		return redis.NewStringResult("", redis.Nil)
	}
	f.dirty = false
	return redis.NewStringResult("1", nil)
}

func silent() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// countingDispatcher replaces the database round trip with a counter.
func countingDispatcher(signal *Signal, next *time.Duration) (*Dispatcher, *atomic.Int64) {
	calls := &atomic.Int64{}
	d := NewDispatcher(nil, nil, silent(), 10).WithSignal(signal)
	d.dispatch = func(context.Context) (int, *time.Duration, error) {
		calls.Add(1)
		return 0, next, nil
	}
	return d, calls
}

func TestAMarkedOutboxIsDispatchedOnce(t *testing.T) {
	flags := &fakeFlags{}
	signal := NewSignal(flags, "reelpin", silent())
	d, _ := countingDispatcher(signal, nil)
	ctx := context.Background()

	signal.Notify(ctx)
	if dispatch, woke := d.due(ctx, time.Time{}); !dispatch || !woke {
		t.Fatal("a marked outbox was not dispatched as a wake")
	}
	if dispatch, _ := d.due(ctx, time.Time{}); dispatch {
		t.Error("one mark was dispatched twice: taking it must clear it")
	}
}

func TestAnUnmarkedOutboxNeverReachesTheDatabase(t *testing.T) {
	d, calls := countingDispatcher(NewSignal(&fakeFlags{}, "reelpin", silent()), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := d.Run(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// The one dispatch is the startup catch-up; every tick after it asked
	// Redis and found nothing.
	if calls.Load() != 1 {
		t.Errorf("dispatched %d times with nothing marked, want only the one at startup", calls.Load())
	}
}

func TestAWriteDuringRunIsDispatched(t *testing.T) {
	signal := NewSignal(&fakeFlags{}, "reelpin", silent())
	d, calls := countingDispatcher(signal, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, time.Millisecond) }()

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the startup dispatch never ran")
		}
		time.Sleep(time.Millisecond)
	}
	signal.Notify(ctx)
	for calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("a marked outbox was never dispatched")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func TestARowDueLaterWakesTheDispatcherWhenItFallsDue(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	wait := 30 * time.Second
	d, _ := countingDispatcher(NewSignal(&fakeFlags{}, "reelpin", silent()), &wait)
	d.now = func() time.Time { return now }
	ctx := context.Background()

	nextDue := d.drain(ctx)
	if !nextDue.Equal(now.Add(wait)) {
		t.Fatalf("next due = %s, want %s", nextDue, now.Add(wait))
	}
	if dispatch, _ := d.due(ctx, nextDue); dispatch {
		t.Error("dispatched before the retry was due")
	}
	now = now.Add(wait)
	if dispatch, woke := d.due(ctx, nextDue); !dispatch || !woke {
		t.Error("a retry that fell due was never dispatched as a wake")
	}
}

func TestAFullBatchIsDrainedBeforeWaiting(t *testing.T) {
	d := NewDispatcher(nil, nil, silent(), 10)
	batches := []int{10, 10, 3}
	calls := 0
	d.dispatch = func(context.Context) (int, *time.Duration, error) {
		published := batches[calls]
		calls++
		return published, nil, nil
	}

	if next := d.drain(context.Background()); !next.IsZero() {
		t.Errorf("next due = %s with nothing waiting", next)
	}
	if calls != 3 {
		t.Errorf("dispatched %d batches, want 3: a full batch goes again at once", calls)
	}
}

func TestAFailedDispatchIsRetriedOnTheNextTick(t *testing.T) {
	d := NewDispatcher(nil, nil, silent(), 10).WithSignal(NewSignal(&fakeFlags{}, "reelpin", silent()))
	d.dispatch = func(context.Context) (int, *time.Duration, error) {
		return 0, nil, errors.New("database waking up")
	}
	ctx := context.Background()

	if dispatch, _ := d.due(ctx, d.drain(ctx)); !dispatch {
		t.Error("a failed dispatch waited for the next write to retry")
	}
}

func TestAnUnreachableRedisFallsBackToPolling(t *testing.T) {
	d, _ := countingDispatcher(NewSignal(&fakeFlags{err: errors.New("connection refused")}, "reelpin", silent()), nil)
	dispatch, woke := d.due(context.Background(), time.Time{})
	if !dispatch {
		t.Error("an unreadable signal skipped the dispatch; rows would wait forever")
	}
	if woke {
		t.Error("polling past an unreadable signal counted as a wake")
	}
}

func TestWithoutRedisTheDispatcherPollsEveryTick(t *testing.T) {
	d, calls := countingDispatcher(NewSignal(nil, "reelpin", silent()), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	if err := d.Run(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if calls.Load() < 3 {
		t.Errorf("dispatched %d times in 30 ticks without Redis, want the old polling", calls.Load())
	}
}

func TestWorkRidesOnAWakeAndNotOnAPoll(t *testing.T) {
	signal := NewSignal(&fakeFlags{}, "reelpin", silent())
	d, calls := countingDispatcher(signal, nil)
	wakes := &atomic.Int64{}
	d.WithAfterWake(func() { wakes.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, time.Millisecond) }()

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the startup dispatch never ran")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if wakes.Load() != 0 {
		t.Fatalf("after-wake ran %d times with nothing marked", wakes.Load())
	}
	signal.Notify(ctx)
	for wakes.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("a marked outbox was dispatched without the after-wake work")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if wakes.Load() != 1 {
		t.Errorf("after-wake ran %d times for one mark, want 1", wakes.Load())
	}

	polling, _ := countingDispatcher(NewSignal(nil, "reelpin", silent()), nil)
	polls := &atomic.Int64{}
	polling.WithAfterWake(func() { polls.Add(1) })
	pollCtx, pollCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer pollCancel()
	if err := polling.Run(pollCtx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if polls.Load() != 0 {
		t.Errorf("after-wake ran %d times while polling without Redis", polls.Load())
	}
}

func TestANilSignalIgnoresNotify(t *testing.T) {
	var signal *Signal
	signal.Notify(context.Background())
}
