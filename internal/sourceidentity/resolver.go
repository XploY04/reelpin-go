package sourceidentity

import (
	"context"
	"strings"
)

// RedditResolver turns a Reddit mobile share link into its canonical permalink.
// Reddit 403s a plain fetch of those from datacenter IPs, so the real
// implementation calls the authenticated API. Keeping it an interface keeps
// identity resolution testable without OAuth.
type RedditResolver interface {
	ResolveShareLink(ctx context.Context, rawURL string) (string, error)
}

// RedirectResolver follows a shortener to its destination. safehttp.Client
// satisfies it.
type RedirectResolver interface {
	ResolveRedirects(ctx context.Context, rawURL string) (string, error)
}

// Resolver adds the two network-dependent cases to plain identity resolution.
// Both are optional: without them, a t.co or Reddit share link is rejected
// rather than silently mis-identified.
type Resolver struct {
	Redirects RedirectResolver
	Reddit    RedditResolver
}

func (r *Resolver) Resolve(ctx context.Context, rawURL string) (SourceIdentity, error) {
	parsed, host, err := parse(rawURL)
	if err != nil {
		return SourceIdentity{}, err
	}
	raw := strings.TrimSpace(rawURL)
	if !strings.Contains(raw, "://") {
		raw = "https://" + strings.TrimLeft(raw, "/")
	}

	switch {
	case matchesHost(host, "instagram.com", "instagr.am"):
		return instagramIdentity(raw, parsed), nil
	case matchesHost(host, "tiktok.com"):
		return tiktokIdentity(raw, parsed), nil
	case matchesHost(host, "youtube.com", "youtu.be"):
		return youtubeIdentity(raw, parsed, host), nil
	case host == "x.com" || host == "twitter.com" || host == "t.co":
		return r.xIdentity(ctx, raw, parsed, host)
	case matchesHost(host, "linkedin.com"):
		return linkedinIdentity(raw, parsed), nil
	case matchesHost(host, "reddit.com") || host == "redd.it":
		return r.redditIdentity(ctx, raw, parsed, host)
	case matchesHost(host, "pinterest.com") || host == "pin.it":
		return pinterestIdentity(raw, parsed), nil
	}

	if platform := placePlatform(host, parsed.Path); platform != "" {
		return genericIdentity(raw, parsed, platform)
	}
	return genericIdentity(raw, parsed, "")
}

func placePlatform(host, path string) string {
	lowerPath := strings.ToLower(path)
	switch {
	case host == "maps.app.goo.gl", strings.HasPrefix(host, "maps.google."):
		return "google_maps"
	case host == "goo.gl" && strings.HasPrefix(lowerPath, "/maps"):
		return "google_maps"
	case strings.Contains(host, "google.") && strings.HasPrefix(lowerPath, "/maps"):
		return "google_maps"
	}
	for _, candidate := range placeHostTokens {
		if strings.Contains(host, candidate.token) {
			return candidate.platform
		}
	}
	return ""
}
