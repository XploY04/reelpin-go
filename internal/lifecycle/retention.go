package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Retention windows. Each one answers "how long is this still useful?", not
// "how long can we keep it".
const (
	// TerminalJobRetention: a finished job is history the app stops polling.
	TerminalJobRetention = 90 * 24 * time.Hour
	// ProcessingCacheRetention: the global content model has replaced it, and
	// the backfill has already read it.
	ProcessingCacheRetention = 180 * 24 * time.Hour
	// GeocodeCacheRetention: places move, and a two-year-old answer is worth
	// re-asking once.
	GeocodeCacheRetention = 365 * 24 * time.Hour
	// PublishedOutboxRetention: a published event is only useful for a short
	// audit window.
	PublishedOutboxRetention = 14 * 24 * time.Hour
	// DeviceTokenRetention: a device that has not been seen in this long is
	// gone, and notifying it wastes provider quota.
	DeviceTokenRetention = 180 * 24 * time.Hour
	// ServiceHealthRetention keeps the aggregate rows from growing forever.
	ServiceHealthRetention = 30 * 24 * time.Hour
)

// RetentionReport counts what a sweep removed. A dry run fills the same fields
// with what it would have removed.
type RetentionReport struct {
	Execute             bool `json:"execute"`
	TerminalJobs        int  `json:"terminal_jobs"`
	ProcessingCache     int  `json:"processing_cache"`
	GeocodeCache        int  `json:"geocode_cache"`
	PublishedOutbox     int  `json:"published_outbox"`
	DeviceTokens        int  `json:"device_tokens"`
	ServiceHealth       int  `json:"service_health"`
	UnreferencedContent int  `json:"unreferenced_content"`
	ReclaimedLeases     int  `json:"reclaimed_leases"`
}

// Sweep applies every retention rule. It is dry-run by default, and each rule
// is bounded so one pass cannot lock a table for minutes.
func Sweep(ctx context.Context, pool *pgxpool.Pool, execute bool, batch int) (RetentionReport, error) {
	if batch <= 0 {
		batch = 5_000
	}
	report := RetentionReport{Execute: execute}

	rules := []struct {
		target *int
		count  string
		delete string
		age    time.Duration
	}{
		{
			target: &report.TerminalJobs,
			count: `SELECT count(*) FROM public.processing_jobs
			        WHERE status IN ('completed','failed','dead_lettered') AND created_at < $1`,
			delete: `DELETE FROM public.processing_jobs WHERE id IN (
			           SELECT id FROM public.processing_jobs
			           WHERE status IN ('completed','failed','dead_lettered') AND created_at < $1
			           LIMIT $2)`,
			age: TerminalJobRetention,
		},
		{
			target: &report.ProcessingCache,
			count:  `SELECT count(*) FROM public.processing_cache WHERE updated_at < $1`,
			delete: `DELETE FROM public.processing_cache WHERE id IN (
			           SELECT id FROM public.processing_cache WHERE updated_at < $1 LIMIT $2)`,
			age: ProcessingCacheRetention,
		},
		{
			target: &report.GeocodeCache,
			count:  `SELECT count(*) FROM public.geocode_cache WHERE updated_at < $1`,
			delete: `DELETE FROM public.geocode_cache WHERE query_key IN (
			           SELECT query_key FROM public.geocode_cache WHERE updated_at < $1 LIMIT $2)`,
			age: GeocodeCacheRetention,
		},
		{
			target: &report.PublishedOutbox,
			count:  `SELECT count(*) FROM reelpin.outbox_events WHERE published_at IS NOT NULL AND published_at < $1`,
			delete: `DELETE FROM reelpin.outbox_events WHERE event_id IN (
			           SELECT event_id FROM reelpin.outbox_events
			           WHERE published_at IS NOT NULL AND published_at < $1 LIMIT $2)`,
			age: PublishedOutboxRetention,
		},
		{
			target: &report.DeviceTokens,
			count:  `SELECT count(*) FROM public.device_push_tokens WHERE last_seen_at < $1`,
			delete: `DELETE FROM public.device_push_tokens WHERE id IN (
			           SELECT id FROM public.device_push_tokens WHERE last_seen_at < $1 LIMIT $2)`,
			age: DeviceTokenRetention,
		},
		{
			target: &report.ServiceHealth,
			count:  `SELECT count(*) FROM reelpin.service_health WHERE updated_at < $1`,
			delete: `DELETE FROM reelpin.service_health WHERE component IN (
			           SELECT component FROM reelpin.service_health WHERE updated_at < $1 LIMIT $2)`,
			age: ServiceHealthRetention,
		},
		{
			target: &report.UnreferencedContent,
			// Content nobody saved and nothing is processing is dead weight.
			count: `SELECT count(*) FROM reelpin.contents c
			        WHERE c.created_at < $1
			          AND NOT EXISTS (SELECT 1 FROM reelpin.content_versions v
			                          JOIN public.reels r ON r.content_version_id = v.id
			                          WHERE v.content_id = c.id)
			          AND NOT EXISTS (SELECT 1 FROM reelpin.processing_runs run
			                          WHERE run.content_id = c.id
			                            AND run.status IN ('queued','processing','retry_scheduled'))`,
			delete: `DELETE FROM reelpin.contents WHERE id IN (
			           SELECT c.id FROM reelpin.contents c
			           WHERE c.created_at < $1
			             AND NOT EXISTS (SELECT 1 FROM reelpin.content_versions v
			                             JOIN public.reels r ON r.content_version_id = v.id
			                             WHERE v.content_id = c.id)
			             AND NOT EXISTS (SELECT 1 FROM reelpin.processing_runs run
			                             WHERE run.content_id = c.id
			                               AND run.status IN ('queued','processing','retry_scheduled'))
			           LIMIT $2)`,
			age: ProcessingCacheRetention,
		},
	}

	for _, rule := range rules {
		cutoff := time.Now().UTC().Add(-rule.age)

		if !execute {
			var count int
			if err := pool.QueryRow(ctx, rule.count, cutoff).Scan(&count); err != nil {
				if isMissingTable(err) {
					continue
				}
				return report, fmt.Errorf("counting expired rows: %w", err)
			}
			*rule.target = count
			continue
		}

		tag, err := pool.Exec(ctx, rule.delete, cutoff, batch)
		if err != nil {
			if isMissingTable(err) {
				continue
			}
			return report, fmt.Errorf("removing expired rows: %w", err)
		}
		*rule.target = int(tag.RowsAffected())
	}

	return report, nil
}
