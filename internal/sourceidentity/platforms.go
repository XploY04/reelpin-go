package sourceidentity

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Platform identities carry PublicScope: their URL shape is a canonical content
// id anyone can open. Page fallbacks and generic links carry UserScope, because
// nothing about their shape proves what a fetch would need to see them.

func instagramIdentity(raw string, parsed *url.URL) SourceIdentity {
	parts := segments(parsed.Path)
	if len(parts) >= 2 {
		shortcode := parts[1]
		switch strings.ToLower(parts[0]) {
		case "reel", "reels":
			return SourceIdentity{
				OriginalURL:   raw,
				NormalizedURL: "https://www.instagram.com/reel/" + shortcode + "/",
				Platform:      "instagram",
				ContentType:   "reel",
				ContentID:     shortcode,
				Scope:         PublicScope(),
			}
		case "p":
			return SourceIdentity{
				OriginalURL:   raw,
				NormalizedURL: "https://www.instagram.com/p/" + shortcode + "/",
				Platform:      "instagram",
				ContentType:   "post",
				ContentID:     shortcode,
				Scope:         PublicScope(),
			}
		case "tv":
			return SourceIdentity{
				OriginalURL:   raw,
				NormalizedURL: "https://www.instagram.com/tv/" + shortcode + "/",
				Platform:      "instagram",
				ContentType:   "video",
				ContentID:     shortcode,
				Scope:         PublicScope(),
			}
		}
	}

	return SourceIdentity{
		OriginalURL:   raw,
		NormalizedURL: normalizeGenericURL(parsed, "www.instagram.com"),
		Platform:      "instagram",
		ContentType:   "page",
		Scope:         UserScope(""),
	}
}

func tiktokIdentity(raw string, parsed *url.URL) SourceIdentity {
	parts := segments(parsed.Path)
	videoID := ""
	for index, segment := range parts {
		if strings.EqualFold(segment, "video") && index+1 < len(parts) {
			videoID = parts[index+1]
			break
		}
	}

	scope := UserScope("")
	if videoID != "" {
		scope = PublicScope()
	}
	return SourceIdentity{
		OriginalURL:   raw,
		NormalizedURL: normalizeGenericURL(parsed, "www.tiktok.com"),
		Platform:      "tiktok",
		ContentType:   "video",
		ContentID:     videoID,
		Scope:         scope,
	}
}

func youtubeIdentity(raw string, parsed *url.URL, host string) SourceIdentity {
	parts := segments(parsed.Path)
	videoID := ""
	contentType := "video"
	normalized := ""

	switch {
	case host == "youtu.be" && len(parts) > 0:
		videoID = parts[0]
		normalized = "https://www.youtube.com/shorts/" + videoID
		contentType = "short"
	case len(parts) >= 2 && strings.EqualFold(parts[0], "shorts"):
		videoID = parts[1]
		normalized = "https://www.youtube.com/shorts/" + videoID
		contentType = "short"
	default:
		videoID = parsed.Query().Get("v")
		if videoID != "" {
			normalized = "https://www.youtube.com/watch?v=" + url.QueryEscape(videoID)
		} else {
			normalized = normalizeGenericURL(parsed, "www.youtube.com")
		}
	}

	scope := UserScope("")
	if videoID != "" {
		scope = PublicScope()
	}
	return SourceIdentity{
		OriginalURL:   raw,
		NormalizedURL: normalized,
		Platform:      "youtube",
		ContentType:   contentType,
		ContentID:     videoID,
		Scope:         scope,
	}
}

// xIdentity accepts direct post URLs, and follows a t.co link to one. A t.co
// link without a resolver is refused rather than guessed at.
func (r *Resolver) xIdentity(ctx context.Context, raw string, parsed *url.URL, host string) (SourceIdentity, error) {
	if host != "t.co" {
		return parseDirectXPostURL(raw, raw)
	}

	if r.Redirects == nil {
		return SourceIdentity{}, fmt.Errorf("%w: a t.co link needs redirect resolution", ErrUnsupportedURL)
	}
	resolved, err := r.Redirects.ResolveRedirects(ctx, parsed.String())
	if err != nil {
		return SourceIdentity{}, fmt.Errorf("%w: the shortened link could not be resolved safely", ErrUnsupportedURL)
	}

	identity, err := parseDirectXPostURL(resolved, raw)
	if err != nil {
		return SourceIdentity{}, fmt.Errorf("%w: the t.co link does not resolve to a supported X post", ErrUnsupportedURL)
	}
	return identity, nil
}

