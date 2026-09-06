//go:build integration

package ai

// The measurement behind docs/model-migration.md. It calls the real provider,
// so it needs GEMINI_API_KEY and skips without one: CI has no key and must stay
// green. Re-run it when a candidate list changes rather than trusting the
// numbers in the doc to still hold.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/spend"
)

// candidateModels are the pinned versions the migration chose between.
// gemini-2.0-flash-lite was measured alongside them and is deliberately not
// here: it answers 404 to every generateContent call, so leaving it in makes
// this test fail forever over a result docs/model-migration.md already records.
var candidateModels = []string{
	"gemini-2.5-flash-lite",
	"gemini-3.1-flash-lite",
	"gemini-3.5-flash-lite",
}

const (
	// evalReels is how many corpus entries each run covers, and evalRepeats how
	// many times each one is extracted. Schema conformance is the thing being
	// measured, and a single pass cannot tell a reliable model from a lucky one.
	evalReels   = 20
	evalRepeats = 3
	// One request at a time. Concurrent requests share Go's HTTP/2 connection,
	// and a stall on that connection times out requests that have nothing to do
	// with the model being measured.
	evalWorkers = 1
)

const corpusPath = "../search/testdata/corpus-v1.json"

type corpusReel struct {
	Title       string   `json:"title"`
	Summary     string   `json:"summary"`
	Caption     string   `json:"caption"`
	Facts       []string `json:"facts"`
	Category    string   `json:"category"`
	Subcategory string   `json:"subcategory"`
	Tags        []string `json:"tags"`
	Places      []string `json:"places"`
	Transcript  string   `json:"transcript"`
}

