package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"github.com/XploY04/reelpin-go/internal/backfill"
	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
)

// runBackfillLegacy copies the rows the Python service wrote into the canonical
// schema. It is dry-run by default: it reports what it would create and writes
// nothing until --execute is passed.
//
// It is safe to interrupt. Each pass records where it stopped, so a rerun
// resumes rather than starting over, and every row it already carried over is
// recognised by its preserved id.
func runBackfillLegacy(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("backfill-legacy", flag.ContinueOnError)
	execute := flags.Bool("execute", false, "write changes instead of reporting them")
	batchSize := flags.Int("batch-size", 500, "rows per keyset page")
	maxRows := flags.Int("max-rows", 0, "stop after this many legacy reels (0 means all)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	started := time.Now()
	report, err := backfill.New(pool, logger).Run(ctx, backfill.Options{
		Execute:   *execute,
		BatchSize: *batchSize,
		MaxRows:   *maxRows,
	})

	// The report is logged even on failure: a partial run is still evidence.
	logger.Info("legacy backfill report",
		"execute", *execute,
		"version", backfill.Version,
		"reels_scanned", report.ReelsScanned,
		"unique_content", report.UniqueContent,
		"content_versions", report.ContentVersions,
		"cache_hits", report.CacheHits,
		"saves_created", report.SavesCreated,
		"saves_already_there", report.SavesAlreadyThere,
		"invalid_urls", report.InvalidURLs,
		"unreadable", report.Unreadable,
		"blocklisted", report.Blocklisted,
		"conflicts", report.Conflicts,
		"failures", report.Failures,
		"jobs_scanned", report.JobsScanned,
		"jobs_created", report.JobsCreated,
		"jobs_already_there", report.JobsAlreadyThere,
		"jobs_uncertain", report.JobsUncertain,
		"runs_created", report.RunsCreated,
		"took", time.Since(started).String(),
	)
	if !*execute {
		logger.Info("nothing was written",
			"note", "rerun with --execute to carry the legacy rows over")
	}
	return err
}
