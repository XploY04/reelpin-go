package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/lifecycle"
)

// runRetention removes data that has outlived its purpose and retries any
// account deletion still waiting on its identity half. Both are scheduled work
// rather than request-path work: bulk deletes belong where they can be watched.
func runRetention(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("retention", flag.ContinueOnError)
	// Passed rather than configured: this command runs on the worker host,
	// where the operator already knows the path, and config is the API's
	// concern. It becomes a config field when the worker's own deployment does.
	workspaceRoot := flags.String("workspace-root", "",
		"where the worker keeps per-job scratch space; empty skips the disk sweep")
	skipDeletions := flags.Bool("skip-deletions", false,
		"only sweep retention, do not retry pending account deletions")
	if err := flags.Parse(args); err != nil {
		return err
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	report, err := lifecycle.NewRetention(pool, *workspaceRoot).Sweep(ctx, time.Now())
	// The report is logged even on failure: a partial sweep is still evidence.
	logger.Info("retention sweep",
		"idempotency_keys", report.IdempotencyKeys,
		"published_events", report.PublishedEvents,
		"stage_results", report.StageResults,
		"abandoned_workspaces", report.AbandonedWorkspace,
	)
	if err != nil {
		return err
	}

	if *skipDeletions {
		return nil
	}

	// A deletion whose identity half failed stays pending forever unless
	// something retries it. This is that something.
	//
	// ponytail: no auth deleter is wired yet, so these retries record the
	// failure and keep the request pending; wiring one is a config change here,
	// not a code change.
	service := lifecycle.New(pool, nil, nil, logger)
	pending, err := service.PendingRequests(ctx, 100)
	if err != nil {
		return err
	}
	for _, userID := range pending {
		report, err := service.ResumeAccountDeletion(ctx, userID)
		if err != nil {
			logger.Error("resuming an account deletion failed", "error", err)
			continue
		}
		logger.Info("resumed an account deletion",
			"data_deleted", report.DatabaseCleaned,
			"identity_deleted", report.IdentityDeleted,
			"still_pending", report.Pending,
		)
	}
	if len(pending) > 0 {
		logger.Info("account deletions retried", "count", len(pending))
	}
	return nil
}
