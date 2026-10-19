package pricetracks

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// RunHour is when the daily check runs, in UTC so a host timezone change
// cannot move it into peak hours.
const RunHour = 3

// RunBudget bounds one run. Every page read has its own 15 second budget;
// this bounds the whole run.
const RunBudget = time.Hour

// CheckConcurrency is how many product pages one run reads at once.
const CheckConcurrency = 4

// runGap is how far past the end of a run the next one must be. A worker
// whose clock trails the broker's can be handed today's run a moment before
// RunHour by its own reckoning; without the gap it would look for today's run
// to schedule, find it claimed, and schedule nothing.
const runGap = time.Hour

// NextRun is the first RunHour:00 UTC at or after the given time.
func NextRun(after time.Time) time.Time {
	after = after.UTC()
	run := time.Date(after.Year(), after.Month(), after.Day(), RunHour, 0, 0, 0, time.UTC)
	if run.Before(after) {
		run = run.AddDate(0, 0, 1)
	}
	return run
}

// Claims is the dedupe between workers: exactly one claim on a key succeeds
// until it expires or is released. RedisClaims implements it.
type Claims interface {
	Claim(ctx context.Context, key string, ttl time.Duration) (bool, error)
	Release(ctx context.Context, key string) error
}

// DelayedPublisher parks a message until it is due. queue.Publisher
// implements it.
type DelayedPublisher interface {
	PublishDelayed(ctx context.Context, delay time.Duration, message queue.Message) error
}

// Runner is the check itself. Checker implements it.
type Runner interface {
	CheckDue(ctx context.Context) (Report, error)
}

// Daily runs the check once a day on the worker fleet. Each run is one
// delayed broker message; whoever schedules a run first claims its date in
// Redis, so any number of workers starting, or a run delivered twice, still
// parks one message per day. PostgreSQL is read only when a run is delivered.
type Daily struct {
	runner    Runner
	claims    Claims
	publisher DelayedPublisher
	prefix    string
	logger    *slog.Logger
	now       func() time.Time
}

func NewDaily(runner Runner, claims Claims, publisher DelayedPublisher, prefix string, logger *slog.Logger, now func() time.Time) *Daily {
	if prefix == "" {
		prefix = "reelpin"
	}
	if now == nil {
		now = time.Now
	}
	return &Daily{runner: runner, claims: claims, publisher: publisher, prefix: prefix, logger: logger, now: now}
}

// Start schedules the next run unless another worker already has. A worker
// calls it once when it starts.
func (d *Daily) Start(ctx context.Context) error {
	return d.schedule(ctx, d.now())
}

// Handle is the queue handler for a delivered run: check every stale track,
// then schedule tomorrow's run. A failed check is logged and waits for
// tomorrow, which picks up every track still stale. A failed schedule parks
// this delivery for a retry, because without it nothing would run again.
func (d *Daily) Handle(ctx context.Context, _ queue.Message) (queue.Outcome, error) {
	runCtx, cancel := context.WithTimeout(ctx, RunBudget)
	report, err := d.runner.CheckDue(runCtx)
	cancel()
	d.logger.Info("price check finished",
		"checked", report.Checked,
		"priced", report.Priced,
		"notified", report.Notified,
		"failed", report.Failed,
	)
	if err != nil {
		d.logger.Error("the price check stopped early", "error", err)
	}

	if err := d.schedule(ctx, d.now().Add(runGap)); err != nil {
		return queue.Outcome{Kind: queue.Retry, Attempt: len(queue.RetryDelays)}, err
	}
	return queue.Outcome{Kind: queue.Done}, nil
}

func (d *Daily) schedule(ctx context.Context, after time.Time) error {
	now := d.now()
	runAt := NextRun(after)
	key := d.prefix + ":price-check:scheduled:" + runAt.Format(time.DateOnly)

	// The claim outlives its run by a day, so a run delivered late or twice
	// still finds it and parks nothing more.
	claimed, err := d.claims.Claim(ctx, key, runAt.Sub(now)+24*time.Hour)
	if err != nil {
		return fmt.Errorf("claiming the next price check: %w", err)
	}
	if !claimed {
		return nil
	}

	message := queue.Message{
		EventID:       uuid.NewString(),
		SchemaVersion: queue.SchemaVersion,
		EventType:     queue.EventPriceCheck,
		CreatedAt:     now.UTC(),
	}
	if err := d.publisher.PublishDelayed(ctx, runAt.Sub(now), message); err != nil {
		// A claim with no message behind it would stop every worker from
		// scheduling this run, so it goes back.
		if release := d.claims.Release(ctx, key); release != nil {
			d.logger.Error("releasing an unscheduled price check failed", "error", release)
		}
		return fmt.Errorf("scheduling the next price check: %w", err)
	}
	d.logger.Info("scheduled the daily price check", "run_at", runAt)
	return nil
}

// RedisClaims is Claims as SET NX keys.
type RedisClaims struct {
	client redis.Cmdable
}

func NewRedisClaims(client redis.Cmdable) *RedisClaims {
	return &RedisClaims{client: client}
}

func (c *RedisClaims) Claim(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	claimed, err := c.client.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		return false, err
	}
	return claimed, nil
}

func (c *RedisClaims) Release(ctx context.Context, key string) error {
	return c.client.Del(ctx, key).Err()
}
