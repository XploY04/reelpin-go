package pricetracks

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/queue"
)

// fakeClaims is SET NX in memory: a key holds until released.
type fakeClaims struct {
	mu   sync.Mutex
	keys map[string]time.Duration
	err  error
}

func newFakeClaims() *fakeClaims { return &fakeClaims{keys: map[string]time.Duration{}} }

func (f *fakeClaims) Claim(_ context.Context, key string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	if _, held := f.keys[key]; held {
		return false, nil
	}
	f.keys[key] = ttl
	return true, nil
}

func (f *fakeClaims) Release(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.keys, key)
	return nil
}

type parked struct {
	delay   time.Duration
	message queue.Message
}

type fakeDelayer struct {
	mu     sync.Mutex
	parked []parked
	err    error
}

func (f *fakeDelayer) PublishDelayed(_ context.Context, delay time.Duration, message queue.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.parked = append(f.parked, parked{delay: delay, message: message})
	return nil
}

type fakeRunner struct {
	runs int
	err  error
}

func (f *fakeRunner) CheckDue(context.Context) (Report, error) {
	f.runs++
	return Report{Checked: 1}, f.err
}

func daily(runner Runner, claims Claims, publisher DelayedPublisher, now time.Time) *Daily {
	return NewDaily(runner, claims, publisher, "test", slog.New(slog.NewJSONHandler(io.Discard, nil)),
		func() time.Time { return now })
}

func TestNextRunIsTheNextThreeOClockUTC(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	tests := []struct {
		name  string
		after time.Time
		want  time.Time
	}{
		{"before the hour", time.Date(2026, 11, 1, 2, 59, 0, 0, time.UTC), time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)},
		{"on the hour", time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)},
		{"just past the hour", time.Date(2026, 11, 1, 3, 0, 1, 0, time.UTC), time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC)},
		{"across a month", time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)},
		{"a host clock in another zone", time.Date(2026, 11, 1, 8, 31, 0, 0, ist), time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NextRun(tt.after); !got.Equal(tt.want) {
				t.Fatalf("NextRun(%s) = %s, want %s", tt.after, got, tt.want)
			}
		})
	}
}

func TestWorkersStartingTogetherParkOneRun(t *testing.T) {
	claims, publisher := newFakeClaims(), &fakeDelayer{}
	now := time.Date(2026, 11, 1, 1, 30, 0, 0, time.UTC)
	for range 3 {
		if err := daily(&fakeRunner{}, claims, publisher, now).Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}

	if len(publisher.parked) != 1 {
		t.Fatalf("parked %d runs, want 1: two workers must never both schedule a day", len(publisher.parked))
	}
	got := publisher.parked[0]
	if got.delay != 90*time.Minute {
		t.Fatalf("delay = %s, want 1h30m to 03:00 UTC", got.delay)
	}
	if got.message.EventType != queue.EventPriceCheck {
		t.Fatalf("event type = %q", got.message.EventType)
	}
	if err := got.message.Validate(); err != nil {
		t.Fatalf("the parked message would be dead-lettered on delivery: %v", err)
	}
}

func TestARunChecksAndSchedulesTomorrow(t *testing.T) {
	claims, publisher, runner := newFakeClaims(), &fakeDelayer{}, &fakeRunner{}
	start := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	if err := daily(runner, claims, publisher, start).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.runs != 0 {
		t.Fatal("starting a worker ran the check; PostgreSQL is read only when a run is delivered")
	}

	delivered := time.Date(2026, 11, 1, 3, 0, 0, 500, time.UTC)
	outcome, err := daily(runner, claims, publisher, delivered).Handle(context.Background(), publisher.parked[0].message)
	if err != nil || outcome.Kind != queue.Done {
		t.Fatalf("Handle = %+v, %v", outcome, err)
	}
	if runner.runs != 1 {
		t.Fatalf("runs = %d, want 1", runner.runs)
	}
	if len(publisher.parked) != 2 {
		t.Fatalf("parked %d runs, want today's and tomorrow's", len(publisher.parked))
	}
	if next := delivered.Add(publisher.parked[1].delay); !next.Equal(time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("the next run lands at %s, want tomorrow 03:00 UTC", next)
	}
}

func TestARunDeliveredTwiceSchedulesTomorrowOnce(t *testing.T) {
	claims, publisher, runner := newFakeClaims(), &fakeDelayer{}, &fakeRunner{}
	worker := daily(runner, claims, publisher, time.Date(2026, 11, 1, 3, 0, 2, 0, time.UTC))
	for range 2 {
		if outcome, err := worker.Handle(context.Background(), queue.Message{}); err != nil || outcome.Kind != queue.Done {
			t.Fatalf("Handle = %+v, %v", outcome, err)
		}
	}
	if len(publisher.parked) != 1 {
		t.Fatalf("parked %d runs, want 1: a redelivery must not fork the schedule", len(publisher.parked))
	}
}

func TestARunOnATrailingClockStillSchedulesTomorrow(t *testing.T) {
	claims, publisher := newFakeClaims(), &fakeDelayer{}
	if err := daily(&fakeRunner{}, claims, publisher, time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC)).Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The broker delivered at 03:00 by its clock; this worker reads 02:59:58.
	trailing := time.Date(2026, 11, 1, 2, 59, 58, 0, time.UTC)
	if _, err := daily(&fakeRunner{}, claims, publisher, trailing).Handle(context.Background(), queue.Message{}); err != nil {
		t.Fatal(err)
	}
	if len(publisher.parked) != 2 {
		t.Fatalf("parked %d runs, want 2: the run found today's claim and scheduled nothing after it", len(publisher.parked))
	}
	if next := trailing.Add(publisher.parked[1].delay); !next.Equal(time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("the next run lands at %s, want tomorrow 03:00 UTC", next)
	}
}

func TestAFailedCheckStillSchedulesTomorrow(t *testing.T) {
	claims, publisher := newFakeClaims(), &fakeDelayer{}
	outcome, err := daily(&fakeRunner{err: errFake}, claims, publisher, clock).Handle(context.Background(), queue.Message{})
	if err != nil || outcome.Kind != queue.Done {
		t.Fatalf("Handle = %+v, %v: a failed check waits for tomorrow", outcome, err)
	}
	if len(publisher.parked) != 1 {
		t.Fatal("a failed check stopped the daily schedule")
	}
}

func TestAFailedPublishReleasesTheClaim(t *testing.T) {
	claims, publisher := newFakeClaims(), &fakeDelayer{err: errFake}
	now := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	if err := daily(&fakeRunner{}, claims, publisher, now).Start(context.Background()); err == nil {
		t.Fatal("Start succeeded without parking a run")
	}
	if len(claims.keys) != 0 {
		t.Fatal("the claim outlived its failed publish, so no worker would schedule this run")
	}

	publisher.err = nil
	if err := daily(&fakeRunner{}, claims, publisher, now).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(publisher.parked) != 1 {
		t.Fatal("the next worker did not schedule the run the first one failed to")
	}
}

func TestAFailedScheduleRetriesTheDelivery(t *testing.T) {
	claims := newFakeClaims()
	claims.err = errFake
	outcome, err := daily(&fakeRunner{}, claims, &fakeDelayer{}, clock).Handle(context.Background(), queue.Message{})
	if err == nil || outcome.Kind != queue.Retry {
		t.Fatalf("Handle = %+v, %v: an unscheduled tomorrow must not be acknowledged", outcome, err)
	}
}
