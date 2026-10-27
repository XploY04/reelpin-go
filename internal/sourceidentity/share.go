package sourceidentity

import (
	"context"
	"net/url"
	"regexp"
	"strings"
)

// MaxSharePayloadLength matches the contract's bound on raw_payload_text.
const MaxSharePayloadLength = 8192

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

// ExtractURLCandidates returns every link in a raw share payload, in payload
// order, without duplicates.
func ExtractURLCandidates(payload string) []string {
	if len(payload) > MaxSharePayloadLength {
		payload = payload[:MaxSharePayloadLength]
	}
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

// ResolvePayload answers what a raw share is about. The handler owning the
// endpoint maps the identity or the error to the contract shape; this package
// returns domain values only.
func (r *Resolver) ResolvePayload(ctx context.Context, payload string) (SourceIdentity, error) {
	extracted := SelectPrimaryURL(ExtractURLCandidates(payload))
	if extracted == "" {
		return SourceIdentity{}, ErrUnsupportedURL
	}
	return r.Resolve(ctx, extracted)
}