var directXHosts = map[string]bool{
	"x.com": true, "www.x.com": true,
	"twitter.com": true, "www.twitter.com": true,
	"mobile.twitter.com": true, "mobile.x.com": true,
}

func parseDirectXPostURL(rawURL, originalURL string) (SourceIdentity, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return SourceIdentity{}, fmt.Errorf("%w: %v", ErrUnsupportedURL, err)
	}
	if scheme := strings.ToLower(parsed.Scheme); scheme != "http" && scheme != "https" {
		return SourceIdentity{}, fmt.Errorf("%w: only http and https are supported", ErrUnsupportedURL)
	}
	if !directXHosts[strings.ToLower(parsed.Hostname())] {
		return SourceIdentity{}, fmt.Errorf("%w: only direct x.com or twitter.com post urls are supported", ErrUnsupportedURL)
	}

	parts := segments(parsed.Path)
	// /<user>/status/<id>/photo/1 and /video/1 name the same post.
	if len(parts) == 5 && strings.EqualFold(parts[1], "status") &&
		(strings.EqualFold(parts[3], "photo") || strings.EqualFold(parts[3], "video")) &&
		digitsPattern.MatchString(parts[4]) {
		parts = parts[:3]
	}

	username, postID := "", ""
	switch {
	case len(parts) == 3 && strings.EqualFold(parts[1], "status"):
		username, postID = parts[0], parts[2]
	case len(parts) == 4 && strings.EqualFold(parts[0], "i") &&
		strings.EqualFold(parts[1], "web") && strings.EqualFold(parts[2], "status"):
		postID = parts[3]
	}

	if postID == "" || !digitsPattern.MatchString(postID) {
		return SourceIdentity{}, fmt.Errorf("%w: the url is not an X post url with a numeric post id", ErrUnsupportedURL)
	}
	if username != "" && !isAlphanumericUnderscore(username) {
		return SourceIdentity{}, fmt.Errorf("%w: the X post username is invalid", ErrUnsupportedURL)
	}

	normalized := "https://x.com/i/web/status/" + postID
	if username != "" {
		normalized = "https://x.com/" + username + "/status/" + postID
	}

	return SourceIdentity{
		OriginalURL:   originalURL,
		NormalizedURL: normalized,
		Platform:      "x",
		ContentType:   "post",
		ContentID:     postID,
		Scope:         PublicScope(),
	}, nil
}

func pinterestIdentity(raw string, parsed *url.URL) SourceIdentity {
	parts := segments(parsed.Path)
	pinID := ""
	for index, segment := range parts {
		if segment == "pin" && index+1 < len(parts) && digitsPattern.MatchString(parts[index+1]) {
			pinID = parts[index+1]
			break
		}
	}

	if pinID != "" {
		return SourceIdentity{
			OriginalURL:   raw,
			NormalizedURL: "https://www.pinterest.com/pin/" + pinID + "/",
			Platform:      "pinterest",
			ContentType:   "pin",
			ContentID:     pinID,
			Scope:         PublicScope(),
		}
	}

	normalized := normalizeGenericURL(parsed, "")
	return SourceIdentity{
		OriginalURL:   raw,
		NormalizedURL: normalized,
		Platform:      "pinterest",
		ContentType:   "pin",
		ContentID:     contentHash(normalized),
		Scope:         UserScope(""),
	}
}

