package metrics

import (
	"context"
	"testing"
	"time"
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

func gaugeValue(t *testing.T, m *Metrics, name string) float64 {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	for _, family := range families {
		if family.GetName() == name {
			return family.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("%s is not exported", name)
	return 0
}

// An idle pool leaves the queue gauges at their last values, so the staleness
// gauge has to keep growing until a real read lands.
func TestStalenessGrowsWhileIdleAndResetsOnARealRead(t *testing.T) {
	m := New()
	pool := &fakePool{}
	activity := pool.activity()
	var last sampled
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	refreshed := start
	read := func(ok bool) func() bool {
		return func() bool { pool.query(); return ok }
	}
	staleness := func() float64 { return gaugeValue(t, m, "reelpin_db_gauges_staleness_seconds") }

	m.refreshQueues(activity, &last, &refreshed, start, read(true))
	if staleness() != 0 {
		t.Errorf("staleness = %v right after a read, want 0", staleness())
	}

	m.refreshQueues(activity, &last, &refreshed, start.Add(45*time.Second), read(true))
	if staleness() != 45 {
		t.Errorf("staleness = %v after 45 idle seconds, want 45", staleness())
	}

	pool.query() // a dispatch
	m.refreshQueues(activity, &last, &refreshed, start.Add(60*time.Second), read(true))
	if staleness() != 0 {
		t.Errorf("staleness = %v after a read that followed use, want 0", staleness())
	}

	pool.query()
	m.refreshQueues(activity, &last, &refreshed, start.Add(90*time.Second), read(false))
	if staleness() != 30 {
		t.Errorf("staleness = %v after a failed read, want 30 since the last good one", staleness())
	}
}
