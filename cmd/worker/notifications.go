package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/XploY04/reelpin-go/internal/notify"
	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/jackc/pgx/v5/pgxpool"
)

// notificationHandler turns a committed run into user notifications.
//
// The envelope carries identifiers only, so the handler reads the run's
// terminal jobs from PostgreSQL and tells each subscriber once. Every send is
// keyed by its job, and the notifications table's unique event key is what
// makes that exactly-once: a redelivered message, or one message per
// subscriber, converges on one buzz per person either way.
func notificationHandler(pool *pgxpool.Pool, service *notify.Service, logger *slog.Logger) queue.Handler {
	return func(ctx context.Context, message queue.Message) (queue.Outcome, error) {
		rows, err := pool.Query(ctx, `
			SELECT j.id::text, j.user_id::text, j.status, j.failure_code,
			       j.user_save_id::text, coalesce(v.title, '')
			FROM reelpin.processing_jobs j
			LEFT JOIN reelpin.user_saves s ON s.id = j.user_save_id
			LEFT JOIN reelpin.contents c ON c.id = s.content_id
			LEFT JOIN reelpin.content_versions v ON v.id = c.current_version_id
			WHERE j.run_id = $1 AND j.status IN ('completed', 'failed', 'dead_lettered')`,
			message.RunID)
		if err != nil {
			// The run's state is not readable: retry through the queue rather
			// than dropping a notification nobody will send again.
			return queue.Outcome{Kind: queue.Retry, Attempt: 1},
				fmt.Errorf("reading the run's jobs: %w", err)
		}

		type subscriber struct {
			jobID, userID, status string
			failureCode, saveID   *string
			title                 string
		}
		subscribers := []subscriber{}
		for rows.Next() {
			var s subscriber
			if err := rows.Scan(&s.jobID, &s.userID, &s.status, &s.failureCode, &s.saveID, &s.title); err != nil {
				rows.Close()
				return queue.Outcome{Kind: queue.Retry, Attempt: 1},
					fmt.Errorf("reading a subscriber: %w", err)
			}
			subscribers = append(subscribers, s)
		}
		rows.Close()
		if rows.Err() != nil {
			return queue.Outcome{Kind: queue.Retry, Attempt: 1}, rows.Err()
		}

		var firstErr error
		for _, s := range subscribers {
			notification := notify.Notification{
				UserID:   s.userID,
				EventKey: "job:" + s.jobID,
				Type:     "processing_" + s.status,
				Data:     map[string]string{"job_id": s.jobID},
			}
			if s.status == "completed" {
				notification.Title = "Your reel is ready"
				notification.Body = s.title
				if notification.Body == "" {
					notification.Body = "Tap to open it."
				}
				if s.saveID != nil {
					notification.Target = "reel"
					notification.TargetID = *s.saveID
					notification.Data["reel_id"] = *s.saveID
				}
			} else {
				notification.Title = "That link could not be saved"
				notification.Body = "Open the app to see what happened."
				if s.failureCode != nil {
					notification.Data["failure_code"] = *s.failureCode
				}
			}

			_, err := service.SendToUser(ctx, notification)
			switch {
			case err == nil, errors.Is(err, notify.ErrNoDeviceTokens):
				// No device is not a failure: the app often registers its
				// token seconds after a first share, and the row records that
				// there was nowhere to send.
			case errors.Is(err, notify.ErrRetryable):
				if firstErr == nil {
					firstErr = err
				}
			default:
				// A provider refusal must never change job state, so it is
				// logged and the delivery is left recorded as failed.
				logger.Error("notifying a subscriber failed",
					"job_id", s.jobID, "error", err)
			}
		}

		if firstErr != nil {
			return queue.Outcome{Kind: queue.Retry, Attempt: 1}, firstErr
		}
		return queue.Outcome{Kind: queue.Done}, nil
	}
}
