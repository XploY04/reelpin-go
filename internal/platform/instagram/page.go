package instagram

import (
	"context"
	"html"
	"regexp"
	"sort"
	"strings"
)

// pageContent is what a public page fetch yields. It is often everything the
// pipeline needs, which is why it is the first rung of the ladder.
type pageContent struct {
	Caption      string
	Title        string
	VideoURL     string
	ImageURLs    []string
	ThumbnailURL string
}

var (
	metaProperty = regexp.MustCompile(`(?i)<meta[^>]+property=["']([^"']+)["'][^>]+content=["']([^"']*)["'][^>]*>`)
	metaName     = regexp.MustCompile(`(?i)<meta[^>]+name=["']([^"']+)["'][^>]+content=["']([^"']*)["'][^>]*>`)
	displayURL   = regexp.MustCompile(`"display_url"\s*:\s*"([^"]+)"`)
	videoURLJSON = regexp.MustCompile(`"video_url"\s*:\s*"([^"]+)"`)
)

func (h *Handler) fetchPage(ctx context.Context, url string) (pageContent, error) {
	response, err := h.deps.HTTP.Get(ctx, url)
	if err != nil {
		return pageContent{}, err
	}
	if response.Status < 200 || response.Status >= 300 {
		return pageContent{}, errStatus(response.Status)
	}
	return parsePage(string(response.Body)), nil
}

// parsePage reads the Open Graph tags first, then the embedded JSON. Both are
// public, and neither needs a session.
func parsePage(body string) pageContent {
	meta := map[string]string{}
	for _, match := range metaProperty.FindAllStringSubmatch(body, -1) {
		meta[strings.ToLower(match[1])] = html.UnescapeString(match[2])
	}
	for _, match := range metaName.FindAllStringSubmatch(body, -1) {
		key := strings.ToLower(match[1])
		if _, exists := meta[key]; !exists {
			meta[key] = html.UnescapeString(match[2])
		}
	}

	page := pageContent{
		Caption:      firstOf(meta["og:description"], meta["description"]),
		Title:        meta["og:title"],
		VideoURL:     firstOf(meta["og:video:secure_url"], meta["og:video"], jsonURL(videoURLJSON, body)),
		ThumbnailURL: meta["og:image"],
	}

	seen := map[string]bool{}
	for _, candidate := range append(imageURLs(body), meta["og:image"]) {
		cleaned := strings.TrimSpace(candidate)
		if cleaned == "" || seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		page.ImageURLs = append(page.ImageURLs, cleaned)
	}
	if page.ThumbnailURL == "" && len(page.ImageURLs) > 0 {
		page.ThumbnailURL = page.ImageURLs[0]
	}
	return page
}

func imageURLs(body string) []string {
	urls := []string{}
	for _, match := range displayURL.FindAllStringSubmatch(body, -1) {
		urls = append(urls, decodeJSONString(match[1]))
	}
	// Slides appear in order in the page, and the order is the carousel's.
	sort.SliceStable(urls, func(i, j int) bool { return false })
	return urls
}

func jsonURL(pattern *regexp.Regexp, body string) string {
	match := pattern.FindStringSubmatch(body)
	if match == nil {
		return ""
	}
	return decodeJSONString(match[1])
}

// decodeJSONString undoes the escaping Instagram's embedded JSON uses.
func decodeJSONString(value string) string {
	replacer := strings.NewReplacer(`\/`, "/", `&`, "&", `\"`, `"`, `\\`, `\`)
	return html.UnescapeString(replacer.Replace(value))
}

func firstOf(values ...string) string {
	for _, value := range values {
		if cleaned := strings.TrimSpace(value); cleaned != "" {
			return cleaned
		}
	}
	return ""
}

type statusError int

func (s statusError) Error() string { return "instagram page fetch returned a non-success status" }

func errStatus(status int) error { return statusError(status) }
