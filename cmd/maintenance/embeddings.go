package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/embed"
)

// runBackfillEmbeddings embeds content versions that have none, in bounded
// resumable passes. Every pass prints its checkpoint, so an interrupted run
// resumes rather than starting over and paying twice.
func runBackfillEmbeddings(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("backfill-embeddings", flag.ContinueOnError)
	after := flags.String("after", "", "resume after this content version id")
	batchSize := flags.Int("batch-size", 100, "versions per pass")
	maxBatches := flags.Int("max-batches", 0, "stop after this many passes (0 means until done)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	embedder := embed.NewGemini(embed.GeminiConfig{
		APIKey:    cfg.GeminiAPIKey,
		Model:     cfg.EmbeddingModel,
		Dimension: cfg.EmbeddingDimension,
	})
	indexer := embed.NewIndexer(pool, embedder, logger)

	totals := embed.BackfillReport{LastID: *after}
	started := time.Now()
	for pass := 0; *maxBatches == 0 || pass < *maxBatches; pass++ {
		report, err := indexer.Backfill(ctx, totals.LastID, *batchSize)
		totals.Scanned += report.Scanned
		totals.Embedded += report.Embedded
		totals.Failed += report.Failed
		totals.LastID = report.LastID

		if err != nil {
			// The checkpoint is still reported: that is what makes the next
			// run resume instead of repeating completed work.
			logger.Error("backfill stopped",
				"scanned", totals.Scanned, "embedded", totals.Embedded,
				"failed", totals.Failed, "resume_after", totals.LastID, "error", err)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		if report.Scanned == 0 {
			break
		}
	}

	logger.Info("embedding backfill complete",
		"model", embedder.Model(),
		"dimension", embedder.Dimension(),
		"document_version", embed.DocumentVersion,
		"scanned", totals.Scanned,
		"embedded", totals.Embedded,
		"failed", totals.Failed,
		"resume_after", totals.LastID,
		"took", time.Since(started).String(),
		// One provider call per embedded version: the cost of a rerun is
		// visible before anyone runs it.
		"provider_calls", totals.Embedded,
	)
	return nil
}
