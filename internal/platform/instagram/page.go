package instagram

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"regexp"
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

// fetchPage reads the public page. It costs nothing but one light HTTP slot,
// which is why every content type starts here.
func (h *Handler) fetchPage(ctx context.Context, url string) (pageContent, error) {
	release, err := h.deps.Limits.AcquireLightHTTP(ctx)
	if err != nil {
		return pageContent{}, err
	}
	defer release()

	response, err := h.deps.HTTP.Get(ctx, url)
	if err != nil {
		return pageContent{}, fmt.Errorf("%w: %v", ErrProviderOutage, err)
	}
	if err := classifyStatus(response.Status); err != nil {
		return pageContent{}, err
	}

	body := string(response.Body)
	if err := classifyBody(body); err != nil {
		return pageContent{}, err
	}
	return parsePage(body), nil
}

// classifyStatus turns the page's HTTP status into one of this handler's
// failures. Instagram answers a signed-out request for private content with a
// login page rather than a 403, so the body check matters as much as this.
func classifyStatus(status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusNotFound, status == http.StatusGone:
		return ErrRemoved
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return ErrLoginWall
	case status >= 500:
		return ErrProviderOutage
	}
	return fmt.Errorf("%w: page status %d", ErrMalformed, status)
}

// bodySignals are the pages Instagram serves instead of content. They are
// matched on the visible copy, which changes rarely, rather than on markup.
var bodySignals = []struct {
	needle string
	err    error
}{
	{"sorry, this page isn't available", ErrRemoved},
	{"the link you followed may be broken", ErrRemoved},
	{"this account is private", ErrPrivate},
	{"log in to see photos and videos", ErrLoginWall},
	{`id="loginform"`, ErrLoginWall},
	{"please wait a few minutes before you try again", ErrRateLimited},
}

func classifyBody(body string) error {
	lowered := strings.ToLower(body)
	for _, signal := range bodySignals {
		if strings.Contains(lowered, signal.needle) {
			return signal.err
		}
	}
	return nil
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

	// Slides appear in the page in the carousel's own order, so the order they
	// are found in is the order they are kept in.
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
	replacer := strings.NewReplacer(`\/`, "/", `\"`, `"`, `\\`, `\`)
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
