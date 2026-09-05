package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/embed"
)

// runEmbeddingBackfill indexes content that has no current vector. It is
// dry-run by default and bounded per pass, so it can be run repeatedly while
// watching provider spend.
func runEmbeddingBackfill(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("backfill-embeddings", flag.ContinueOnError)
	execute := flags.Bool("execute", false, "embed instead of reporting what needs embedding")
	limit := flags.Int("limit", 500, "content versions per pass")
	if err := flags.Parse(args); err != nil {
		return err
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	indexer := embed.NewIndexer(pool, embed.NewGemini(cfg.GeminiAPIKey, 0), logger)
	report, err := indexer.Backfill(ctx, *execute, *limit)

	logger.Info("embedding backfill",
		"execute", report.Execute,
		"model", embed.Model,
		"dimension", embed.Dimension,
		"document_version", embed.DocumentVersion,
		"versions_seen", report.VersionsSeen,
		"versions_indexed", report.VersionsIndexed,
		"versions_skipped", report.VersionsSkipped,
		"chunks_indexed", report.ChunksIndexed,
		"failures", report.Failures,
	)
	return err
}
