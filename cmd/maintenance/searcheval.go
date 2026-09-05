package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/embed"
	"github.com/XploY04/reelpin-go/internal/search"
)

// runSearchEval measures the labeled query set against one real library. It
// reads only, but every query costs an embedding call, so it names the user
// explicitly rather than picking one.
func runSearchEval(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("search-eval", flag.ContinueOnError)
	userID := flags.String("user", "", "the user whose library the labeled set is judged against")
	setPath := flags.String("set", "api/eval/search-v1.json", "path to the labeled query set")
	maxDistance := flags.Float64("max-distance", search.MaxDenseDistance, "vector arm relevance gate, for tuning runs")
	out := flags.String("out", "", "write the report as JSON to this path as well as logging it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *userID == "" {
		return fmt.Errorf("search-eval needs --user")
	}

	set, err := search.LoadLabeledSet(*setPath)
	if err != nil {
		return err
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	service := search.NewService(pool, embed.NewGemini(cfg.GeminiAPIKey, 0), logger, time.Now)
	service.MaxDistance = *maxDistance

	report, _, err := service.Evaluate(ctx, *userID, set)
	if err != nil {
		return err
	}

	logger.Info("search evaluation",
		"set_version", report.SetVersion,
		"queries", report.Queries,
		"precision_at_5", report.PrecisionAt5,
		"recall_at_10", report.RecallAt10,
		"mrr", report.MRR,
		"ndcg_at_10", report.NDCGAt10,
		"zero_result_rate", report.ZeroResultRate,
		"p50", report.P50.String(),
		"p95", report.P95.String(),
		"embedding_calls", report.DenseQueries,
		"max_distance", *maxDistance,
	)

	if *out != "" {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, append(encoded, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}
