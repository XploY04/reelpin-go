package search

import (
	"math"
	"testing"
	"time"
)

func closeTo(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.0001 {
		t.Errorf("%s = %f, want %f", name, got, want)
	}
}

func TestScoreAPerfectRanking(t *testing.T) {
	relevant := map[string]int{"a": 3, "b": 2}
	score := Score([]string{"a", "b", "c", "d", "e"}, relevant)

	closeTo(t, "precision@5", score.PrecisionAt5, 0.4) // two of five slots
	closeTo(t, "recall@10", score.RecallAt10, 1)
	closeTo(t, "reciprocal rank", score.ReciprocalRank, 1)
	closeTo(t, "ndcg@10", score.NDCGAt10, 1)
	if score.ZeroResults {
		t.Error("a ranked list was reported as zero results")
	}
}

func TestABuriedHitScoresLower(t *testing.T) {
	relevant := map[string]int{"a": 3}
	top := Score([]string{"a", "x", "y"}, relevant)
	buried := Score([]string{"x", "y", "a"}, relevant)

	if buried.NDCGAt10 >= top.NDCGAt10 {
		t.Error("burying the right answer did not lower ndcg")
	}
	closeTo(t, "reciprocal rank", buried.ReciprocalRank, 1.0/3)
	closeTo(t, "recall@10", buried.RecallAt10, 1)
}

func TestAMissBeyondTenDoesNotCount(t *testing.T) {
	ranked := make([]string, 0, 11)
	for i := 0; i < 10; i++ {
		ranked = append(ranked, "miss")
	}
	ranked = append(ranked, "a")

	score := Score(ranked, map[string]int{"a": 3})
	closeTo(t, "recall@10", score.RecallAt10, 0)
	closeTo(t, "ndcg@10", score.NDCGAt10, 0)
	// The hit still exists, so reciprocal rank sees it.
	closeTo(t, "reciprocal rank", score.ReciprocalRank, 1.0/11)
}

func TestAnEmptyResultIsCountedAsSuch(t *testing.T) {
	score := Score(nil, map[string]int{"a": 3})
	if !score.ZeroResults {
		t.Error("an empty ranking was not counted as a zero-result query")
	}
	closeTo(t, "recall@10", score.RecallAt10, 0)
}

func TestAQueryWithNoRightAnswerScoresZeroNotNaN(t *testing.T) {
	score := Score(nil, map[string]int{})
	if math.IsNaN(score.RecallAt10) || math.IsNaN(score.NDCGAt10) {
		t.Fatalf("score = %+v, want zeros rather than NaN", score)
	}
}

func TestHigherGainsRankFirstInTheIdeal(t *testing.T) {
	relevant := map[string]int{"best": 3, "ok": 1}
	right := Score([]string{"best", "ok"}, relevant)
	wrong := Score([]string{"ok", "best"}, relevant)

	if wrong.NDCGAt10 >= right.NDCGAt10 {
		t.Error("ndcg ignored the difference between a 3 and a 1")
	}
	closeTo(t, "ideal ndcg", right.NDCGAt10, 1)
}

func TestSummarizeAveragesAndPicksPercentiles(t *testing.T) {
	scores := []QueryScore{
		{PrecisionAt5: 1, RecallAt10: 1, ReciprocalRank: 1, NDCGAt10: 1},
		{ZeroResults: true},
	}
	latencies := []time.Duration{10 * time.Millisecond, 200 * time.Millisecond}

	report := Summarize("go-hybrid", scores, latencies, 2)
	if report.SetVersion != EvalSetVersion || report.Queries != 2 || report.DenseQueries != 2 {
		t.Fatalf("report = %+v", report)
	}
	closeTo(t, "precision@5", report.PrecisionAt5, 0.5)
	closeTo(t, "mrr", report.MRR, 0.5)
	closeTo(t, "zero-result rate", report.ZeroResultRate, 0.5)
	if report.P50 != 10*time.Millisecond || report.P95 != 200*time.Millisecond {
		t.Errorf("p50 = %s p95 = %s", report.P50, report.P95)
	}
}

func TestSummarizeWithNothingMeasured(t *testing.T) {
	report := Summarize("go-hybrid", nil, nil, 0)
	if report.Queries != 0 || report.P95 != 0 || math.IsNaN(report.MRR) {
		t.Fatalf("report = %+v", report)
	}
}

func TestTheLabeledSetLoads(t *testing.T) {
	set, err := LoadLabeledSet("../../api/eval/search-v1.json")
	if err != nil {
		t.Fatalf("LoadLabeledSet: %v", err)
	}
	if set.Version != EvalSetVersion {
		t.Errorf("version = %q, want %q", set.Version, EvalSetVersion)
	}
	for _, query := range set.Queries {
		if query.ID == "" || query.Query == "" {
			t.Errorf("query %+v is missing an id or text", query)
		}
	}
}
