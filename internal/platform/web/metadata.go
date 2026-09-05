// Package web prepares everything that is not a first-class social platform:
// YouTube, TikTok, Pinterest, curated place links, and any other page a user
// shares. They differ in where the text comes from, not in what happens next,
// so they share one handler with per-platform behaviour.
package web

import (
	"context"
	"html"
	"regexp"
	"strings"

	"github.com/XploY04/reelpin-go/internal/safehttp"
)

// Metadata is what a page says about itself. For a place link or an article it
// is the whole payload; for a video it is the caption around the media.
type Metadata struct {
	Title       string
	Description string
	ImageURL    string
	VideoURL    string
	SiteName    string
}

var (
	metaProperty = regexp.MustCompile(`(?is)<meta[^>]+property=["']([^"']+)["'][^>]+content=["']([^"']*)["'][^>]*>`)
	metaName     = regexp.MustCompile(`(?is)<meta[^>]+name=["']([^"']+)["'][^>]+content=["']([^"']*)["'][^>]*>`)
	titleTag     = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	tagStripper  = regexp.MustCompile(`(?s)<[^>]*>`)
)

// ParseMetadata reads Open Graph first, then Twitter cards, then the plain
// title. Every source here is what a page publishes for link previews.
func ParseMetadata(body string) Metadata {
	values := map[string]string{}
	for _, match := range metaProperty.FindAllStringSubmatch(body, -1) {
		values[strings.ToLower(strings.TrimSpace(match[1]))] = html.UnescapeString(match[2])
	}
	for _, match := range metaName.FindAllStringSubmatch(body, -1) {
		key := strings.ToLower(strings.TrimSpace(match[1]))
		if _, exists := values[key]; !exists {
			values[key] = html.UnescapeString(match[2])
		}
	}

	metadata := Metadata{
		Title:       firstOf(values["og:title"], values["twitter:title"], plainTitle(body)),
		Description: firstOf(values["og:description"], values["twitter:description"], values["description"]),
		ImageURL:    firstOf(values["og:image"], values["og:image:secure_url"], values["twitter:image"]),
		VideoURL:    firstOf(values["og:video:secure_url"], values["og:video"], values["og:video:url"]),
		SiteName:    values["og:site_name"],
	}
	return metadata
}

// Usable reports whether a page said anything worth saving. A page with no
// title, description or image is terminal: there is nothing to extract.
func (m Metadata) Usable() bool {
	return strings.TrimSpace(m.Title) != "" ||
		strings.TrimSpace(m.Description) != "" ||
		strings.TrimSpace(m.ImageURL) != ""
}

// Text is what the extractor reads for a page with no media.
func (m Metadata) Text() string {
	parts := []string{}
	for _, part := range []string{m.Title, m.Description} {
		if cleaned := strings.TrimSpace(part); cleaned != "" {
			parts = append(parts, cleaned)
		}
	}
	return strings.Join(parts, "\n\n")
}

func fetchMetadata(ctx context.Context, client *safehttp.Client, url string) (Metadata, error) {
	response, err := client.Get(ctx, url)
	if err != nil {
		return Metadata{}, err
	}
	if response.Status < 200 || response.Status >= 300 {
		return Metadata{}, errStatus(response.Status)
	}
	return ParseMetadata(string(response.Body)), nil
}

func plainTitle(body string) string {
	match := titleTag.FindStringSubmatch(body)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(tagStripper.ReplaceAllString(match[1], "")))
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

func (s statusError) Error() string { return "the page returned a non-success status" }

func errStatus(status int) error { return statusError(status) }
