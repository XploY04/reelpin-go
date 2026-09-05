package pipeline

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/queue"
)

// backoff is the wait before the next attempt, jittered so a provider that
// dropped many runs at once does not get them all back at the same instant.
func backoff(attempt int) time.Duration {
	stages := []time.Duration{30 * time.Second, 5 * time.Minute, 30 * time.Minute}
	index := attempt - 1
	if index < 0 {
		index = 0
	}
	if index >= len(stages) {
		index = len(stages) - 1
	}
	base := stages[index]
	return base + time.Duration(rand.Int64N(int64(base/4)))
}

// Handle runs one message and records what happened. The returned outcome is
// what the consumer does with the delivery.
func (p *Pipeline) Handle(ctx context.Context, message queue.Message) (queue.Outcome, error) {
	err := p.Process(ctx, message)
	if err == nil {
		return queue.Done, nil
	}
	if errors.Is(err, context.Canceled) {
		// Shutdown, not failure: let the message come back.
		return queue.Retry, err
	}

	failure := Classify(err)
	attempt := message.Attempt + 1

	outcome, applyErr := p.applyFailure(ctx, message, failure, attempt)
	if applyErr != nil {
		p.deps.Logger.Error("recording a failure failed",
			"run_id", message.RunID, "error", applyErr)
	}
	return outcome, failure
}

func (p *Pipeline) applyFailure(ctx context.Context, message queue.Message, failure *Failure, attempt int) (queue.Outcome, error) {
	// Shutdown must not stop the bookkeeping for work that already failed.
	ctx = context.WithoutCancel(ctx)

	var maxAttempts int
	if err := p.deps.Pool.QueryRow(ctx,
		`SELECT max_attempts FROM reelpin.processing_runs WHERE id = $1`, message.RunID,
	).Scan(&maxAttempts); err != nil {
		maxAttempts = 3
	}

	if failure.Class == ProviderExhausted {
		// The whole platform waits, not just this run: hammering a provider
		// that is refusing makes the refusal last longer.
		if _, err := p.deps.Pool.Exec(ctx, `
			INSERT INTO reelpin.provider_cooldowns
				(platform, cooldown_until, reason, source_run_id, environment)
			VALUES ($1, now() + $2::interval, $3, $4, $5)
			ON CONFLICT (platform, environment)
			DO UPDATE SET cooldown_until = GREATEST(reelpin.provider_cooldowns.cooldown_until, EXCLUDED.cooldown_until),
			              reason = EXCLUDED.reason,
			              source_run_id = EXCLUDED.source_run_id,
			              updated_at = now()`,
			message.Platform, backoff(attempt).String(), failure.Code, message.RunID, p.deps.Environment,
		); err != nil {
			return queue.Retry, fmt.Errorf("recording a provider cooldown: %w", err)
		}
	}

	terminal := failure.Terminal() || attempt >= maxAttempts
	if !terminal {
		if _, err := p.deps.Pool.Exec(ctx, `
			UPDATE reelpin.processing_runs
			SET status = 'retry_scheduled', next_retry_at = now() + $2::interval,
			    failure_code = $3, failure_message = $4,
			    lease_owner = NULL, lease_expires_at = NULL, updated_at = now()
			WHERE id = $1`,
			message.RunID, backoff(attempt).String(), failure.Code, failure.Message,
		); err != nil {
			return queue.Retry, fmt.Errorf("scheduling a retry: %w", err)
		}
		// The private jobs stay queued: from the app's point of view the work
		// is still in progress.
		if _, err := p.deps.Pool.Exec(ctx, `
			UPDATE public.processing_jobs
			SET status = 'queued', current_step = 'retry_scheduled',
			    next_retry_at = now() + $2::interval, attempt_count = attempt_count + 1,
			    updated_at = now()
			WHERE processing_run_id = $1 AND status IN ('queued', 'processing')`,
			message.RunID, backoff(attempt).String(),
		); err != nil {
			return queue.Retry, fmt.Errorf("marking jobs for retry: %w", err)
		}
		return queue.Retry, nil
	}

	status := "failed"
	if !failure.Terminal() {
		// Attempts ran out on something that was not content's fault: a person
		// should look at it.
		status = "dead_lettered"
	}

	if _, err := p.deps.Pool.Exec(ctx, `
		UPDATE reelpin.processing_runs
		SET status = $2, stage = $2, progress_percent = 100,
		    failure_code = $3, failure_message = $4,
		    lease_owner = NULL, lease_expires_at = NULL,
		    completed_at = now(), updated_at = now()
		WHERE id = $1`,
		message.RunID, status, failure.Code, failure.Message,
	); err != nil {
		return queue.DeadLetter, fmt.Errorf("recording a terminal failure: %w", err)
	}

	if _, err := p.deps.Pool.Exec(ctx, `
		UPDATE public.processing_jobs
		SET status = $2, current_step = $2, progress_percent = 100,
		    failure_code = $3, error_message = $4,
		    completed_at = now(), updated_at = now()
		WHERE processing_run_id = $1 AND status IN ('queued', 'processing')`,
		message.RunID, status, failure.Code, failure.Message,
	); err != nil {
		return queue.DeadLetter, fmt.Errorf("failing the jobs: %w", err)
	}

	// A content-terminal failure is a fact about the link, so it is worth
	// keeping: the user sees why it could not be saved, and re-sharing it does
	// not start the same doomed work again. A transient failure leaves nothing
	// behind, so a later share gets a clean run.
	if failure.Terminal() {
		if err := p.saveUnparsed(ctx, message.RunID, failure); err != nil {
			return queue.Done, err
		}
	}

	if status == "dead_lettered" {
		return queue.DeadLetter, nil
	}
	return queue.Done, nil
}

// saveUnparsed writes a placeholder reel for each waiting user, so the share
// they made is visible instead of vanishing.
func (p *Pipeline) saveUnparsed(ctx context.Context, runID string, failure *Failure) error {
	subscribers, err := p.subscribers(ctx, runID)
	if err != nil {
		return err
	}

	var normalizedURL, platformName, contentType string
	if err := p.deps.Pool.QueryRow(ctx, `
		SELECT c.normalized_url, c.source_platform, c.source_content_type
		FROM reelpin.processing_runs r
		JOIN reelpin.contents c ON c.id = r.content_id
		WHERE r.id = $1`, runID,
	).Scan(&normalizedURL, &platformName, &contentType); err != nil {
		return fmt.Errorf("reading the run's content: %w", err)
	}

	for _, subscriber := range subscribers {
		if _, err := p.deps.Pool.Exec(ctx, `
			INSERT INTO public.reels
				(user_id, url, normalized_url, source_platform, source_content_type,
				 title, summary, parse_status)
			SELECT $1, $2, $2, $3, $4, $5, $6, 'unparsed'
			WHERE NOT EXISTS (
				SELECT 1 FROM public.reels WHERE user_id = $1 AND normalized_url = $2
			)`,
			subscriber.UserID, normalizedURL, platformName, contentType,
			"Saved link", failure.Message,
		); err != nil {
			return fmt.Errorf("saving an unparsed record: %w", err)
		}
	}
	return nil
}

// SweepTempDirectories removes run directories a previous process left behind.
// It runs at startup, because a crashed worker cannot clean up after itself.
func SweepTempDirectories(root string, olderThan time.Duration, now time.Time) (int, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the temp root: %w", err)
	}

	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "run-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < olderThan {
			// It may belong to a worker that is still running.
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}
