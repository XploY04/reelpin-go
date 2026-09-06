package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"

	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/taxonomy"
)

// runCurateTaxonomy is the weekly job. It is dry-run by default in spirit but
// not in flag: the timer runs it for real, and an operator inspecting it types
// --dry-run deliberately.
func runCurateTaxonomy(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("curate-taxonomy", flag.ContinueOnError)
	dryRun := flags.Bool("dry-run", false, "decide and report without writing anything")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if cfg.GeminiAPIKey == "" {
		return errors.New("GEMINI_API_KEY is required to curate the taxonomy")
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	curator := taxonomy.NewCurator(pool, taxonomy.NewGeminiJudge(cfg.GeminiAPIKey, ""), logger)
	report, err := curator.Curate(ctx, *dryRun)
	if err != nil {
		// A failed run changed nothing and can wait a week, so this is logged
		// as a failure of the run rather than escalated into an outage.
		return err
	}

	logger.Info("taxonomy curation finished",
		"run_id", report.RunID,
		"dry_run", report.DryRun,
		"applied", report.Applied,
		"additions", report.Additions,
		"aliases", report.Aliases,
		"rejected", report.Rejected,
		"skipped", report.Skipped,
	)
	for _, action := range report.Actions {
		logger.Info("taxonomy decision",
			"name", action.NormalizedName,
			"verdict", action.Verdict,
			"confidence", action.Confidence,
			"applied", action.Applied,
			"skipped", action.Skipped,
		)
	}
	return nil
}

// runRollbackTaxonomy undoes one run and leaves its record behind.
func runRollbackTaxonomy(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("rollback-taxonomy", flag.ContinueOnError)
	runID := flags.String("run-id", "", "the taxonomy run to undo")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *runID == "" {
		return errors.New("rollback-taxonomy needs --run-id")
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	undone, err := taxonomy.NewCurator(pool, nil, logger).Rollback(ctx, *runID)
	if err != nil {
		return err
	}
	logger.Info("taxonomy run rolled back", "run_id", *runID, "actions_undone", undone)
	return nil
}
