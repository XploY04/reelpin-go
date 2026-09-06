// Command maintenance runs bounded operational tasks: schema migrations now,
// backfills and retention later. It is never part of serving traffic.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/migrations"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(logger, os.Args[1:]); err != nil {
		logger.Error("maintenance command failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: maintenance <migrate|migrate-status|migrate-down|" +
			"rebuild-queue|retention|purge|backfill-embeddings|backfill-legacy|" +
			"curate-taxonomy|rollback-taxonomy>")
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

	case "backfill-embeddings":
		return runBackfillEmbeddings(ctx, logger, cfg, args[1:])

	case "backfill-legacy":
		return runBackfillLegacy(ctx, logger, cfg, args[1:])

	case "rebuild-queue":
		return runRebuildQueue(ctx, logger, cfg, args[1:])

	case "retention":
		return runRetention(ctx, logger, cfg, args[1:])

	case "purge":
		return runPurge(ctx, logger, cfg, args[1:])

	case "curate-taxonomy":
		return runCurateTaxonomy(ctx, logger, cfg, args[1:])

	case "rollback-taxonomy":
		return runRollbackTaxonomy(ctx, logger, cfg, args[1:])

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

	default:
		return fmt.Errorf("unknown command %q", command)
	}
}
