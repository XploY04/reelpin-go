package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/lifecycle"
)

// runRetention applies the retention windows. Like every destructive command
// here it reports by default and needs --execute to remove anything.
func runRetention(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("retention", flag.ContinueOnError)
	execute := flags.Bool("execute", false, "remove expired rows instead of counting them")
	batch := flags.Int("batch", 5000, "maximum rows removed per rule per pass")
	if err := flags.Parse(args); err != nil {
		return err
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	report, err := lifecycle.Sweep(ctx, pool, *execute, *batch)

	logger.Info("retention sweep",
		"execute", report.Execute,
		"terminal_jobs", report.TerminalJobs,
		"processing_cache", report.ProcessingCache,
		"geocode_cache", report.GeocodeCache,
		"published_outbox", report.PublishedOutbox,
		"device_tokens", report.DeviceTokens,
		"service_health", report.ServiceHealth,
		"unreferenced_content", report.UnreferencedContent,
	)
	return err
}
