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
)

// runPushTokenBuilds prints how many live device tokens each platform and app
// build has, which is what a forced update needs to know before it cuts a
// build off. It only reads.
func runPushTokenBuilds(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("push-token-builds", flag.ContinueOnError)
	days := flags.Int("days", 30, "count tokens registered or refreshed in the last this many days")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *days < 1 {
		return fmt.Errorf("-days must be at least 1, got %d", *days)
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	since := time.Now().AddDate(0, 0, -*days)
	counts, err := notify.NewService(pool, nil, logger, time.Now).LiveBuilds(ctx, since)
	if err != nil {
		return err
	}
	for _, count := range counts {
		logger.Info("live push tokens",
			"platform", count.Platform, "app_build", count.AppBuild, "tokens", count.Tokens)
	}
	logger.Info("push token builds counted", "days", *days, "groups", len(counts))
	return nil
}
