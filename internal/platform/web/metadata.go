// Package web reads a page for what it says about itself: Open Graph, Twitter
// cards, the title, and the readable body text. Every other light handler in
// this tree reuses this parser rather than growing its own, because a
// Pinterest pin, a restaurant listing and a news article all publish the same
// link preview tags.
package web

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/XploY04/reelpin-go/internal/safehttp"
)

// MaxPageTextRunes bounds what reaches a model. A page can be megabytes of
// boilerplate; the extractor pays per token, and nothing past the first few
// thousand words changes what a saved link is about.
const MaxPageTextRunes = 20000

// Metadata is what a page publishes about itself. For an article or a place
// listing it is the whole payload; for a video page it is the caption around
// media that lives elsewhere.
type Metadata struct {
	Title       string
	Description string
	ImageURL    string
	VideoURL    string
	SiteName    string
	// Canonical is the publisher's own preferred address. A share often
	// arrives with tracking parameters or an AMP prefix.
	Canonical string
}

var (
	metaProperty  = regexp.MustCompile(`(?is)<meta[^>]+property=["']([^"']+)["'][^>]+content=["']([^"']*)["'][^>]*>`)
	metaName      = regexp.MustCompile(`(?is)<meta[^>]+name=["']([^"']+)["'][^>]+content=["']([^"']*)["'][^>]*>`)
	linkCanonical = regexp.MustCompile(`(?is)<link[^>]+rel=["']canonical["'][^>]+href=["']([^"']*)["'][^>]*>`)
	titleTag      = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	scriptOrStyle = regexp.MustCompile(`(?is)<(script|style|noscript)[^>]*>.*?</(script|style|noscript)>`)
	tagStripper   = regexp.MustCompile(`(?s)<[^>]*>`)
	inlineSpace   = regexp.MustCompile(`[ \t\x{00a0}]+`)
	blankLines    = regexp.MustCompile(`\n{3,}`)
)

// ParseMetadata reads Open Graph first, then Twitter cards, then the plain
// title. Every source here is what the page publishes for link previews, so a
// page that renders its content in JavaScript still yields something useful.
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
	if match := linkCanonical.FindStringSubmatch(body); match != nil {
		metadata.Canonical = strings.TrimSpace(html.UnescapeString(match[1]))
	}
	if metadata.Canonical == "" {
		metadata.Canonical = strings.TrimSpace(values["og:url"])
	}
	return metadata
}

// Usable reports whether the page said anything worth saving. A page with no
// title, description or image is terminal: there is nothing to extract, and
// another attempt reads the same nothing.
func (m Metadata) Usable() bool {
	return strings.TrimSpace(m.Title) != "" ||
		strings.TrimSpace(m.Description) != "" ||
		strings.TrimSpace(m.ImageURL) != ""
}

// Caption is the author's own words: the title and description the publisher
// wrote for sharing.
func (m Metadata) Caption() string {
	parts := []string{}
	for _, part := range []string{m.Title, m.Description} {
		if cleaned := strings.TrimSpace(part); cleaned != "" {
			parts = append(parts, cleaned)
		}
	}
	return strings.Join(parts, "\n\n")
}

// ReadableText strips markup and returns the page's prose, bounded. Scripts
// and styles go first: they are the bulk of a modern page and none of its
// meaning.
func ReadableText(body string) string {
	stripped := scriptOrStyle.ReplaceAllString(body, " ")
	stripped = tagStripper.ReplaceAllString(stripped, " ")
	stripped = html.UnescapeString(stripped)

	lines := []string{}
	for _, line := range strings.Split(stripped, "\n") {
		if cleaned := strings.TrimSpace(inlineSpace.ReplaceAllString(line, " ")); cleaned != "" {
			lines = append(lines, cleaned)
		}
	}
	text := blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return truncateRunes(text, MaxPageTextRunes)
}

// truncateRunes cuts on a rune boundary, so a multibyte character is never
// split into invalid UTF-8 on its way to a model.
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit]))
}

// Fetch reads one page through the safe client and parses it. The client caps
// the body, checks every resolved address and re-checks after each redirect,
// so no handler needs its own network rules.
func Fetch(ctx context.Context, client *safehttp.Client, rawURL string) (Metadata, string, error) {
	response, err := client.Get(ctx, rawURL)
	if err != nil {
		return Metadata{}, "", err
	}
	if response.Status < 200 || response.Status >= 300 {
		return Metadata{}, "", &StatusError{Status: response.Status}
	}
	body := string(response.Body)
	return ParseMetadata(body), body, nil
}

// StatusError is a page that answered, but not with a page. The status is
// carried because it decides whether another attempt could differ.
type StatusError struct {
	Status int
}

func (s *StatusError) Error() string {
	return fmt.Sprintf("the page returned status %d", s.Status)
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
