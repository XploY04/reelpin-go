package providers

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stress hammers one acquire function and reports the highest concurrency the
// cap ever allowed.
func stress(t *testing.T, callers int, acquire func(context.Context) (func(), error)) int64 {
	t.Helper()
	var current, peak atomic.Int64
	var wait sync.WaitGroup

	for i := 0; i < callers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			release, err := acquire(context.Background())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			now := current.Add(1)
			for {
				seen := peak.Load()
				if now <= seen || peak.CompareAndSwap(seen, now) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			current.Add(-1)
			release()
		}()
	}
	wait.Wait()
	return peak.Load()
}

func TestGeminiNeverExceedsTwo(t *testing.T) {
	limits := NewLimits()
	if peak := stress(t, 50, limits.AcquireGemini); peak > GeminiConcurrency {
		t.Fatalf("peak = %d, cap is %d", peak, GeminiConcurrency)
	}
	if limits.InUse("gemini") != 0 {
		t.Fatal("slots leaked")
	}
}

func TestLightHTTPNeverExceedsFour(t *testing.T) {
	limits := NewLimits()
	if peak := stress(t, 50, limits.AcquireLightHTTP); peak > LightHTTPConcurrency {
		t.Fatalf("peak = %d, cap is %d", peak, LightHTTPConcurrency)
	}
}

func TestOneActorRunsOneCallAtATime(t *testing.T) {
	limits := NewLimits()
	acquire := func(ctx context.Context) (func(), error) {
		return limits.AcquireActor(ctx, "apify~instagram-scraper")
	}
	if peak := stress(t, 20, acquire); peak > PerActorConcurrency {
		t.Fatalf("peak = %d, the same actor ran twice at once", peak)
	}
}

func TestDifferentActorsRunInParallel(t *testing.T) {
	limits := NewLimits()

	releaseA, err := limits.AcquireActor(context.Background(), "actor-a")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA()

	// A second actor is not behind the first one's slot.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	releaseB, err := limits.AcquireActor(ctx, "actor-b")
	if err != nil {
		t.Fatalf("actor-b waited on actor-a: %v", err)
	}
	releaseB()
}

func TestACancelledJobStopsWaiting(t *testing.T) {
	limits := NewLimits()
	holds := make([]func(), 0, GeminiConcurrency)
	for i := 0; i < GeminiConcurrency; i++ {
		release, err := limits.AcquireGemini(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		holds = append(holds, release)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := limits.AcquireGemini(ctx); err == nil {
		t.Fatal("a full pool granted a slot")
	}

	for _, release := range holds {
		release()
	}
}

func TestReleaseTwiceFreesOneSlot(t *testing.T) {
	limits := NewLimits()
	release, err := limits.AcquireGemini(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	release() // a deferred release racing an explicit one must not free a stranger's slot

	if limits.InUse("gemini") != 0 {
		t.Fatal("occupancy went negative or a slot leaked")
	}
	// Both slots are still grantable.
	for i := 0; i < GeminiConcurrency; i++ {
		r, err := limits.AcquireGemini(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer r()
	}
}

func TestInUseReportsOccupancy(t *testing.T) {
	limits := NewLimits()
	release, err := limits.AcquireLightHTTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if limits.InUse("light_http") != 1 {
		t.Fatalf("in use = %d", limits.InUse("light_http"))
	}
	release()
	if limits.InUse("light_http") != 0 {
		t.Fatalf("in use after release = %d", limits.InUse("light_http"))
	}
	if limits.InUse("never-acquired-actor") != 0 {
		t.Fatal("an unknown name reports occupancy")
	}
}
