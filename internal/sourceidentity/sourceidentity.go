// Package sourceidentity turns a shared link into the canonical identity the
// rest of the system keys on. It is the ingestion boundary: every platform,
// every dedup key, and every cache lookup starts here.
package sourceidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// ErrUnsupportedURL is every URL this service will not accept. The wrapped
// reason is for logs; handlers map the sentinel to one stable public code.
var ErrUnsupportedURL = errors.New("unsupported url")

// MaxURLLength bounds every URL before parsing, matching safehttp's rule.
const MaxURLLength = 2048

// AccessScope decides how far deduplication may reach. Content proven publicly
// addressable shares one global row across every user; anything unknown or
// fetched with credentials stays fenced to the user who shared it, so one
// user's private extraction can never leak into another's library.
//
// Promotion from user scope to public happens later, in a worker, only after
// fetching the source without user credentials proves it public. Nothing in
// this package promotes.
type AccessScope struct {
	public bool
	userID string
}

// PublicScope is content whose URL shape proves public addressability: a
// platform's canonical content id that anyone can open.
func PublicScope() AccessScope { return AccessScope{public: true} }

// UserScope fences content to one user. The user is usually unknown at resolve
// time; ForUser fills it in at enqueue.
func UserScope(userID string) AccessScope { return AccessScope{userID: userID} }

func (s AccessScope) IsPublic() bool { return s.public }

// ForUser qualifies a user scope with the authenticated user. A public scope
// is unchanged: the whole point is that it does not depend on who shared it.
func (s AccessScope) ForUser(userID string) AccessScope {
	if s.public {
		return s
	}
	return AccessScope{userID: userID}
}

// Hash is the scope component of the global identity key. A user scope without
// a user is an error, never a shared bucket: an empty qualifier would silently
// merge every unqualified save into one identity.
func (s AccessScope) Hash() (string, error) {
	if s.public {
		return "public", nil
	}
	if s.userID == "" {
		return "", errors.New("a user scope needs a user; call ForUser first")
	}
	sum := sha256.Sum256([]byte("user:" + s.userID))
	return hex.EncodeToString(sum[:])[:16], nil
}

type SourceIdentity struct {
	OriginalURL   string `json:"original_url"`
	NormalizedURL string `json:"normalized_url"`
	Platform      string `json:"source_platform"`
	ContentType   string `json:"source_content_type"`
	ContentID     string `json:"source_content_id"`
	// Scope decides whether this identity may deduplicate across users.
	Scope AccessScope `json:"-"`
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
	if len(raw) > MaxURLLength {
		return nil, "", fmt.Errorf("%w: over %d characters", ErrUnsupportedURL, MaxURLLength)
	}
	// Share text often carries a bare host; anything with an explicit scheme
	// other than http(s) is refused rather than rewritten.
	if !strings.Contains(raw, "://") {
		raw = "https://" + strings.TrimLeft(raw, "/")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUnsupportedURL, err)
	}
	if scheme := strings.ToLower(parsed.Scheme); scheme != "http" && scheme != "https" {
		return nil, "", fmt.Errorf("%w: only http and https are supported", ErrUnsupportedURL)
	}
	if parsed.User != nil {
		return nil, "", fmt.Errorf("%w: credentials in a url are not allowed", ErrUnsupportedURL)
	}

	host := normalizedHost(parsed.Hostname())
	if host == "" {
		return nil, "", fmt.Errorf("%w: the url has no host", ErrUnsupportedURL)
	}
	if strings.Contains(host, "%") {
		return nil, "", fmt.Errorf("%w: an ipv6 zone identifier is not allowed", ErrUnsupportedURL)
	}
	if port := parsed.Port(); port != "" && port != "80" && port != "443" {
		return nil, "", fmt.Errorf("%w: only ports 80 and 443 are allowed", ErrUnsupportedURL)
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
// host, no default port, no fragment, no duplicate or trailing slashes,
// tracking parameters removed, and the rest sorted. Rebuilding the path and
// query from their decoded values also canonicalizes percent-encoding, so
// %7Euser and ~user produce one identity. Case and meaning in the path and
// remaining query are kept.
func normalizeGenericURL(parsed *url.URL, preferredHost string) string {
	host := preferredHost
	if host == "" {
		host = parsed.Hostname()
	}
	host = strings.ToLower(host)
	// Hostname() strips the brackets an IPv6 literal needs to be a valid host.
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}

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
		entries := kept[key]
		sort.Strings(entries)
		for _, entry := range entries {
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
