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

// runSearchEval measures the labeled query set against one real library.
//
// A labeled set judges specific reels by URL, so it only means anything against
// a library that contains them. The bundled set judges the synthetic corpus in
// internal/search/testdata/corpus-v1.json: seed that into a dedicated
// evaluation user, or pass --set with a set labeled with the chosen user's own
// reel URLs. The command checks coverage first and refuses to spend a provider
// call per query on a library it cannot measure.
func runSearchEval(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("search-eval", flag.ContinueOnError)
	userID := flags.String("user", "", "the user whose library the labeled set is judged against")
	setPath := flags.String("set", "api/eval/search-v1.json", "path to the labeled query set")
	maxDistance := flags.Float64("max-distance", search.MaxDenseDistance, "vector arm relevance gate, for tuning runs")
	out := flags.String("out", "", "write the report as JSON to this path as well as logging it")
	minCoverage := flags.Float64("min-coverage", 0.8,
		"refuse to run unless this fraction of the set's judged reels are in the library")
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

	embedder := embed.NewGemini(embed.GeminiConfig{
		APIKey:    cfg.GeminiAPIKey,
		Model:     cfg.EmbeddingModel,
		Dimension: cfg.EmbeddingDimension,
	})
	service := search.NewService(pool, embedder, logger, time.Now)
	service.MaxDistance = *maxDistance

	present, total, err := service.Coverage(ctx, *userID, set)
	if err != nil {
		return err
	}
	coverage := 1.0
	if total > 0 {
		coverage = float64(present) / float64(total)
	}
	if coverage < *minCoverage {
		return fmt.Errorf(
			"this library holds %d of the set's %d judged reels (%.0f%%): the scores would be zero because the reels are missing, not because search failed. Seed the corpus into an evaluation user, or pass --set labelled with this user's own reel URLs",
			present, total, coverage*100)
	}
	logger.Info("labeled set coverage", "present", present, "judged", total)

	report, _, err := service.Evaluate(ctx, *userID, set)
	if err != nil {
		return err
	}

	logger.Info("search evaluation",
		"set_version", report.SetVersion,
		"system", report.System,
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
