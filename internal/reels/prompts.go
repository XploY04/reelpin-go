package reels

import (
	"sort"
	"strings"
)

// MaxQuickSearchPrompts is how many suggestions Discover shows.
const MaxQuickSearchPrompts = 5

// genericLabels are words that describe nothing. A prompt made of one sends the
// user to a search that returns everything.
var genericLabels = map[string]bool{
	"general": true, "misc": true, "miscellaneous": true, "none": true,
	"other": true, "uncategorized": true, "unknown": true,
}

// fallbackPrompts fill an empty or very new library, so Discover is never blank.
var fallbackPrompts = []string{"Travel", "Food", "Fitness", "Movies", "Coffee"}

// promptKindOrder is the order a prompt of each kind is taken in: one per kind
// first, so the suggestions are varied rather than five subcategories.
var promptKindOrder = []string{"subcategory", "category", "location", "source", "tag", "person"}

var platformPromptLabels = map[string]string{
	"direct_upload": "Uploaded",
	"instagram":     "Instagram",
	"tiktok":        "TikTok",
	"upload":        "Uploaded",
	"web":           "Web",
	"youtube":       "YouTube",
}

var contentTypePromptLabels = map[string]string{
	"page": "Pages", "post": "Posts", "reel": "Reels", "share": "Shares",
	"short": "Shorts", "web": "Links", "video": "Videos",
}

type promptCandidate struct {
	text  string
	score int
}

// BuildQuickSearchPrompts turns a library into a handful of searches worth
// running. Scores rank within a kind; the kind order keeps the set varied.
func BuildQuickSearchPrompts(records []ReelRecord) []string {
	if len(records) == 0 {
		return []string{}
	}

	candidates := map[string]map[string]promptCandidate{}
	for _, record := range DedupeRecords(records) {
		categoryLabel := promptLabel(record.Category)
		subcategoryLabel := promptLabel(record.Subcategory)
		platformLabel := platformPromptLabel(optionalString(record.SourcePlatform))
		contentLabel := contentTypePromptLabel(optionalString(record.SourceContentType))

		if subcategoryLabel != "" {
			addCandidate(candidates, "subcategory", subcategoryLabel, 120)
		}
		if categoryLabel != "" {
			addCandidate(candidates, "category", categoryLabel, 100)
		}
		switch {
		case platformLabel != "" && contentLabel != "":
			addCandidate(candidates, "source", platformLabel+" "+contentLabel, 90)
		case platformLabel != "":
			addCandidate(candidates, "source", platformLabel, 55)
		}

		for _, location := range record.Locations {
			locationLabel := promptLocationLabel(location)
			switch {
			case categoryLabel != "" && locationLabel != "":
				addCandidate(candidates, "location", categoryLabel+" in "+locationLabel, 95)
			case locationLabel != "":
				addCandidate(candidates, "location", locationLabel, 65)
			}
		}
		for _, tag := range record.SecondaryCategories {
			if label := promptLabel(tag); label != "" {
				addCandidate(candidates, "tag", label, 85)
			}
		}
		for _, person := range record.PeopleMentioned {
			if label := promptLabel(person); label != "" {
				addCandidate(candidates, "person", label, 45)
			}
		}
	}

	prompts := []string{}
	for _, kind := range promptKindOrder {
		for _, prompt := range rankedCandidates(candidates[kind]) {
			if appendUniquePrompt(&prompts, prompt) {
				break
			}
		}
		if len(prompts) >= MaxQuickSearchPrompts {
			return prompts[:MaxQuickSearchPrompts]
		}
	}

	// Then the best of everything left, whatever kind it is.
	all := []promptCandidate{}
	for _, kindCandidates := range candidates {
		for _, candidate := range kindCandidates {
			all = append(all, candidate)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return strings.ToLower(all[i].text) < strings.ToLower(all[j].text)
	})
	for _, candidate := range all {
		if len(prompts) >= MaxQuickSearchPrompts {
			break
		}
		appendUniquePrompt(&prompts, candidate.text)
	}

	for _, fallback := range fallbackPrompts {
		if len(prompts) >= MaxQuickSearchPrompts {
			break
		}
		appendUniquePrompt(&prompts, fallback)
	}
	if len(prompts) > MaxQuickSearchPrompts {
		prompts = prompts[:MaxQuickSearchPrompts]
	}
	return prompts
}

func addCandidate(candidates map[string]map[string]promptCandidate, kind, prompt string, score int) {
	normalized := normalizePrompt(prompt)
	if len([]rune(normalized)) < 3 {
		return
	}
	if candidates[kind] == nil {
		candidates[kind] = map[string]promptCandidate{}
	}
	key := strings.ToLower(normalized)
	if existing, ok := candidates[kind][key]; ok {
		// A label that appears across many saves is a better suggestion.
		existing.score += score
		candidates[kind][key] = existing
		return
	}
	candidates[kind][key] = promptCandidate{text: normalized, score: score}
}

func rankedCandidates(candidates map[string]promptCandidate) []string {
	ranked := make([]promptCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		ranked = append(ranked, candidate)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return strings.ToLower(ranked[i].text) < strings.ToLower(ranked[j].text)
	})

	texts := make([]string, 0, len(ranked))
	for _, candidate := range ranked {
		texts = append(texts, candidate.text)
	}
	return texts
}

func appendUniquePrompt(prompts *[]string, prompt string) bool {
	normalized := normalizePrompt(prompt)
	if normalized == "" {
		return false
	}
	for _, existing := range *prompts {
		if strings.EqualFold(existing, normalized) {
			return false
		}
	}
	*prompts = append(*prompts, normalized)
	return true
}

// normalizePrompt trims a label to something that fits a chip, cutting at a
// word boundary rather than mid-word.
func normalizePrompt(prompt string) string {
	normalized := strings.Join(strings.Fields(strings.Trim(prompt, ` "'`)), " ")
	if len([]rune(normalized)) <= 48 {
		return normalized
	}
	trimmed := strings.TrimSpace(string([]rune(normalized)[:48]))
	if index := strings.LastIndex(trimmed, " "); index > 0 {
		trimmed = trimmed[:index]
	}
	return trimmed
}

func promptLabel(value string) string {
	cleaned := strings.TrimSpace(value)
	if cleaned == "" {
		return ""
	}
	normalized := strings.ToLower(strings.NewReplacer("_", " ", "-", " ").Replace(cleaned))
	if genericLabels[strings.TrimSpace(normalized)] {
		return ""
	}
	return Labelize(cleaned)
}

func promptLocationLabel(location Location) string {
	for _, candidate := range []string{
		optionalString(location.City), optionalString(location.Neighborhood),
		optionalString(location.State), location.Name, optionalString(location.Country),
	} {
		if label := promptLabel(candidate); label != "" {
			return label
		}
	}
	return ""
}

func platformPromptLabel(value string) string {
	cleaned := strings.TrimSpace(value)
	if cleaned == "" {
		return ""
	}
	if label, ok := platformPromptLabels[strings.ToLower(cleaned)]; ok {
		return label
	}
	return Labelize(cleaned)
}

func contentTypePromptLabel(value string) string {
	cleaned := strings.TrimSpace(value)
	if cleaned == "" {
		return ""
	}
	if label, ok := contentTypePromptLabels[strings.ToLower(cleaned)]; ok {
		return label
	}
	return Labelize(cleaned)
}
