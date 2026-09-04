// Package sourceidentity turns a shared link into the canonical identity the
// rest of the system keys on. It is the ingestion boundary: every platform,
// every dedup key, and every cache lookup starts here.
package sourceidentity

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// ErrUnsupportedURL is every URL this service will not accept.
var ErrUnsupportedURL = errors.New("unsupported url")

type SourceIdentity struct {
	OriginalURL   string `json:"original_url"`
	NormalizedURL string `json:"normalized_url"`
	Platform      string `json:"source_platform"`
	ContentType   string `json:"source_content_type"`
	ContentID     string `json:"source_content_id"`
	// LegacyContentID is the SHA-1 id Python wrote for generic links. New keys
	// use ContentID; this only matches rows written before the migration.
	LegacyContentID string `json:"-"`
}

// trackingQueryKeys are dropped during normalization so the same post shared
// from two apps produces one identity.
var trackingQueryKeys = map[string]bool{
	"fbclid":        true,
	"feature":       true,
	"igsh":          true,
	"igshid":        true,
	"pp":            true,
	"si":            true,
	"share_app_id":  true,
	"share_item_id": true,
	"spm":           true,
}

// placeHostTokens are curated place and travel hosts. They carry a location
// directly, so the pipeline skips the video attempt for them.
var placeHostTokens = []struct{ token, platform string }{
	{"tripadvisor", "tripadvisor"},
	{"airbnb", "airbnb"},
	{"zomato", "zomato"},
	{"swiggy", "swiggy"},
	{"makemytrip", "makemytrip"},
	{"booking.com", "booking"},
}

var linkedInPageKinds = map[string]string{
	"in":          "profile",
	"company":     "company",
	"school":      "company",
	"jobs":        "job",
	"pulse":       "article",
	"newsletters": "newsletter",
	"newsletter":  "newsletter",
	"events":      "event",
	"groups":      "group",
	"learning":    "course",
}

var (
	linkedInPostPattern = regexp.MustCompile(`(?i)(?:urn:li:)?(activity|ugcpost|share)[:\-](\d{6,})`)
	linkedInJobPattern  = regexp.MustCompile(`(\d{5,})`)
	redditSharePattern  = regexp.MustCompile(`^/r/[^/]+/s/[^/]+/?$`)
	digitsPattern       = regexp.MustCompile(`^\d+$`)
	multiSlashPattern   = regexp.MustCompile(`/{2,}`)
)

// Resolve derives an identity without touching the network. A t.co link or a
// Reddit mobile share link needs a Resolver instead.
func Resolve(rawURL string) (SourceIdentity, error) {
	return new(Resolver).Resolve(nil, rawURL)
}

func normalizedHost(host string) string {
	lowered := strings.ToLower(host)
	for _, prefix := range []string{"www.", "m.", "mobile."} {
		if strings.HasPrefix(lowered, prefix) {
			lowered = lowered[len(prefix):]
		}
	}
	return lowered
}

func matchesHost(host string, allowed ...string) bool {
	for _, candidate := range allowed {
		if host == candidate || strings.HasSuffix(host, "."+candidate) {
			return true
		}
	}
	return false
}

func parse(rawURL string) (*url.URL, string, error) {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return nil, "", fmt.Errorf("%w: a url is required", ErrUnsupportedURL)
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + strings.TrimLeft(raw, "/")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUnsupportedURL, err)
	}
	host := normalizedHost(parsed.Hostname())
	if host == "" {
		return nil, "", fmt.Errorf("%w: the url has no host", ErrUnsupportedURL)
	}
	return parsed, host, nil
}

func segments(path string) []string {
	parts := []string{}
	for _, segment := range strings.Split(path, "/") {
		if segment != "" {
			parts = append(parts, segment)
		}
	}
	return parts
}

// normalizeGenericURL collapses a URL to its stable form: https, lowercase
// host, no duplicate or trailing slashes, tracking parameters removed, and the
// rest sorted. Case and meaning in the path and remaining query are kept.
func normalizeGenericURL(parsed *url.URL, preferredHost string) string {
	host := preferredHost
	if host == "" {
		host = parsed.Hostname()
	}
	host = strings.ToLower(host)

	path := parsed.Path
	if path == "" {
		path = "/"
	}
	path = multiSlashPattern.ReplaceAllString(path, "/")
	if path != "/" {
		path = strings.TrimRight(path, "/")
	}
	if path == "" {
		path = "/"
	}

	values := parsed.Query()
	kept := url.Values{}
	for key, entries := range values {
		if isTrackingKey(key) {
			continue
		}
		for _, entry := range entries {
			if entry == "" {
				continue
			}
			kept.Add(key, entry)
		}
	}

	keys := make([]string, 0, len(kept))
	for key := range kept {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var query strings.Builder
	for _, key := range keys {
		for _, entry := range kept[key] {
			if query.Len() > 0 {
				query.WriteByte('&')
			}
			query.WriteString(url.QueryEscape(key))
			query.WriteByte('=')
			query.WriteString(url.QueryEscape(entry))
		}
	}

	normalized := &url.URL{Scheme: "https", Host: host, Path: path, RawQuery: query.String()}
	return normalized.String()
}

func isTrackingKey(key string) bool {
	lowered := strings.ToLower(key)
	return strings.HasPrefix(lowered, "utm_") || trackingQueryKeys[lowered]
}

// contentHash is the identity of a link that carries no id of its own.
func contentHash(normalizedURL string) string {
	sum := sha256.Sum256([]byte(normalizedURL))
	return hex.EncodeToString(sum[:])[:16]
}

// legacyContentHash reproduces the SHA-1 id Python wrote, for reading old rows.
func legacyContentHash(normalizedURL string) string {
	sum := sha1.Sum([]byte(normalizedURL))
	return hex.EncodeToString(sum[:])[:16]
}
