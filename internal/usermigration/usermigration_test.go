package usermigration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

func counting(fail *atomic.Bool) (*Recorder, *sync.Map) {
	writes := &sync.Map{}
	recorder := &Recorder{
		logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		record: func(_ context.Context, userID string) error {
			if fail != nil && fail.Load() {
				return errors.New("database unavailable")
			}
			total, _ := writes.LoadOrStore(userID, new(atomic.Int64))
			total.(*atomic.Int64).Add(1)
			return nil
		},
	}
	return recorder, writes
}

func writesFor(writes *sync.Map, userID string) int64 {
	total, ok := writes.Load(userID)
	if !ok {
		return 0
	}
	return total.(*atomic.Int64).Load()
}

func TestEachUserIsWrittenOncePerProcess(t *testing.T) {
	recorder, writes := counting(nil)

	var wait sync.WaitGroup
	for range 50 {
		wait.Add(2)
		go func() { defer wait.Done(); recorder.Seen(context.Background(), userA) }()
		go func() { defer wait.Done(); recorder.Seen(context.Background(), userB) }()
	}
	wait.Wait()
	recorder.Seen(context.Background(), userA)

	if got := writesFor(writes, userA); got != 1 {
		t.Errorf("user a written %d times, want 1", got)
	}
	if got := writesFor(writes, userB); got != 1 {
		t.Errorf("user b written %d times, want 1", got)
	}
}

func TestAFailedWriteIsRetriedOnTheNextRequest(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	recorder, writes := counting(&fail)

	recorder.Seen(context.Background(), userA)
	if got := writesFor(writes, userA); got != 0 {
		t.Fatalf("a failed write counted %d", got)
	}

	fail.Store(false)
	recorder.Seen(context.Background(), userA)
	recorder.Seen(context.Background(), userA)
	if got := writesFor(writes, userA); got != 1 {
		t.Errorf("user a written %d times after the retry, want 1", got)
	}
}
