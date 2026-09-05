package search

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

// EvalSetVersion is stamped into every report so two numbers are only ever
// compared when they came from the same labeled set.
const EvalSetVersion = "search-eval-v1"

// LabeledQuery is one judged query. Relevant maps a saved reel's URL to a
// gain: 3 is exactly what the user meant, 2 is a good alternative, 1 is
// related. A URL keeps the set portable across databases, where reel ids are
// not.
type LabeledQuery struct {
	ID       string         `json:"id"`
	Query    string         `json:"query"`
	Intent   string         `json:"intent"`
	Filters  LabeledFilters `json:"filters"`
	Relevant map[string]int `json:"relevant"`
}

type LabeledFilters struct {
	Platforms   []string `json:"platforms"`
	Category    string   `json:"category"`
	Subcategory string   `json:"subcategory"`
	SavedDate   string   `json:"saved_date"`
}

type LabeledSet struct {
	Version string         `json:"version"`
	Note    string         `json:"note"`
	Queries []LabeledQuery `json:"queries"`
}

func LoadLabeledSet(path string) (LabeledSet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return LabeledSet{}, err
	}
	var set LabeledSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return LabeledSet{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(set.Queries) == 0 {
		return LabeledSet{}, fmt.Errorf("%s has no queries", path)
	}
	return set, nil
}

// QueryScore is one query's relevance, measured against its judgments.
type QueryScore struct {
	PrecisionAt5   float64
	RecallAt10     float64
	ReciprocalRank float64
	NDCGAt10       float64
	ZeroResults    bool
}

// Score measures one ranked list of keys against its judgments. Keys are
// whatever the labeled set is keyed by; only equality matters here.
func Score(ranked []string, relevant map[string]int) QueryScore {
	score := QueryScore{ZeroResults: len(ranked) == 0}

	hitsAt5 := 0
	for index, key := range ranked {
		if index >= 5 {
			break
		}
		if relevant[key] > 0 {
			hitsAt5++
		}
	}
	score.PrecisionAt5 = float64(hitsAt5) / 5

	if total := len(relevant); total > 0 {
		found := 0
		for index, key := range ranked {
			if index >= 10 {
				break
			}
			if relevant[key] > 0 {
				found++
			}
		}
		score.RecallAt10 = float64(found) / float64(total)
	}

	for index, key := range ranked {
		if relevant[key] > 0 {
			score.ReciprocalRank = 1 / float64(index+1)
			break
		}
	}

	score.NDCGAt10 = ndcgAt10(ranked, relevant)
	return score
}

func ndcgAt10(ranked []string, relevant map[string]int) float64 {
	gained := 0.0
	for index, key := range ranked {
		if index >= 10 {
			break
		}
		if gain := relevant[key]; gain > 0 {
			gained += (math.Pow(2, float64(gain)) - 1) / math.Log2(float64(index+2))
		}
	}

	// The ideal ranking puts the highest gains first.
	gains := make([]int, 0, len(relevant))
	for _, gain := range relevant {
		gains = append(gains, gain)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(gains)))

	ideal := 0.0
	for index, gain := range gains {
		if index >= 10 {
			break
		}
		ideal += (math.Pow(2, float64(gain)) - 1) / math.Log2(float64(index+2))
	}
	if ideal == 0 {
		return 0
	}
	return gained / ideal
}

// Report is what one run of the labeled set measured. Latencies are end to end
// through the search service, not the HTTP handler.
type Report struct {
	SetVersion     string        `json:"set_version"`
	System         string        `json:"system"`
	Queries        int           `json:"queries"`
	PrecisionAt5   float64       `json:"precision_at_5"`
	RecallAt10     float64       `json:"recall_at_10"`
	MRR            float64       `json:"mrr"`
	NDCGAt10       float64       `json:"ndcg_at_10"`
	ZeroResultRate float64       `json:"zero_result_rate"`
	P50            time.Duration `json:"p50"`
	P95            time.Duration `json:"p95"`
	// DenseQueries counts the queries whose vector arm ran, which is also the
	// number of embedding calls a run paid for.
	DenseQueries int `json:"dense_queries"`
}

