package metrics

import (
	"context"
	"testing"
)

// fakePool counts acquires the way pgxpool does: every query is one.
type fakePool struct{ acquires int64 }

func (f *fakePool) query()       { f.acquires++ }
func (f *fakePool) count() int64 { return f.acquires }
func (f *fakePool) activity() *Activity {
	return &Activity{acquired: f.count}
}

func TestTheFirstSampleAlwaysRuns(t *testing.T) {
	pool := &fakePool{}
	activity := pool.activity()
	var last sampled

	if !activity.runIfUsed(&last, pool.query) {
		t.Error("the first sample was skipped; the gauges would start empty")
	}
}

func TestAnIdlePoolIsNotQueried(t *testing.T) {
	pool := &fakePool{}
	activity := pool.activity()
	var last sampled

	activity.runIfUsed(&last, pool.query)
	for range 5 {
		if activity.runIfUsed(&last, pool.query) {
			t.Fatal("a sample ran with no use since the last one; its own query woke the database")
		}
	}
}

func TestUseSinceTheLastSampleIsSampled(t *testing.T) {
	pool := &fakePool{}
	activity := pool.activity()
	var last sampled

	activity.runIfUsed(&last, pool.query)
	pool.query() // a request
	if !activity.runIfUsed(&last, pool.query) {
		t.Error("a request since the last sample was not sampled")
	}
	if activity.runIfUsed(&last, pool.query) {
		t.Error("the sample after that counted the sampler's own query as use")
	}
}

// Two samplers on one pool must not keep each other awake: each one's queries
// are the samplers' own, not activity the other should react to.
func TestSamplersSharingAPoolDoNotWakeEachOther(t *testing.T) {
	pool := &fakePool{}
	activity := pool.activity()
	var queues, spend sampled

	activity.runIfUsed(&queues, func() { pool.query(); pool.query() })
	activity.runIfUsed(&spend, pool.query)
	for range 5 {
		if activity.runIfUsed(&queues, func() { pool.query(); pool.query() }) {
			t.Fatal("the queue sampler woke on the spend sampler's query")
		}
		if activity.runIfUsed(&spend, pool.query) {
			t.Fatal("the spend sampler woke on the queue sampler's queries")
		}
	}

	pool.query() // a request
	if !activity.runIfUsed(&queues, pool.query) || !activity.runIfUsed(&spend, pool.query) {
		t.Error("a request was not seen by both samplers")
	}
}

func TestSpendIsReadOnlyAfterUse(t *testing.T) {
	m := New()
	pool := &fakePool{}
	activity := pool.activity()
	var last sampled
	reads := 0
	read := func(context.Context) (float64, error) {
		reads++
		pool.query()
		return 1.5, nil
	}

	m.sampleSpendOnce(context.Background(), activity, &last, read)
	m.sampleSpendOnce(context.Background(), activity, &last, read)
	if reads != 1 {
		t.Errorf("spend read %d times on an idle pool, want 1", reads)
	}

	// Without a pool to watch there is nothing to gate on.
	m.sampleSpendOnce(context.Background(), nil, &last, read)
	if reads != 2 {
		t.Errorf("spend read %d times with no activity gate, want every sample", reads)
	}
}
