package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/notify"
	"github.com/XploY04/reelpin-go/internal/postgres"
	"github.com/XploY04/reelpin-go/internal/pricetracks"
	"github.com/XploY04/reelpin-go/internal/safehttp"
)

// runCheckPriceTracks is the daily price check the timer starts. It reads the
// table only now, once a day, rather than a process sweeping it on a loop.
func runCheckPriceTracks(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("check-price-tracks", flag.ContinueOnError)
	concurrency := flags.Int("concurrency", 4, "product pages read at once")
	if err := flags.Parse(args); err != nil {
		return err
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	// Without Firebase the drop is still recorded, and the notification row
	// says there was nowhere to send it.
	notifications := notify.NewService(pool,
		notify.NewFCM(cfg.FirebaseCredentialsJSON, cfg.FirebaseProjectID, 0), logger, time.Now)
	checker := pricetracks.NewChecker(postgres.NewPriceTracks(pool),
		pricetracks.PageFetcher{Client: safehttp.New(safehttp.Config{})},
		notifications, logger, time.Now, *concurrency)

	report, err := checker.CheckDue(ctx)
	logger.Info("price check finished",
		"checked", report.Checked,
		"priced", report.Priced,
		"notified", report.Notified,
		"failed", report.Failed,
	)
	return err
}
