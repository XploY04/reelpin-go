package providers

import (
	"context"
	"fmt"
	"sync"
)

// The concurrency the providers tolerate, inside the worker's own bounds.
// These are caps on money and on bans: Gemini bills per call, Apify runs one
// actor at a time per account, and light HTTP is bounded so a burst of cheap
// pages cannot look like a scraper.
const (
	GeminiConcurrency    = 2
	PerActorConcurrency  = 1
	LightHTTPConcurrency = 4
)

// Limits hands out slots for provider work. Every Acquire respects context
// cancellation, so a cancelled job never sits in line for a provider it will
// not call.
type Limits struct {
	gemini    chan struct{}
	lightHTTP chan struct{}

	mu     sync.Mutex
	actors map[string]chan struct{}
}

func NewLimits() *Limits {
	return &Limits{
		gemini:    make(chan struct{}, GeminiConcurrency),
		lightHTTP: make(chan struct{}, LightHTTPConcurrency),
		actors:    map[string]chan struct{}{},
	}
}

// AcquireGemini blocks until a Gemini slot is free or the context ends.
// Release exactly once.
func (l *Limits) AcquireGemini(ctx context.Context) (release func(), err error) {
	return acquire(ctx, l.gemini)
}

// AcquireLightHTTP is the slot for a cheap page or API fetch.
func (l *Limits) AcquireLightHTTP(ctx context.Context) (release func(), err error) {
	return acquire(ctx, l.lightHTTP)
}

// AcquireActor serialises calls per Apify actor. Two different actors run in
// parallel; the same actor never does.
func (l *Limits) AcquireActor(ctx context.Context, actor string) (release func(), err error) {
	l.mu.Lock()
	slot, ok := l.actors[actor]
	if !ok {
		slot = make(chan struct{}, PerActorConcurrency)
		l.actors[actor] = slot
	}
	l.mu.Unlock()
	return acquire(ctx, slot)
}

func acquire(ctx context.Context, slot chan struct{}) (func(), error) {
	select {
	case slot <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-slot }) }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for a provider slot: %w", ctx.Err())
	}
}

// InUse reports current occupancy for metrics: "gemini", "light_http", or an
// actor name. Saturation showing on a dashboard is how the caps get retuned
// with evidence instead of feel.
func (l *Limits) InUse(name string) int {
	switch name {
	case "gemini":
		return len(l.gemini)
	case "light_http":
		return len(l.lightHTTP)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if slot, ok := l.actors[name]; ok {
		return len(slot)
	}
	return 0
}