// linkedinIdentity keeps the urn type. The same post has a different id under
// activity, ugcPost, and share, and the scraper returns nothing for the wrong
// one. Everything that is not a post falls through to page metadata.
func linkedinIdentity(raw string, parsed *url.URL) SourceIdentity {
	if match := linkedInPostPattern.FindStringSubmatch(parsed.Path); match != nil {
		urnType := map[string]string{
			"activity": "activity",
			"ugcpost":  "ugcPost",
			"share":    "share",
		}[strings.ToLower(match[1])]
		postID := match[2]

		return SourceIdentity{
			OriginalURL:   raw,
			NormalizedURL: "https://www.linkedin.com/feed/update/urn:li:" + urnType + ":" + postID + "/",
			Platform:      "linkedin",
			ContentType:   "post",
			ContentID:     postID,
			Scope:         PublicScope(),
		}
	}

	contentType, slug := linkedinPageKind(parsed.Path)
	normalized := normalizeGenericURL(parsed, "www.linkedin.com")
	identity := SourceIdentity{
		OriginalURL:   raw,
		NormalizedURL: normalized,
		Platform:      "linkedin",
		ContentType:   contentType,
		ContentID:     slug,
		Scope:         UserScope(""),
	}
	if slug == "" {
		identity.ContentID = contentHash(normalized)
	}
	return identity
}

func linkedinPageKind(path string) (string, string) {
	parts := segments(path)
	if len(parts) == 0 {
		return "page", ""
	}
	kind, ok := linkedInPageKinds[strings.ToLower(parts[0])]
	if !ok {
		kind = "page"
	}
	if kind == "job" {
		if match := linkedInJobPattern.FindString(path); match != "" {
			return kind, match
		}
		return kind, ""
	}
	if len(parts) > 1 {
		return kind, parts[1]
	}
	return kind, ""
}

// redditIdentity resolves a mobile share link through the injected resolver.
// The share shape is matched exactly, so no other path triggers an
// authenticated request.
func (r *Resolver) redditIdentity(ctx context.Context, raw string, parsed *url.URL, host string) (SourceIdentity, error) {
	postID := redditPostID(parsed.Path, host)

	if postID == "" && redditSharePattern.MatchString(parsed.Path) {
		if r.Reddit == nil {
			return SourceIdentity{}, fmt.Errorf("%w: a reddit share link needs share resolution", ErrUnsupportedURL)
		}
		canonical, err := r.Reddit.ResolveShareLink(ctx, raw)
		if err == nil && canonical != "" {
			if canonicalURL, canonicalHost, parseErr := parse(canonical); parseErr == nil {
				postID = redditPostID(canonicalURL.Path, canonicalHost)
			}
		}
	}

	if postID == "" || !isAlphanumericUnderscore(postID) {
		return SourceIdentity{}, fmt.Errorf("%w: the url is not a supported reddit post url", ErrUnsupportedURL)
	}

	return SourceIdentity{
		OriginalURL:   raw,
		NormalizedURL: "https://www.reddit.com/comments/" + postID + "/",
		Platform:      "reddit",
		ContentType:   "post",
		ContentID:     postID,
		Scope:         PublicScope(),
	}, nil
}

func redditPostID(path, host string) string {
	parts := segments(path)
	for index, segment := range parts {
		if segment == "comments" && index+1 < len(parts) {
			return parts[index+1]
		}
	}
	if host == "redd.it" && len(parts) > 0 && !strings.EqualFold(parts[0], "r") {
		return parts[0]
	}
	return ""
}

func genericIdentity(raw string, parsed *url.URL, platform string) (SourceIdentity, error) {
	normalized := normalizeGenericURL(parsed, "")
	if platform == "" {
		platform = normalizedHost(parsed.Hostname())
	}

	// Nothing about a generic link's shape proves what a fetch would need to
	// see it, so it starts fenced to the sharing user. A worker may promote it
	// after an uncredentialed fetch proves it public.
	return SourceIdentity{
		OriginalURL:   raw,
		NormalizedURL: normalized,
		Platform:      platform,
		ContentType:   "link",
		ContentID:     contentHash(normalized),
		Scope:         UserScope(""),
	}, nil
}

func isAlphanumericUnderscore(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}
