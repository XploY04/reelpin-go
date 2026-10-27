package social

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/apify"
	"github.com/XploY04/reelpin-go/internal/pipeline"
	"github.com/XploY04/reelpin-go/internal/platform/platformtest"
)

const linkedinPostURL = "https://www.linkedin.com/feed/update/urn:li:activity:7100000000000000000/"

func TestALinkedInPostIsReadThroughTheActor(t *testing.T) {
	server := site(t, func(http.ResponseWriter, *http.Request) string {
		return "image-bytes"
	})

	uploader := &platformtest.Uploader{}
	deps := testDeps()
	deps.Thumbnails.Storage = uploader
	actor := &fakeActor{
		configured: map[string]bool{linkedinActor: true},
		items:      actorItems(t, "linkedin_actor.json", server.URL),
	}
	deps.Apify = actor

	prepared, err := NewLinkedIn(deps).Prepare(context.Background(),
		identity("linkedin", "post", "7100000000000000000", linkedinPostURL))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if prepared.NeedsMedia {
		t.Error("a LinkedIn post asked for media")
	}
	if !strings.HasPrefix(prepared.Caption, "Priya Nair: ") {
		t.Errorf("caption = %q, want the author attributed", prepared.Caption)
	}
	if !strings.Contains(prepared.PageText, "back-pressure") {
		t.Errorf("page text = %q", prepared.PageText)
	}
	if !strings.Contains(prepared.PageText, "write-up coming") {
		t.Error("the discussion was dropped")
	}
	if strings.Contains(prepared.PageText, "\n\n\n") {
		t.Error("a blank comment became an empty paragraph")
	}
	if uploader.Uploads != 1 || prepared.ThumbnailURL == "" {
		t.Errorf("uploaded %d images, thumbnail %q", uploader.Uploads, prepared.ThumbnailURL)
	}
	if actor.runs != 1 {
		t.Errorf("the actor ran %d times for one post", actor.runs)
	}
}

func TestALinkedInPageCostsNoActorRun(t *testing.T) {
	// A profile, company or article publishes ordinary link-preview tags.
	// Paying an actor to read what the page already gives away is waste.
	server := site(t, func(http.ResponseWriter, *http.Request) string {
		return fixture(t, "linkedin_page.html")
	})

	actor := &fakeActor{configured: map[string]bool{linkedinActor: true}}
	uploader := &platformtest.Uploader{}
	deps := testDeps()
	deps.Apify = actor
	deps.Thumbnails.Storage = uploader

	prepared, err := NewLinkedIn(deps).Prepare(context.Background(),
		identity("linkedin", "profile", "priya-nair", server.URL))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if actor.runs != 0 {
		t.Fatal("a page that publishes its own metadata still cost an actor run")
	}
	if !strings.Contains(prepared.Caption, "Priya Nair") {
		t.Errorf("caption = %q", prepared.Caption)
	}
	if !strings.Contains(prepared.PageText, "Previously infrastructure") {
		t.Errorf("the page body was not read: %q", prepared.PageText)
	}
	if strings.Contains(prepared.PageText, "__data") {
		t.Error("script content reached the extractor")
	}
	// Stored, not linked. A page's og:image is a LinkedIn CDN URL, and the web
	// only renders images from this project's own bucket, so linking it is the
	// same as having no thumbnail at all.
	if !strings.HasPrefix(prepared.ThumbnailURL, platformtest.StoredPrefix) {
		t.Errorf("thumbnail = %q, want a stored one", prepared.ThumbnailURL)
	}
	if uploader.Uploads != 1 {
		t.Errorf("uploaded %d previews, want 1", uploader.Uploads)
	}
}

func TestAPostWithoutTheActorIsRetryableNotTerminal(t *testing.T) {
	// A missing credential is a deploy away from being fixed, so the run is
	// not told the content is unreadable.
	deps := testDeps()
	deps.Apify = &fakeActor{configured: map[string]bool{}}

	_, err := NewLinkedIn(deps).Prepare(context.Background(),
		identity("linkedin", "post", "7100000000000000000", linkedinPostURL))

	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want the configuration gap named", err)
	}
	failure := failureOf(t, err)
	if !failure.Retryable() {
		t.Error("a missing credential was reported as permanently unreadable")
	}
	if failure.Class != pipeline.Transient {
		t.Errorf("class = %v", failure.Class)
	}
}

func TestAThrottledLinkedInActorIsProviderExhausted(t *testing.T) {
	deps, logs := logged()
	deps.Apify = &fakeActor{
		configured: map[string]bool{linkedinActor: true},
		err:        apify.ErrRateLimited,
	}

	_, err := NewLinkedIn(deps).Prepare(context.Background(),
		identity("linkedin", "post", "7100000000000000000", linkedinPostURL))

	failure := failureOf(t, err)
	if failure.Class != pipeline.ProviderExhausted {
		t.Fatalf("class = %v, want provider exhausted so the cooldown holds the queue", failure.Class)
	}
	if strings.Contains(logs.String(), "linkedin.com") {
		t.Errorf("the post URL reached the log: %s", logs.String())
	}
}

func TestAnEmptyActorResultIsTerminal(t *testing.T) {
	deps := testDeps()
	deps.Apify = &fakeActor{
		configured: map[string]bool{linkedinActor: true},
		items:      actorItems(t, "linkedin_actor.json", "http://127.0.0.1:1")[:0],
	}

	_, err := NewLinkedIn(deps).Prepare(context.Background(),
		identity("linkedin", "post", "7100000000000000000", linkedinPostURL))

	if !errors.Is(err, ErrNoPublicContent) {
		t.Fatalf("err = %v", err)
	}
	if failureOf(t, err).Retryable() {
		t.Error("an empty result was made retryable")
	}
}

func TestALinkedInPageWithNothingToSayIsTerminal(t *testing.T) {
	server := site(t, func(http.ResponseWriter, *http.Request) string {
		return "<html><head></head><body></body></html>"
	})

	_, err := NewLinkedIn(testDeps()).Prepare(context.Background(),
		identity("linkedin", "company", "somewhere", server.URL))

	failure := failureOf(t, err)
	if failure.Retryable() {
		t.Error("an empty page was made retryable")
	}
	if failure.Code != "page_empty" {
		t.Errorf("code = %q", failure.Code)
	}
}
