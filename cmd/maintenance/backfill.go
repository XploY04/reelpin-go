package main

import (
	"context"
	"errors"
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

// runBackfillVerify judges a backfill rather than performing one. It reads the
// audit the backfill left behind against both schemas and exits non-zero unless
// every legacy row in scope is accounted for and the sampled rows agree field
// by field. It writes nothing, so it can be run as often as a rehearsal needs.
func runBackfillVerify(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("backfill-verify", flag.ContinueOnError)
	sample := flags.Int("sample", 100, "carried-over rows per legacy table compared field by field")
	batchSize := flags.Int("batch-size", 500, "sampled rows read per query")
	if err := flags.Parse(args); err != nil {
		return err
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	started := time.Now()
	report, err := backfill.NewVerifier(pool).Verify(ctx, backfill.VerifyOptions{
		Sample:    *sample,
		BatchSize: *batchSize,
	})
	if err != nil {
		return err
	}

	for _, table := range report.Tables {
		logger.Info("legacy backfill verification",
			"version", backfill.Version,
			"source_table", table.SourceTable,
			"in_scope", table.InScope,
			"carried", table.Carried,
			"skipped", table.Skipped,
			"skipped_by_action", table.SkippedByAction,
			"unexplained", table.Unexplained,
			"unexplained_ids", table.UnexplainedIDs,
			"carried_but_missing", table.CarriedButMissing,
			"missing_ids", table.MissingIDs,
		)
	}
	logger.Info("legacy backfill field comparison",
		"compared", report.Sample.Compared,
		"text_not_comparable", report.Sample.TextNotComparable,
		"mismatches", report.Sample.Mismatches,
		"examples", report.Sample.Examples,
		"took", time.Since(started).String(),
	)

	// A run that compared nothing passes every check it made, which is not the
	// same as passing, and a runbook reading only the exit code cannot tell.
	if report.Sample.Compared == 0 {
		logger.Warn("no carried-over row was compared; the field check proves nothing")
	}
	if !report.OK() {
		return errors.New("the legacy backfill does not verify: see the unexplained rows and mismatches above")
	}
	logger.Info("the legacy backfill verifies", "sample", *sample)
	return nil
}