// Summarize turns per-query scores and latencies into one report.
func Summarize(system string, scores []QueryScore, latencies []time.Duration, denseQueries int) Report {
	report := Report{
		SetVersion:   EvalSetVersion,
		System:       system,
		Queries:      len(scores),
		DenseQueries: denseQueries,
	}
	if len(scores) == 0 {
		return report
	}

	zeros := 0
	for _, score := range scores {
		report.PrecisionAt5 += score.PrecisionAt5
		report.RecallAt10 += score.RecallAt10
		report.MRR += score.ReciprocalRank
		report.NDCGAt10 += score.NDCGAt10
		if score.ZeroResults {
			zeros++
		}
	}
	count := float64(len(scores))
	report.PrecisionAt5 /= count
	report.RecallAt10 /= count
	report.MRR /= count
	report.NDCGAt10 /= count
	report.ZeroResultRate = float64(zeros) / count

	report.P50 = percentile(latencies, 50)
	report.P95 = percentile(latencies, 95)
	return report
}

func percentile(latencies []time.Duration, percent int) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), latencies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	index := (len(sorted)*percent + 99) / 100
	if index < 1 {
		index = 1
	}
	if index > len(sorted) {
		index = len(sorted)
	}
	return sorted[index-1]
}

// JudgedURLs is every reel URL the set has an opinion about.
func (set LabeledSet) JudgedURLs() []string {
	seen := map[string]bool{}
	urls := []string{}
	for _, query := range set.Queries {
		for url := range query.Relevant {
			if !seen[url] {
				seen[url] = true
				urls = append(urls, url)
			}
		}
	}
	sort.Strings(urls)
	return urls
}

// Coverage reports how many of the set's judged reels the user actually has.
// A labeled set judges specific reels, so running it against a library that
// does not contain them measures nothing and scores zero everywhere. The
// caller checks this before spending a provider call per query.
func (s *Service) Coverage(ctx context.Context, userID string, set LabeledSet) (present int, total int, err error) {
	judged := set.JudgedURLs()
	if len(judged) == 0 {
		return 0, 0, nil
	}

	err = s.pool.QueryRow(ctx, `
		SELECT count(*) FROM public.reels
		WHERE user_id = $1 AND (url = ANY($2) OR normalized_url = ANY($2))`,
		userID, judged).Scan(&present)
	if err != nil {
		return 0, len(judged), fmt.Errorf("checking the labeled set against the library: %w", err)
	}
	return present, len(judged), nil
}

// Evaluate runs the labeled set against one user's library and measures it.
// The set is keyed by reel URL, so results are mapped back before
// scoring.
func (s *Service) Evaluate(ctx context.Context, userID string, set LabeledSet) (Report, []QueryScore, error) {
	scores := make([]QueryScore, 0, len(set.Queries))
	latencies := make([]time.Duration, 0, len(set.Queries))
	dense := 0

	for _, labeled := range set.Queries {
		started := time.Now()
		response, err := s.Search(ctx, userID, labeled.Query, Filters{
			Platforms:   labeled.Filters.Platforms,
			Category:    labeled.Filters.Category,
			Subcategory: labeled.Filters.Subcategory,
			SavedDate:   labeled.Filters.SavedDate,
		}, MaxLimit)
		if err != nil {
			return Report{}, nil, fmt.Errorf("query %q: %w", labeled.ID, err)
		}
		latencies = append(latencies, time.Since(started))
		if strings.Contains(response.SearchMode, ArmDense) {
			dense++
		}

		ranked := make([]string, 0, len(response.Results))
		for _, result := range response.Results {
			ranked = append(ranked, result.Reel.URL)
		}
		scores = append(scores, Score(ranked, labeled.Relevant))
	}

	system := "go-hybrid"
	if dense == 0 {
		system = "go-lexical"
	}
	return Summarize(system, scores, latencies, dense), scores, nil
}
