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
//
// --since turns it into a delta: only legacy rows that changed after the mark
// are read, and the copy is idempotent by primary key. Every run, delta or not,
// leaves alone the rows of every user Go has already served. A finished real
// run logs the mark the next --since should use, which is --dumped-at less a
// safety margin. The tables Python changes without moving a timestamp are
// copied whole on every run, delta or not.
func runBackfillLegacy(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("backfill-legacy", flag.ContinueOnError)
	execute := flags.Bool("execute", false, "write changes instead of reporting them")
	batchSize := flags.Int("batch-size", 500, "rows per keyset page")
	maxRows := flags.Int("max-rows", 0, "stop after this many legacy reels (0 means all)")
	since := flags.String("since", "", "copy only legacy rows changed after this RFC3339 instant")
	dumpedAt := flags.String("dumped-at", "",
		"RFC3339 instant the Supabase dump was taken, read with SELECT now() on Supabase just before pg_dump; "+
			"required with --execute or --since")
	propagateDeletes := flags.Bool("propagate-deletes", false,
		"final delta only, with Python stopped: delete carried rows whose legacy row is gone, "+
			"for users Go has not served")
	legacySchema := flags.String("legacy-schema", backfill.DefaultLegacySchema,
		"schema the restored Python tables are read from")
	if err := flags.Parse(args); err != nil {
		return err
	}

	mark, err := optionalInstant("--since", *since)
	if err != nil {
		return err
	}
	dumped, err := optionalInstant("--dumped-at", *dumpedAt)
	if err != nil {
		return err
	}
	if dumped == nil && (*execute || mark != nil) {
		return errors.New("--dumped-at is required with --execute or --since: " +
			"run SELECT now() on Supabase immediately before pg_dump and pass it here")
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	started := time.Now()
	report, err := backfill.New(pool, logger).Run(ctx, backfill.Options{
		Execute:          *execute,
		BatchSize:        *batchSize,
		MaxRows:          *maxRows,
		LegacySchema:     *legacySchema,
		Since:            mark,
		DumpedAt:         dumped,
		PropagateDeletes: *propagateDeletes,
	})

	// The report is logged even on failure: a partial run is still evidence.
	logger.Info("legacy backfill report",
		"execute", *execute,
		"version", backfill.Version,
		"legacy_schema", *legacySchema,
		"since", *since,
		"dumped_at", *dumpedAt,
		"migrated_users", report.MigratedUsers,
		"deleted", report.Deleted,
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
		"conflicts_merged", report.ConflictsMerged,
		"locations_copied", report.LocationsCopied,
		"embeddings_copied", report.EmbeddingsCopied,
		"failures", report.Failures,
		"jobs_scanned", report.JobsScanned,
		"jobs_created", report.JobsCreated,
		"jobs_already_there", report.JobsAlreadyThere,
		"jobs_uncertain", report.JobsUncertain,
		"runs_created", report.RunsCreated,
		"took", time.Since(started).String(),
	)
	for name, table := range report.Tables {
		logger.Info("legacy backfill table report",
			"source_table", name,
			"absent", table.Absent,
			"scanned", table.Scanned,
			"copied", table.Copied,
			"skipped", table.Skipped,
			"skipped_by_action", table.SkippedByAction,
		)
	}
	if err == nil && report.HighWaterMark != nil {
		logger.Info("legacy backfill high-water mark",
			"next_since", report.HighWaterMark.UTC().Format(time.RFC3339),
			"note", "pass this as --since to the next delta")
	}
	if !*execute {
		logger.Info("nothing was written",
			"note", "rerun with --execute to carry the legacy rows over")
	}
	return err
}

func optionalInstant(flag, value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf("%s must be an RFC3339 instant: %w", flag, err)
	}
	return &parsed, nil
}

// runBackfillVerify judges a backfill rather than performing one. It reads the
// audit the backfill left behind against both schemas and exits non-zero unless
// every legacy row in scope is accounted for and the sampled rows agree field
// by field. It writes nothing, so it can be run as often as a rehearsal needs.
// --allow-drift keeps the report and drops the failure, for the checks taken
// while both apps are still writing.
func runBackfillVerify(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("backfill-verify", flag.ContinueOnError)
	sample := flags.Int("sample", 100, "carried-over rows per legacy table compared field by field")
	batchSize := flags.Int("batch-size", 500, "sampled rows read per query")
	legacySchema := flags.String("legacy-schema", backfill.DefaultLegacySchema,
		"schema the restored Python tables are read from")
	allowDrift := flags.Bool("allow-drift", false,
		"report mismatches but exit zero, for checks taken while both apps still write")
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
		Sample:       *sample,
		BatchSize:    *batchSize,
		LegacySchema: *legacySchema,
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
			"absent", table.Absent,
			"expected", table.Expected,
			"present", table.Present,
			"canonical_total", table.CanonicalTotal,
			"migrated", table.Migrated,
			"users_differing", table.UsersDiffering,
			"user_diffs", table.UserDiffs,
		)
	}
	logger.Info("legacy backfill search and map coverage",
		"embeddings_measured", report.Embeddings.Measured,
		"embeddings_legacy", report.Embeddings.Legacy,
		"embeddings_canonical", report.Embeddings.Canonical,
		"embedding_models", report.Embeddings.Models,
		"legacy_reel_points", report.MapPoints.LegacyReelPoints,
		"canonical_reel_points", report.MapPoints.CanonicalReelPoints,
		"legacy_manual_pins", report.MapPoints.LegacyManualPins,
		"canonical_manual_pins", report.MapPoints.CanonicalManualPins,
	)
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
		if *allowDrift {
			logger.Warn("the legacy backfill does not verify; exiting zero because --allow-drift was given")
			return nil
		}
		return errors.New("the legacy backfill does not verify: see the unexplained rows and mismatches above")
	}
	logger.Info("the legacy backfill verifies", "sample", *sample)
	return nil
}
