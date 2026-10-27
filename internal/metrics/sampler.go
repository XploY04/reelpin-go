package metrics

import (
	"context"
	"io/fs"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SampleInterval is how often the gauges are refreshed. Prometheus scrapes
// every 15 seconds by default, so anything finer is wasted queries.
const SampleInterval = 15 * time.Second

// Activity tells the samplers whether anything other than a sampler has used
// the pool since they last looked. A query the samplers send themselves would
// otherwise count as activity forever, and every sample would wake a database
// that had been allowed to suspend.
//
// The samplers in one process share one Activity: each one's queries are
// subtracted from what the others see.
type Activity struct {
	pool *pgxpool.Pool
	// acquired is the pool's lifetime acquire count. A function so tests need
	// no pool.
	acquired func() int64
	mu       sync.Mutex
	own      int64
}

func NewActivity(pool *pgxpool.Pool) *Activity {
	if pool == nil {
		return nil
	}
	return &Activity{pool: pool, acquired: func() int64 { return pool.Stat().AcquireCount() }}
}

// sampled is where one sampler's last view of the pool is kept. The zero value
// has seen nothing, so the first sample always runs.
type sampled struct {
	seen  bool
	count int64
}

// runIfUsed runs query when the pool has been used by someone other than the
// samplers since last, and reports whether it ran. The samplers take turns,
// so the acquires a query makes are attributed to the samplers and not to the
// application; one that lands concurrently from a request is attributed to
// the samplers too, and the gauge catches up on the next real use.
func (a *Activity) runIfUsed(last *sampled, query func()) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	external := a.acquired() - a.own
	if last.seen && external == last.count {
		return false
	}
	before := a.acquired()
	query()
	a.own += a.acquired() - before
	*last = sampled{seen: true, count: external}
	return true
}

// WorkerCount reports live workers. It is a function rather than an interface
// so the caller can pass whatever it already has.
type WorkerCount func(ctx context.Context) (int, error)

// Sample refreshes every gauge that has to be asked for rather than counted.
// It runs until the context is cancelled, and a failed query leaves the
// previous value in place rather than reporting a zero that looks healthy.
// The database gauges are only asked for when the pool was used since the
// last sample: an idle pool means nothing they report has changed. A receive
// on now samples at once, so a caller that has just used the pool can refresh
// the gauges without waiting for the tick; a nil channel never fires.
func Sample(ctx context.Context, m *Metrics, activity *Activity, workers WorkerCount, now <-chan struct{}) {
	if m == nil {
		return
	}
	ticker := time.NewTicker(SampleInterval)
	defer ticker.Stop()

	var last sampled
	refreshed := time.Now()
	for {
		m.sampleOnce(ctx, activity, &last, &refreshed, workers)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-now:
		}
	}
}

func (m *Metrics) sampleOnce(ctx context.Context, activity *Activity, last *sampled, refreshed *time.Time, workers WorkerCount) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if activity != nil {
		stat := activity.pool.Stat()
		m.DatabasePool.WithLabelValues("total").Set(float64(stat.TotalConns()))
		m.DatabasePool.WithLabelValues("acquired").Set(float64(stat.AcquiredConns()))
		m.DatabasePool.WithLabelValues("idle").Set(float64(stat.IdleConns()))
		m.DatabasePool.WithLabelValues("max").Set(float64(stat.MaxConns()))

		m.refreshQueues(activity, last, refreshed, time.Now(), func() bool {
			return m.sampleQueues(ctx, activity.pool)
		})
	}

	if workers != nil {
		if count, err := workers(ctx); err == nil {
			m.LiveWorkers.Set(float64(count))
		}
	}
}

// refreshQueues reads the database gauges when the pool was used and reports
// how long ago they last held a real answer. An idle pool leaves the last
// values in place, and the staleness gauge is how a reader tells those apart
// from current ones.
func (m *Metrics) refreshQueues(activity *Activity, last *sampled, refreshed *time.Time, now time.Time, query func() bool) {
	activity.runIfUsed(last, func() {
		if query() {
			*refreshed = now
		}
	})
	m.DatabaseGaugesStaleness.Set(now.Sub(*refreshed).Seconds())
}

// sampleQueues reports whether both gauges were refreshed.
func (m *Metrics) sampleQueues(ctx context.Context, pool *pgxpool.Pool) bool {
	refreshed := true
	var seconds float64
	// The oldest unpublished event: the dispatcher falling behind shows up
	// here before anything else notices.
	if err := pool.QueryRow(ctx, `
		SELECT coalesce(extract(epoch FROM now() - min(available_at)), 0)
		FROM reelpin.outbox_events
		WHERE published_at IS NULL`).Scan(&seconds); err == nil {
		m.OutboxAgeSeconds.Set(seconds)
	} else {
		refreshed = false
	}

	// Queued, not processing: this is time spent waiting for a worker.
	if err := pool.QueryRow(ctx, `
		SELECT coalesce(extract(epoch FROM now() - min(created_at)), 0)
		FROM reelpin.processing_jobs
		WHERE status = 'queued'`).Scan(&seconds); err == nil {
		m.OldestQueuedJobAge.Set(seconds)
	} else {
		refreshed = false
	}
	return refreshed
}

// MonthlySpendUSD reports provider spend so far this calendar month.
type MonthlySpendUSD func(ctx context.Context) (float64, error)

// SampleSpend keeps month-to-date provider spend on a gauge. Exactly one
// process should run it, because the number is the whole fleet's, not this
// process's. It runs whether or not a cost gate is configured: watching the
// spend is how the limits get chosen in the first place. It is read only when
// the pool was used since the last sample: spending follows a request, and a
// request uses the pool.
func SampleSpend(ctx context.Context, m *Metrics, activity *Activity, read MonthlySpendUSD) {
	if m == nil || read == nil {
		return
	}
	ticker := time.NewTicker(SampleInterval)
	defer ticker.Stop()

	var last sampled
	for {
		m.sampleSpendOnce(ctx, activity, &last, read)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Metrics) sampleSpendOnce(ctx context.Context, activity *Activity, last *sampled, read MonthlySpendUSD) {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	query := func() {
		// A failed read leaves the previous value in place: a zero here
		// would read as a month with no spending at all.
		if usd, err := read(queryCtx); err == nil {
			m.ProviderSpendMonthUSD.Set(usd)
		}
	}
	if activity == nil {
		query()
		return
	}
	activity.runIfUsed(last, query)
}

// SampleTempDisk keeps the worker's temp-directory size on a gauge. A run that
// leaks a download shows up here long before the disk fills.
func SampleTempDisk(ctx context.Context, m *Metrics, root string) {
	if m == nil || root == "" {
		return
	}
	ticker := time.NewTicker(SampleInterval)
	defer ticker.Stop()

	for {
		if bytes, err := directorySize(root); err == nil {
			m.TempDiskBytes.Set(float64(bytes))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A run directory removed mid-walk is normal here, not a failure.
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}
