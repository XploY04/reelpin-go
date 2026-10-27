package search

import "testing"

func TestFusePrefersWhatSeveralArmsAgreeOn(t *testing.T) {
	arms := map[string][]Candidate{
		// "agreed" is second or third everywhere; "loved" is first in one arm
		// only. Agreement should win.
		ArmDense:  {{ReelID: "loved", Rank: 1}, {ReelID: "agreed", Rank: 2}},
		ArmSparse: {{ReelID: "agreed", Rank: 1}, {ReelID: "other", Rank: 2}},
		ArmFuzzy:  {{ReelID: "agreed", Rank: 2}, {ReelID: "other", Rank: 3}},
	}

	fused := Fuse(arms)
	if len(fused) != 3 {
		t.Fatalf("fused = %d results", len(fused))
	}
	if fused[0].ReelID != "agreed" {
		t.Fatalf("first = %q, want the reel every arm found", fused[0].ReelID)
	}
	for index := 1; index < len(fused); index++ {
		if fused[index].Score > fused[index-1].Score {
			t.Fatalf("results are not ordered by score: %+v", fused)
		}
	}
}

func TestFuseDeduplicatesByReel(t *testing.T) {
	arms := map[string][]Candidate{
		ArmDense:  {{ReelID: "one", Rank: 1}},
		ArmSparse: {{ReelID: "one", Rank: 1}},
		ArmFuzzy:  {{ReelID: "one", Rank: 1}},
	}
	fused := Fuse(arms)
	if len(fused) != 1 {
		t.Fatalf("the same reel appears %d times", len(fused))
	}
	// Three first places beat one.
	single := Fuse(map[string][]Candidate{ArmDense: {{ReelID: "one", Rank: 1}}})
	if fused[0].Score <= single[0].Score {
		t.Error("agreement did not raise the score")
	}
}

func TestFuseIsStableForEqualScores(t *testing.T) {
	arms := map[string][]Candidate{
		ArmDense: {{ReelID: "a", Rank: 1}, {ReelID: "b", Rank: 1}},
	}
	first := Fuse(arms)
	for i := 0; i < 20; i++ {
		again := Fuse(arms)
		for index := range first {
			if again[index].ReelID != first[index].ReelID {
				t.Fatal("equal scores order differently between runs")
			}
		}
	}
}

func TestFuseWithOneArmStillWorks(t *testing.T) {
	// This is the degraded path: no query vector, so only words and spelling.
	fused := Fuse(map[string][]Candidate{
		ArmSparse: {{ReelID: "a", Rank: 1}, {ReelID: "b", Rank: 2}},
	})
	if len(fused) != 2 || fused[0].ReelID != "a" {
		t.Fatalf("fused = %+v", fused)
	}
}

func TestRelevanceIsDisplayRelative(t *testing.T) {
	// The top hit anchors the scale: these numbers compare results within one
	// search and mean nothing across searches.
	if got := RelevancePercent(0.05, 0.05); got != 100 {
		t.Errorf("top hit = %d, want 100", got)
	}
	if got := RelevancePercent(0.025, 0.05); got != 50 {
		t.Errorf("half = %d, want 50", got)
	}
	if got := RelevancePercent(0.0000001, 0.05); got != 1 {
		t.Errorf("tiny score = %d, want a floor of 1", got)
	}
	if got := RelevancePercent(1, 0); got != 0 {
		t.Errorf("no best score = %d, want 0", got)
	}
}

func TestScoreLabels(t *testing.T) {
	tests := []struct {
		percent int
		want    string
	}{
		{100, "Strong match"}, {85, "Strong match"},
		{84, "Good match"}, {60, "Good match"},
		{59, "Possible match"}, {35, "Possible match"},
		{34, "Weak match"}, {1, "Weak match"},
	}
	for _, tt := range tests {
		if got := ScoreLabel(tt.percent); got != tt.want {
			t.Errorf("ScoreLabel(%d) = %q, want %q", tt.percent, got, tt.want)
		}
	}
}

func TestModeNamesWhatRan(t *testing.T) {
	if got := Mode([]string{ArmDense, ArmSparse}); got != "dense+sparse" {
		t.Errorf("mode = %q", got)
	}
	if got := Mode(nil); got != "empty" {
		t.Errorf("mode = %q, want empty", got)
	}
}

func TestNormalizeQuery(t *testing.T) {
	if got := NormalizeQuery("  best   cafes  "); got != "best cafes" {
		t.Errorf("query = %q", got)
	}
	long := make([]rune, MaxQueryRunes+50)
	for i := range long {
		long[i] = 'a'
	}
	if got := NormalizeQuery(string(long)); len([]rune(got)) != MaxQueryRunes {
		t.Errorf("a long query was not bounded: %d runes", len([]rune(got)))
	}
}