func loadEvalCorpus(t *testing.T) []corpusReel {
	t.Helper()
	body, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	var decoded struct {
		Reels []corpusReel `json:"reels"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decoding the corpus: %v", err)
	}
	if len(decoded.Reels) < evalReels {
		t.Fatalf("the corpus holds %d reels, the run needs %d", len(decoded.Reels), evalReels)
	}
	return decoded.Reels[:evalReels]
}

// corpusTaxonomy is the labelled category tree the corpus already carries, so
// categorization is scored against real labels rather than invented ones.
func corpusTaxonomy(reels []corpusReel) []TaxonomyOption {
	subcategories := map[string]map[string]bool{}
	for _, reel := range reels {
		if subcategories[reel.Category] == nil {
			subcategories[reel.Category] = map[string]bool{}
		}
		subcategories[reel.Category][reel.Subcategory] = true
	}
	names := make([]string, 0, len(subcategories))
	for name := range subcategories {
		names = append(names, name)
	}
	sort.Strings(names)

	options := make([]TaxonomyOption, 0, len(names))
	for _, name := range names {
		subs := make([]string, 0, len(subcategories[name]))
		for sub := range subcategories[name] {
			subs = append(subs, sub)
		}
		sort.Strings(subs)
		option := TaxonomyOption{Name: name, Description: "saved " + name + " content"}
		for _, sub := range subs {
			option.Subcategories = append(option.Subcategories, TaxonomyOption{
				Name: sub, Description: sub + " under " + name,
			})
		}
		options = append(options, option)
	}
	return options
}

// The schema as it is sent. A key outside this set is the provider not honouring
// responseSchema, which is the failure mode that would corrupt content versions
// quietly.
var extractionFields = map[string]string{
	"title": "string", "summary": "string",
	"content_domain": "string", "content_format": "string",
	"topical_tags": "strings", "key_facts": "strings",
	"people_mentioned": "strings", "actionable_items": "strings",
	"locations": "locations", "events": "events",
}

var (
	locationFields = map[string]bool{
		"name": true, "neighborhood": true, "city": true, "state": true, "country": true,
	}
	eventFields = map[string]bool{"name": true, "date": true, "time": true}
)

func checkExtractionShape(raw string) []string {
	problems := []string{}
	if raw != stripFences(raw) {
		problems = append(problems, "wrapped in a markdown fence despite responseMimeType")
	}
	object, err := decodeObject(stripFences(raw))
	if err != nil {
		return append(problems, err.Error())
	}
	for _, required := range []string{"title", "summary"} {
		if _, ok := object[required]; !ok {
			problems = append(problems, "required field "+required+" missing")
		}
	}
	for key, value := range object {
		kind, declared := extractionFields[key]
		if !declared {
			problems = append(problems, "undeclared field "+key)
			continue
		}
		problems = append(problems, checkField(key, kind, value)...)
	}
	return problems
}

func checkField(key, kind string, value json.RawMessage) []string {
	switch kind {
	case "string":
		var s string
		if json.Unmarshal(value, &s) != nil {
			return []string{key + " is not a string"}
		}
	case "strings":
		var list []string
		if json.Unmarshal(value, &list) != nil {
			return []string{key + " is not an array of strings"}
		}
	case "locations":
		return checkObjectArray(key, value, locationFields)
	case "events":
		return checkObjectArray(key, value, eventFields)
	}
	return nil
}

func checkObjectArray(key string, value json.RawMessage, allowed map[string]bool) []string {
	var items []json.RawMessage
	if json.Unmarshal(value, &items) != nil {
		return []string{key + " is not an array"}
	}
	problems := []string{}
	for _, item := range items {
		problems = append(problems, checkObjectFields(key, item, allowed)...)
	}
	return problems
}

// checkObjectFields holds every nested object in the two schemas to string
// fields the schema declares. Both schemas nest exactly one level, so this does
// not recurse.
func checkObjectFields(key string, value json.RawMessage, allowed map[string]bool) []string {
	fields, err := decodeObject(string(value))
	if err != nil {
		return []string{key + " is not an object"}
	}
	problems := []string{}
	for name, field := range fields {
		if !allowed[name] {
			problems = append(problems, key+" holds undeclared field "+name)
			continue
		}
		var s string
		if json.Unmarshal(field, &s) != nil {
			problems = append(problems, key+"."+name+" is not a string")
		}
	}
	return problems
}

var (
	categoryFields = map[string]string{
		"category": "string", "subcategory": "string", "proposal": "proposal",
	}
	proposalFields = map[string]bool{"name": true, "description": true}
)

func checkCategoryShape(raw string) []string {
	problems := []string{}
	if raw != stripFences(raw) {
		problems = append(problems, "wrapped in a markdown fence despite responseMimeType")
	}
	object, err := decodeObject(stripFences(raw))
	if err != nil {
		return append(problems, err.Error())
	}
	if _, ok := object["category"]; !ok {
		problems = append(problems, "required field category missing")
	}
	for key, value := range object {
		kind, declared := categoryFields[key]
		if !declared {
			problems = append(problems, "undeclared field "+key)
			continue
		}
		if kind == "proposal" {
			problems = append(problems, checkObjectFields(key, value, proposalFields)...)
			continue
		}
		problems = append(problems, checkField(key, kind, value)...)
	}
	return problems
}

func decodeObject(raw string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil {
		return nil, fmt.Errorf("not a JSON object: %v", err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("trailing content after the JSON object")
	}
	return object, nil
}

// tally is the accumulator for one model. Every field is guarded by its mutex
// because the reels run through a worker pool.
type tally struct {
	mu sync.Mutex

	calls      int
	callErrors []string
	violations []string
	latencies  []time.Duration

	placesFound, placesLabelled int
	emptyExtractions            int

	categoryCalls, categoryHits, subcategoryHits int
	categoryOffTaxonomy                          []string

	inputTokens, outputTokens int64
	// Per stage as well as in total, because the cost gate prices a job stage
	// by stage and the categorize prompt grows with the taxonomy while the
	// extract prompt does not.
	byOperation map[string]*stageTokens
}

type stageTokens struct{ calls, input, output int64 }

func (t *tally) Record(_ context.Context, usage spend.Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inputTokens += usage.InputTokens
	t.outputTokens += usage.OutputTokens
	if t.byOperation == nil {
		t.byOperation = map[string]*stageTokens{}
	}
	stage, ok := t.byOperation[usage.Operation]
	if !ok {
		stage = &stageTokens{}
		t.byOperation[usage.Operation] = stage
	}
	stage.calls++
	stage.input += usage.InputTokens
	stage.output += usage.OutputTokens
}

func (t *tally) note(latency time.Duration, err error, problems []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	t.latencies = append(t.latencies, latency)
	if err != nil {
		t.callErrors = append(t.callErrors, err.Error())
	}
	t.violations = append(t.violations, problems...)
}

func percentile(values []time.Duration, fraction float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(float64(len(sorted)-1) * fraction)
	return sorted[index]
}

func TestCandidateModelsAgainstTheLiveProvider(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	if key == "" {
		t.Skip("GEMINI_API_KEY is not set; the candidate comparison needs the real provider")
	}
	reels := loadEvalCorpus(t)
	taxonomy := corpusTaxonomy(reels)

	for _, model := range candidateModels {
		t.Run(model, func(t *testing.T) {
			counts := &tally{}
			client := NewGemini(GeminiConfig{
				APIKey: key, Model: model, Usage: counts, Timeout: 45 * time.Second,
			})
			ctx := context.Background()

			runOverReels(reels, func(reel corpusReel) {
				for pass := 0; pass < evalRepeats; pass++ {
					started := time.Now()
					raw, err := client.generate(ctx, "extract",
						[]geminiPart{{Text: extractionUserPrompt(spokenContent(reel), reel.Caption)}},
						extractionSchema)
					latency := time.Since(started)
					problems := []string{}
					if err == nil {
						problems = checkExtractionShape(raw)
					}
					counts.note(latency, err, problems)
					if err != nil {
						continue
					}
					scoreExtraction(counts, raw, reel)
				}
			})

			runOverReels(reels, func(reel corpusReel) {
				// The extraction is built from the corpus labels rather than
				// from what this model just extracted, so every candidate
				// categorizes the same input.
				prompt := categoryUserPrompt(Extraction{
					Title: reel.Title, Summary: reel.Summary, TopicalTags: reel.Tags,
				}, taxonomy)
				raw, err := client.generate(ctx, "categorize", []geminiPart{{Text: prompt}}, categorySchema)

				counts.mu.Lock()
				defer counts.mu.Unlock()
				counts.categoryCalls++
				if err != nil {
					counts.callErrors = append(counts.callErrors, "categorize: "+err.Error())
					return
				}
				counts.violations = append(counts.violations, checkCategoryShape(raw)...)

				var category Category
				if json.Unmarshal([]byte(stripFences(raw)), &category) != nil {
					counts.violations = append(counts.violations, "categorize: undecodable")
					return
				}
				if category.Category == reel.Category {
					counts.categoryHits++
				} else if !inTaxonomy(taxonomy, category.Category) {
					counts.categoryOffTaxonomy = append(counts.categoryOffTaxonomy, category.Category)
				}
				if category.Subcategory == reel.Subcategory {
					counts.subcategoryHits++
				}
			})

			report(t, model, counts)
		})
	}
}

// scoreExtraction measures against the corpus labels. Place recall is scored
// after Normalize, because that is the value the pipeline would store.
func scoreExtraction(counts *tally, raw string, reel corpusReel) {
	var extraction Extraction
	if json.Unmarshal([]byte(stripFences(raw)), &extraction) != nil {
		return
	}
	extraction = extraction.Normalize()

	var text strings.Builder
	for _, location := range extraction.Locations {
		text.WriteString(strings.ToLower(location.Name + " " + location.Address() + " "))
	}
	found := 0
	for _, place := range reel.Places {
		if strings.Contains(text.String(), strings.ToLower(place)) {
			found++
		}
	}

	counts.mu.Lock()
	defer counts.mu.Unlock()
	counts.placesFound += found
	counts.placesLabelled += len(reel.Places)
	if extraction.Validate() != nil {
		counts.emptyExtractions++
	}
}

// spokenContent stands in for what a real transcription would carry. The
// corpus transcript is one line written for search tests, and extracting places
// from it would measure nothing: the places it is labelled with are named in the
// title, summary and facts.
func spokenContent(reel corpusReel) string {
	parts := append([]string{reel.Title, reel.Summary, reel.Transcript}, reel.Facts...)
	return strings.Join(parts, " ")
}

func inTaxonomy(taxonomy []TaxonomyOption, name string) bool {
	for _, option := range taxonomy {
		if option.Name == name {
			return true
		}
	}
	return false
}

func runOverReels(reels []corpusReel, work func(corpusReel)) {
	queue := make(chan corpusReel)
	var group sync.WaitGroup
	for worker := 0; worker < evalWorkers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for reel := range queue {
				work(reel)
			}
		}()
	}
	for _, reel := range reels {
		queue <- reel
	}
	close(queue)
	group.Wait()
}

func report(t *testing.T, model string, counts *tally) {
	t.Helper()
	t.Logf("%s: %d extract calls, %d failed, %d schema violations, %d unusable extractions",
		model, counts.calls, len(counts.callErrors), len(counts.violations), counts.emptyExtractions)
	t.Logf("%s: latency p50 %v p95 %v max %v", model,
		percentile(counts.latencies, 0.5), percentile(counts.latencies, 0.95),
		percentile(counts.latencies, 1))
	t.Logf("%s: tokens in %d out %d over %d calls",
		model, counts.inputTokens, counts.outputTokens, counts.calls+counts.categoryCalls)
	for _, stage := range []string{"extract", "categorize"} {
		billed, ok := counts.byOperation[stage]
		if !ok {
			continue
		}
		t.Logf("%s: %s billed %d calls, mean tokens in %d out %d", model, stage,
			billed.calls, billed.input/billed.calls, billed.output/billed.calls)
	}
	t.Logf("%s: place recall %d/%d", model, counts.placesFound, counts.placesLabelled)
	t.Logf("%s: category %d/%d, subcategory %d/%d, off-taxonomy %v",
		model, counts.categoryHits, counts.categoryCalls,
		counts.subcategoryHits, counts.categoryCalls, counts.categoryOffTaxonomy)
	for _, problem := range firstFew(counts.violations, 5) {
		t.Logf("%s: violation %s", model, problem)
	}
	for _, failure := range firstFew(counts.callErrors, 5) {
		t.Logf("%s: call failed %s", model, failure)
	}
}

func firstFew(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	return values[:limit]
}

// TestCandidateModelsTranscribe needs a real audio file, which this repository
// deliberately does not carry: a committed media fixture is megabytes that every
// clone pays for. Point REELPIN_EVAL_AUDIO at one to measure transcription.
func TestCandidateModelsTranscribe(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	audio := strings.TrimSpace(os.Getenv("REELPIN_EVAL_AUDIO"))
	if key == "" || audio == "" {
		t.Skip("set GEMINI_API_KEY and REELPIN_EVAL_AUDIO to measure transcription")
	}
	for _, model := range candidateModels {
		t.Run(model, func(t *testing.T) {
			counts := &tally{}
			client := NewGemini(GeminiConfig{
				APIKey: key, Model: model, Usage: counts, Timeout: 45 * time.Second,
			})
			started := time.Now()
			transcript, err := client.Transcribe(context.Background(),
				Media{Path: audio, MIMEType: mimeTypeFor(audio)})
			if err != nil {
				t.Fatalf("Transcribe: %v", err)
			}
			t.Logf("%s: %v, tokens in %d out %d, transcript %q",
				model, time.Since(started), counts.inputTokens, counts.outputTokens, transcript)
		})
	}
}

func mimeTypeFor(path string) string {
	switch {
	case strings.HasSuffix(path, ".mp3"):
		return "audio/mpeg"
	case strings.HasSuffix(path, ".wav"):
		return "audio/wav"
	default:
		return "audio/mp4"
	}
}
