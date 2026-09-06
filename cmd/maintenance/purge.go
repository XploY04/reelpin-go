package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"

	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/lifecycle"
)

// runPurge removes everything derived from one source and blocks it from being
// ingested again. It is the answer to a privacy or legal request, or to a
// private source that should never have been global.
//
// It is deliberately awkward to run: it needs the identity spelled out, a
// reason, and who asked for it, all of which end up in the audit row.
func runPurge(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("purge", flag.ContinueOnError)
	platform := flags.String("platform", "", "source platform, for example instagram")
	contentType := flags.String("content-type", "", "source content type, for example reel")
	contentID := flags.String("content-id", "", "the stable source id, when the platform has one")
	urlHash := flags.String("url-hash", "", "the normalized URL hash, for sources with no stable id")
	reason := flags.String("reason", "", "why this source is being purged; recorded in the audit row")
	requestedBy := flags.String("requested-by", "", "who asked for the purge; recorded in the audit row")
	execute := flags.Bool("execute", false, "actually purge; without it the command only reports what it would remove")
	if err := flags.Parse(args); err != nil {
		return err
	}

	target := lifecycle.PurgeTarget{
		Platform:    *platform,
		ContentType: *contentType,
		ContentID:   *contentID,
		URLHash:     *urlHash,
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	// ponytail: no object store is wired, so stored files are reported as
	// skipped rather than silently assumed gone; wiring one is a constructor
	// argument here once storage grows a Delete.
	purge := lifecycle.NewPurge(pool, nil, logger)

	if !*execute {
		blocked, err := purge.Blocked(ctx, target)
		if err != nil {
			return err
		}
		logger.Info("purge dry run",
			"platform", target.Platform,
			"content_type", target.ContentType,
			"already_blocklisted", blocked,
			"note", "rerun with --execute to remove the content and block the source",
		)
		return nil
	}

	if *reason == "" || *requestedBy == "" {
		return errors.New("purge --execute needs --reason and --requested-by: both are recorded in the audit row")
	}

	report, err := purge.Run(ctx, target, *reason, *requestedBy)
	if err != nil {
		return err
	}

	logger.Info("purge complete",
		"contents", report.Contents,
		"versions", report.Versions,
		"saves", report.Saves,
		"runs", report.Runs,
		"objects_deleted", report.Objects,
		"objects_skipped", report.ObjectsSkipped,
		"blocklisted", report.Blocklisted,
	)
	if report.ObjectsSkipped > 0 {
		logger.Warn("stored files were not deleted; no object store is configured",
			"objects", report.ObjectsSkipped)
	}
	return nil
}
