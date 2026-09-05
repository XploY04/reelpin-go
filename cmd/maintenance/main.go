// Command maintenance runs bounded operational tasks: schema migrations now,
// backfills and retention later. It is never part of serving traffic.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/XploY04/reelpin-go/internal/backfill"
	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/migrations"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(logger, os.Args[1:]); err != nil {
		logger.Error("maintenance command failed", "error", err)
		os.Exit(1)
	}
}

// runBackfill is dry-run by default: it reports what it would do and writes
// nothing until --execute is passed.
func runBackfill(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("backfill-content", flag.ContinueOnError)
	execute := flags.Bool("execute", false, "write changes instead of reporting them")
	batchSize := flags.Int("batch-size", 500, "rows per keyset page")
	maxRows := flags.Int("max-rows", 0, "stop after this many reels (0 means all)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	report, err := backfill.New(pool, logger).Run(ctx, backfill.Options{
		Execute:   *execute,
		BatchSize: *batchSize,
		MaxRows:   *maxRows,
	})

	// The report is logged even on failure: a partial run is still evidence.
	logger.Info("backfill report",
		"execute", *execute,
		"version", backfill.Version,
		"reels_scanned", report.ReelsScanned,
		"unique_content", report.UniqueContent,
		"content_versions", report.ContentVersions,
		"cache_hits", report.CacheHits,
		"reels_linked", report.ReelsLinked,
		"reels_already_linked", report.ReelsAlreadyLinked,
		"invalid_urls", report.InvalidURLs,
		"conflicts", report.Conflicts,
		"failures", report.Failures,
		"jobs_scanned", report.JobsScanned,
		"jobs_linked", report.JobsLinked,
		"jobs_uncertain", report.JobsUncertain,
		"runs_created", report.RunsCreated,
	)
	return err
}

func run(logger *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: maintenance <migrate|migrate-status|migrate-down|backfill-content> [flags]")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch command := args[0]; command {
	case "migrate":
		applied, err := migrations.Up(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		logger.Info("migrations applied", "environment", cfg.Environment, "count", len(applied), "migrations", applied)
		return nil

	case "migrate-status":
		lines, err := migrations.Status(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		for _, line := range lines {
			logger.Info("migration", "state", line)
		}
		return nil

	case "migrate-down":
		// Expand-only migrations are corrected forward in production. This is
		// for a disposable database.
		if cfg.Environment == "production" {
			return errors.New("migrate-down is not allowed in production")
		}
		name, err := migrations.Down(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		logger.Info("last migration rolled back", "environment", cfg.Environment, "migration", name)
		return nil

	case "backfill-content":
		return runBackfill(ctx, logger, cfg, args[1:])

	default:
		return fmt.Errorf("unknown command %q", command)
	}
}
