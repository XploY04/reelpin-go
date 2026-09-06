// Package search finds a user's saved reels.
//
// Three arms run against the same rows and are fused: dense vectors for
// meaning, full-text for exact words, and trigrams for misspellings. No arm is
// good enough alone, and each one fails differently, which is why a missing
// query vector degrades the results instead of failing the request.
package search

import (
	"strings"

	"github.com/XploY04/reelpin-go/internal/reels"
)

// RRFConstant is the k in reciprocal rank fusion. 60 is the value the original
// paper uses and the one the Python service is compared against; it flattens
// the difference between rank 1 and rank 2 enough that one arm cannot dominate.
const RRFConstant = 60.0

// MaxDenseDistance gates the vector arm so a query about something the user
// never saved comes back empty. 0.42 cosine distance is 0.58 similarity, the
// lower semantic bar the Python search already used. It is a starting point for
// the evaluation sweep, not a measured answer; see api/eval/REPORT.md.
const MaxDenseDistance = 0.42

// Limits for one query.
const (
	MinQueryRunes = 2
	MaxQueryRunes = 200
	DefaultLimit  = 5
	MaxLimit      = 20
	// CandidatesPerArm is how deep each arm reaches before fusion. Deeper costs
	// little and gives fusion something to work with.
	CandidatesPerArm = 50
)

// Arm names appear in the response's search_mode, so a support question can be
// answered with what actually ran.
const (
	ArmDense  = "dense"
	ArmSparse = "sparse"
	ArmFuzzy  = "fuzzy"
)

// Candidate is one saved reel from one arm, with its rank in that arm.
type Candidate struct {
	ReelID string
	Rank   int
}

// Result is one fused hit.
type Result struct {
	Reel              reels.DisplayReel `json:"reel"`
	RelevanceScore    float64           `json:"relevance_score"`
	RelevancePercent  int               `json:"relevance_percent"`
	DisplayScoreLabel string            `json:"display_score_label"`
}

type Response struct {
	Query      string   `json:"query"`
	SearchMode string   `json:"search_mode"`
	Total      int      `json:"total"`
	Results    []Result `json:"results"`
}

// Fuse combines the arms by reciprocal rank. A reel that several arms agree on
// rises above one that a single arm loved, which is the whole point: the arms
// are wrong in different ways.
func Fuse(arms map[string][]Candidate) []scored {
	totals := map[string]float64{}
	order := []string{}

	for _, name := range []string{ArmDense, ArmSparse, ArmFuzzy} {
		for _, candidate := range arms[name] {
			if _, seen := totals[candidate.ReelID]; !seen {
				order = append(order, candidate.ReelID)
			}
			totals[candidate.ReelID] += 1.0 / (RRFConstant + float64(candidate.Rank))
		}
	}

	fused := make([]scored, 0, len(order))
	for _, reelID := range order {
		fused = append(fused, scored{ReelID: reelID, Score: totals[reelID]})
	}

	// Sort by score, then by first-seen order, so equal scores are stable
	// rather than arbitrary between runs.
	for i := 1; i < len(fused); i++ {
		for j := i; j > 0 && fused[j].Score > fused[j-1].Score; j-- {
			fused[j], fused[j-1] = fused[j-1], fused[j]
		}
	}
	return fused
}

type scored struct {
	ReelID string
	Score  float64
}

// RelevancePercent turns a fused score into something a person can read. It is
// display-relative on purpose: the top hit anchors the scale, so the numbers
// compare results within one search and mean nothing across searches.
func RelevancePercent(score, best float64) int {
	if best <= 0 {
		return 0
	}
	percent := int((score / best) * 100)
	switch {
	case percent > 100:
		return 100
	case percent < 1:
		return 1
	}
	return percent
}

// ScoreLabel is the word next to the percentage.
func ScoreLabel(percent int) string {
	switch {
	case percent >= 85:
		return "Strong match"
	case percent >= 60:
		return "Good match"
	case percent >= 35:
		return "Possible match"
	default:
		return "Weak match"
	}
}

// Mode names which arms produced the answer, so "why did I get this" is
// answerable.
func Mode(used []string) string {
	if len(used) == 0 {
		return "empty"
	}
	return strings.Join(used, "+")
}

// NormalizeQuery trims and bounds what a user typed.
func NormalizeQuery(query string) string {
	normalized := strings.Join(strings.Fields(query), " ")
	if len([]rune(normalized)) > MaxQueryRunes {
		normalized = string([]rune(normalized)[:MaxQueryRunes])
	}
	return normalized
}
