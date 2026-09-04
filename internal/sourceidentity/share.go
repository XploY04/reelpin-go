package sourceidentity

import (
	"context"
	"net/url"
	"regexp"
	"strings"
)

// urlPattern matches every http(s) or bare www link in a share payload.
var urlPattern = regexp.MustCompile(`(?i)https?://[^\s<>"']+|(?:www\.)[^\s<>"']+`)

const urlTrailing = ").,]}\"'"

// opaqueShorteners carry no identity of their own. The intended post almost
// always leads the payload, so a leading shortener is stepped past rather than
// reordering every candidate.
var opaqueShorteners = map[string]bool{
	"t.co":        true,
	"lnkd.in":     true,
	"ig.me":       true,
	"bit.ly":      true,
	"tinyurl.com": true,
	"goo.gl":      true,
}

// ShareResolveResponse is the body the app already expects.
type ShareResolveResponse struct {
	Supported     bool    `json:"supported"`
	ExtractedURL  *string `json:"extracted_url"`
	NormalizedURL *string `json:"normalized_url"`
	Provider      *string `json:"provider"`
	ErrorMessage  *string `json:"error_message"`
}

// ExtractURLCandidates returns every link in a raw share payload, in payload
// order, without duplicates.
func ExtractURLCandidates(payload string) []string {
	seen := map[string]bool{}
	candidates := []string{}
	for _, match := range urlPattern.FindAllString(payload, -1) {
		candidate := strings.TrimRight(match, urlTrailing)
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		candidates = append(candidates, candidate)
	}
	return candidates
}

// SelectPrimaryURL picks the link a share is actually about: the first one that
// is not an opaque shortener, and otherwise the first one.
func SelectPrimaryURL(candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	for _, candidate := range candidates {
		if !opaqueShorteners[candidateHost(candidate)] {
			return candidate
		}
	}
	return candidates[0]
}

func candidateHost(rawURL string) string {
	raw := rawURL
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
}

// ResolveSharePayload answers the share-resolve endpoint: what link is this
// share about, and can the pipeline take it?
func (r *Resolver) ResolveSharePayload(ctx context.Context, payload string) ShareResolveResponse {
	extracted := SelectPrimaryURL(ExtractURLCandidates(payload))
	if extracted == "" {
		return ShareResolveResponse{
			ErrorMessage: pointer("No link was found in the shared content."),
		}
	}

	identity, err := r.Resolve(ctx, extracted)
	if err != nil {
		var provider *string
		if host := candidateHost(extracted); host != "" {
			provider = pointer("web")
		}
		return ShareResolveResponse{
			ExtractedURL: &extracted,
			Provider:     provider,
			ErrorMessage: pointer("This URL provider is not supported for reel processing."),
		}
	}

	return ShareResolveResponse{
		Supported:     true,
		ExtractedURL:  &extracted,
		NormalizedURL: &identity.NormalizedURL,
		Provider:      &identity.Platform,
	}
}

func pointer(value string) *string { return &value }
