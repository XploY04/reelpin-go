package reels

import (
	"fmt"
	"sort"
	"strings"
)

const OtherPlatform = "other"

var platformLabels = map[string]string{
	"instagram":   "Instagram",
	"youtube":     "YouTube",
	"x":           "X",
	"tiktok":      "TikTok",
	"pinterest":   "Pinterest",
	"reddit":      "Reddit",
	"linkedin":    "LinkedIn",
	OtherPlatform: "Other",
}

// Legacy spellings already present in stored rows, or that clients may send.
var platformAliases = map[string]string{
	"twitter":       "x",
	"twitter.com":   "x",
	"x.com":         "x",
	"t.co":          "x",
	"instagram.com": "instagram",
	"instagr.am":    "instagram",
	"youtube.com":   "youtube",
	"youtu.be":      "youtube",
	"yt":            "youtube",
	"tiktok.com":    "tiktok",
	"pinterest.com": "pinterest",
	"pin.it":        "pinterest",
	"reddit.com":    "reddit",
	"redd.it":       "reddit",
	"linkedin.com":  "linkedin",
	"lnkd.in":       "linkedin",
}

// AllowedPlatformValues is what a client may send, sorted for stable errors.
func AllowedPlatformValues() []string {
	values := make([]string, 0, len(platformLabels))
	for platform := range platformLabels {
		values = append(values, platform)
	}
	sort.Strings(values)
	return values
}

// KnownStoredPlatformValues is every stored spelling that maps onto a canonical
// platform. A row holding anything else belongs to the `other` bucket.
func KnownStoredPlatformValues() []string {
	seen := map[string]bool{}
	for platform := range platformLabels {
		if platform != OtherPlatform {
			seen[platform] = true
		}
	}
	for alias := range platformAliases {
		seen[alias] = true
	}
	values := make([]string, 0, len(seen))
	for value := range seen {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

// StoredPlatformValues lists the stored spellings resolving to one platform.
func StoredPlatformValues(platform string) []string {
	values := []string{platform}
	for alias, target := range platformAliases {
		if target == platform {
			values = append(values, alias)
		}
	}
	sort.Strings(values)
	return values
}

func CanonicalPlatform(value string) (string, bool) {
	cleaned := strings.ToLower(strings.TrimSpace(value))
	if cleaned == "" {
		return "", false
	}
	if _, ok := platformLabels[cleaned]; ok {
		return cleaned, true
	}
	target, ok := platformAliases[cleaned]
	return target, ok
}

// RecordPlatform buckets a stored source_platform value.
func RecordPlatform(value *string) string {
	if value == nil {
		return OtherPlatform
	}
	platform, ok := CanonicalPlatform(*value)
	if !ok {
		return OtherPlatform
	}
	return platform
}

func PlatformLabel(platform string) string {
	if label, ok := platformLabels[platform]; ok {
		return label
	}
	// Unreachable for canonical ids; mirrors the Python fallback anyway.
	if platform == "" {
		return ""
	}
	return strings.ToUpper(platform[:1]) + platform[1:]
}

type InvalidPlatformError struct {
	Value   string
	Allowed []string
}

func (e *InvalidPlatformError) Error() string {
	return fmt.Sprintf("unknown platform filter value: %s", e.Value)
}

// ParsePlatformFilter reads a single or comma-separated `platform` parameter.
// A blank parameter means no filter.
func ParsePlatformFilter(value string) ([]string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return nil, nil
	}

	var platforms []string
	for _, part := range strings.Split(raw, ",") {
		cleaned := strings.TrimSpace(part)
		if cleaned == "" {
			continue
		}
		platform, ok := CanonicalPlatform(cleaned)
		if !ok {
			return nil, &InvalidPlatformError{Value: cleaned, Allowed: AllowedPlatformValues()}
		}
		if !contains(platforms, platform) {
			platforms = append(platforms, platform)
		}
	}
	return platforms, nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
