package metrics

import (
	"context"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SampleInterval is how often the gauges are refreshed. Prometheus scrapes
// every 15 seconds by default, so anything finer is wasted queries.
const SampleInterval = 15 * time.Second

// WorkerCount reports live workers. It is a function rather than an interface
// so the caller can pass whatever it already has.
type WorkerCount func(ctx context.Context) (int, error)

// Sample refreshes every gauge that has to be asked for rather than counted.
// It runs until the context is cancelled, and a failed query leaves the
// previous value in place rather than reporting a zero that looks healthy.
func Sample(ctx context.Context, m *Metrics, pool *pgxpool.Pool, workers WorkerCount) {
	if m == nil {
		return
	}
	ticker := time.NewTicker(SampleInterval)
	defer ticker.Stop()

	for {
		m.sampleOnce(ctx, pool, workers)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Metrics) sampleOnce(ctx context.Context, pool *pgxpool.Pool, workers WorkerCount) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if pool != nil {
		stat := pool.Stat()
		m.DatabasePool.WithLabelValues("total").Set(float64(stat.TotalConns()))
		m.DatabasePool.WithLabelValues("acquired").Set(float64(stat.AcquiredConns()))
		m.DatabasePool.WithLabelValues("idle").Set(float64(stat.IdleConns()))
		m.DatabasePool.WithLabelValues("max").Set(float64(stat.MaxConns()))

		var seconds float64
		// The oldest unpublished event: the publisher falling behind shows up
		// here before anything else notices.
		if err := pool.QueryRow(ctx, `
			SELECT coalesce(extract(epoch FROM now() - min(available_at)), 0)
			FROM reelpin.outbox_events
			WHERE published_at IS NULL`).Scan(&seconds); err == nil {
			m.OutboxAgeSeconds.Set(seconds)
		}

		if err := pool.QueryRow(ctx, `
			SELECT coalesce(extract(epoch FROM now() - min(created_at)), 0)
			FROM public.processing_jobs
			WHERE status IN ('queued','pending')`).Scan(&seconds); err == nil {
			m.OldestQueuedJobAge.Set(seconds)
		}

		// Which platforms are currently being skipped. A cooldown that never
		// clears is a provider outage nobody has noticed.
		if rows, err := pool.Query(ctx, `
			SELECT platform, cooldown_until > now()
			FROM reelpin.provider_cooldowns`); err == nil {
			for rows.Next() {
				var platform string
				var active bool
				if rows.Scan(&platform, &active) != nil {
					continue
				}
				value := 0.0
				if active {
					value = 1
				}
				m.ProviderCooldown.WithLabelValues(platform).Set(value)
			}
			rows.Close()
		}
	}

	if workers != nil {
		if count, err := workers(ctx); err == nil {
			m.LiveWorkers.Set(float64(count))
		}
	}
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
			// A directory removed mid-walk is normal here, not a failure.
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
