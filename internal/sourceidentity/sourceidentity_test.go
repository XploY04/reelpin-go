package sourceidentity

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestResolveIdentity(t *testing.T) {
	tests := []struct {
		name            string
		url             string
		wantPlatform    string
		wantContentType string
		wantContentID   string
		wantNormalized  string
	}{
		{
			name:         "instagram reel drops tracking parameters",
			url:          "https://www.instagram.com/reel/C8abc123/?utm_source=ig_web_copy_link&igsh=abc",
			wantPlatform: "instagram", wantContentType: "reel", wantContentID: "C8abc123",
			wantNormalized: "https://www.instagram.com/reel/C8abc123/",
		},
		{
			name:         "instagram reels plural is the same identity",
			url:          "https://www.instagram.com/reels/C8abc123/",
			wantPlatform: "instagram", wantContentType: "reel", wantContentID: "C8abc123",
			wantNormalized: "https://www.instagram.com/reel/C8abc123/",
		},
		{
			name:         "instagram post through the short host",
			url:          "https://instagr.am/p/C8xyz999/?igsh=foo",
			wantPlatform: "instagram", wantContentType: "post", wantContentID: "C8xyz999",
			wantNormalized: "https://www.instagram.com/p/C8xyz999/",
		},
		{
			name:         "instagram tv",
			url:          "https://www.instagram.com/tv/C8tv0001/",
			wantPlatform: "instagram", wantContentType: "video", wantContentID: "C8tv0001",
			wantNormalized: "https://www.instagram.com/tv/C8tv0001/",
		},
		{
			name:         "instagram profile is a page",
			url:          "https://www.instagram.com/somecreator/",
			wantPlatform: "instagram", wantContentType: "page", wantContentID: "",
			wantNormalized: "https://www.instagram.com/somecreator",
		},
		{
			name:         "youtube short",
			url:          "https://www.youtube.com/shorts/abc123XYZ09?si=share",
			wantPlatform: "youtube", wantContentType: "short", wantContentID: "abc123XYZ09",
			wantNormalized: "https://www.youtube.com/shorts/abc123XYZ09",
		},
		{
			name:         "youtu.be is treated as a short",
			url:          "https://youtu.be/abc123XYZ09?t=30",
			wantPlatform: "youtube", wantContentType: "short", wantContentID: "abc123XYZ09",
			wantNormalized: "https://www.youtube.com/shorts/abc123XYZ09",
		},
		{
			name:         "youtube watch keeps the video id",
			url:          "https://www.youtube.com/watch?v=abc123XYZ09&feature=share&t=12",
			wantPlatform: "youtube", wantContentType: "video", wantContentID: "abc123XYZ09",
			wantNormalized: "https://www.youtube.com/watch?v=abc123XYZ09",
		},
		{
			name:         "tiktok video",
			url:          "https://www.tiktok.com/@creator/video/1234567890?share=true",
			wantPlatform: "tiktok", wantContentType: "video", wantContentID: "1234567890",
			wantNormalized: "https://www.tiktok.com/@creator/video/1234567890?share=true",
		},
		{
			name:         "x post",
			url:          "https://x.com/someone/status/1234567890123456789?s=20",
			wantPlatform: "x", wantContentType: "post", wantContentID: "1234567890123456789",
			wantNormalized: "https://x.com/someone/status/1234567890123456789",
		},
		{
			name:         "twitter.com is the same identity as x.com",
			url:          "https://twitter.com/someone/status/1234567890123456789",
			wantPlatform: "x", wantContentType: "post", wantContentID: "1234567890123456789",
			wantNormalized: "https://x.com/someone/status/1234567890123456789",
		},
		{
			name:         "x photo suffix names the same post",
			url:          "https://x.com/someone/status/1234567890123456789/photo/1",
			wantPlatform: "x", wantContentType: "post", wantContentID: "1234567890123456789",
			wantNormalized: "https://x.com/someone/status/1234567890123456789",
		},
		{
			name:         "x web status without a username",
			url:          "https://x.com/i/web/status/1234567890123456789",
			wantPlatform: "x", wantContentType: "post", wantContentID: "1234567890123456789",
			wantNormalized: "https://x.com/i/web/status/1234567890123456789",
		},
		{
			name:         "linkedin activity post keeps its urn type",
			url:          "https://www.linkedin.com/feed/update/urn:li:activity:7123456789012345678/",
			wantPlatform: "linkedin", wantContentType: "post", wantContentID: "7123456789012345678",
			wantNormalized: "https://www.linkedin.com/feed/update/urn:li:activity:7123456789012345678/",
		},
		{
			name:         "linkedin ugcPost keeps its own urn type",
			url:          "https://www.linkedin.com/posts/someone_ugcPost-7123456789012345678-abcd/",
			wantPlatform: "linkedin", wantContentType: "post", wantContentID: "7123456789012345678",
			wantNormalized: "https://www.linkedin.com/feed/update/urn:li:ugcPost:7123456789012345678/",
		},
		{
			name:         "linkedin profile falls through to a page",
			url:          "https://www.linkedin.com/in/someone/",
			wantPlatform: "linkedin", wantContentType: "profile", wantContentID: "someone",
			wantNormalized: "https://www.linkedin.com/in/someone",
		},
		{
			name:         "reddit comments permalink",
			url:          "https://www.reddit.com/r/goa/comments/1abc234/best_cafes/",
			wantPlatform: "reddit", wantContentType: "post", wantContentID: "1abc234",
			wantNormalized: "https://www.reddit.com/comments/1abc234/",
		},
		{
			name:         "redd.it short link",
			url:          "https://redd.it/1abc234",
			wantPlatform: "reddit", wantContentType: "post", wantContentID: "1abc234",
			wantNormalized: "https://www.reddit.com/comments/1abc234/",
		},
		{
			name:         "pinterest pin",
			url:          "https://www.pinterest.com/pin/1234567890/?utm_medium=share",
			wantPlatform: "pinterest", wantContentType: "pin", wantContentID: "1234567890",
			wantNormalized: "https://www.pinterest.com/pin/1234567890/",
		},
		{
			name:         "google maps place is a curated place link",
			url:          "https://maps.app.goo.gl/abcDEF123",
			wantPlatform: "google_maps", wantContentType: "link",
			wantNormalized: "https://maps.app.goo.gl/abcDEF123",
		},
		{
			name:         "tripadvisor is a curated place link",
			url:          "https://www.tripadvisor.in/Restaurant_Review-g123-d456.html",
			wantPlatform: "tripadvisor", wantContentType: "link",
			wantNormalized: "https://www.tripadvisor.in/Restaurant_Review-g123-d456.html",
		},
		{
			name:         "zomato is a curated place link",
			url:          "https://www.zomato.com/goa/artjuna-cafe",
			wantPlatform: "zomato", wantContentType: "link",
			wantNormalized: "https://www.zomato.com/goa/artjuna-cafe",
		},
		{
			name:         "generic link keeps its host as the platform",
			url:          "https://someblog.com/Posts/Best-Cafes?page=2&utm_source=news",
			wantPlatform: "someblog.com", wantContentType: "link",
			wantNormalized: "https://someblog.com/Posts/Best-Cafes?page=2",
		},
		{
			name:         "a bare host gets https",
			url:          "someblog.com/article",
			wantPlatform: "someblog.com", wantContentType: "link",
			wantNormalized: "https://someblog.com/article",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity, err := Resolve(tt.url)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tt.url, err)
			}
			if identity.Platform != tt.wantPlatform {
				t.Errorf("platform = %q, want %q", identity.Platform, tt.wantPlatform)
			}
			if identity.ContentType != tt.wantContentType {
				t.Errorf("content type = %q, want %q", identity.ContentType, tt.wantContentType)
			}
			if tt.wantContentID != "" && identity.ContentID != tt.wantContentID {
				t.Errorf("content id = %q, want %q", identity.ContentID, tt.wantContentID)
			}
			if identity.NormalizedURL != tt.wantNormalized {
				t.Errorf("normalized url = %q, want %q", identity.NormalizedURL, tt.wantNormalized)
			}
			if identity.OriginalURL == "" {
				t.Error("the original url was dropped")
			}
		})
	}
}

func TestAliasesShareOneIdentity(t *testing.T) {
	groups := [][]string{
		{
			"https://www.instagram.com/reel/C8abc123/",
			"https://instagram.com/reel/C8abc123/?igshid=x",
			"https://m.instagram.com/reels/C8abc123",
			"https://instagr.am/reel/C8abc123/?utm_campaign=share",
		},
		{
			"https://x.com/someone/status/123456789012345678",
			"https://twitter.com/someone/status/123456789012345678?s=20",
			"https://mobile.twitter.com/someone/status/123456789012345678",
			"https://x.com/someone/status/123456789012345678/video/1",
		},
		{
			"https://www.reddit.com/r/goa/comments/1abc234/title/",
			"https://reddit.com/comments/1abc234",
			"https://redd.it/1abc234",
		},
	}

	for _, group := range groups {
		t.Run(group[0], func(t *testing.T) {
			first, err := Resolve(group[0])
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			for _, alias := range group[1:] {
				identity, err := Resolve(alias)
				if err != nil {
					t.Fatalf("Resolve(%q): %v", alias, err)
				}
				if identity.NormalizedURL != first.NormalizedURL || identity.ContentID != first.ContentID {
					t.Errorf("%q resolved to %s/%s, want %s/%s",
						alias, identity.NormalizedURL, identity.ContentID,
						first.NormalizedURL, first.ContentID)
				}
			}
		})
	}
}

func TestResolveRejects(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "blank", url: ""},
		{name: "whitespace", url: "   "},
		{name: "no host", url: "https:///path"},
		{name: "non http scheme", url: "ftp://files.example.com/x"},
		{name: "x profile is not a post", url: "https://x.com/someone"},
		{name: "x status without digits", url: "https://x.com/someone/status/abc"},
		{name: "reddit subreddit is not a post", url: "https://www.reddit.com/r/goa/"},
		{name: "t.co without a resolver", url: "https://t.co/abc123"},
		{name: "reddit share link without a resolver", url: "https://www.reddit.com/r/goa/s/abc123/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Resolve(tt.url); !errors.Is(err, ErrUnsupportedURL) {
				t.Fatalf("Resolve(%q) error = %v, want ErrUnsupportedURL", tt.url, err)
			}
		})
	}
}

func TestGenericPathKeepsCaseAndMeaningfulQuery(t *testing.T) {
	identity, err := Resolve("https://Example.com//Deep//Path/?b=2&utm_source=x&a=One&empty=")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if identity.NormalizedURL != "https://example.com/Deep/Path?a=One&b=2" {
		t.Fatalf("normalized url = %q", identity.NormalizedURL)
	}
	if identity.ContentID == "" || len(identity.ContentID) != 16 {
		t.Errorf("content id = %q, want a 16 character hash", identity.ContentID)
	}
	if identity.LegacyContentID == identity.ContentID {
		t.Error("the sha-256 id and the legacy sha-1 id must differ")
	}
}

func TestGenericContentIDsAreStableAndDistinct(t *testing.T) {
	first, _ := Resolve("https://someblog.com/a")
	same, _ := Resolve("https://someblog.com/a/")
	other, _ := Resolve("https://someblog.com/b")

	if first.ContentID != same.ContentID {
		t.Error("a trailing slash changed the identity")
	}
	if first.ContentID == other.ContentID {
		t.Error("two different urls share one identity")
	}
}

type stubRedirects struct {
	destination string
	err         error
}

func (s stubRedirects) ResolveRedirects(context.Context, string) (string, error) {
	return s.destination, s.err
}

type stubReddit struct {
	canonical string
	err       error
	calls     int
}

func (s *stubReddit) ResolveShareLink(context.Context, string) (string, error) {
	s.calls++
	return s.canonical, s.err
}

func TestTCoResolvesThroughRedirects(t *testing.T) {
	resolver := &Resolver{Redirects: stubRedirects{
		destination: "https://x.com/someone/status/1234567890123456789",
	}}

	identity, err := resolver.Resolve(context.Background(), "https://t.co/abc123")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if identity.Platform != "x" || identity.ContentID != "1234567890123456789" {
		t.Fatalf("identity = %+v", identity)
	}
	if identity.OriginalURL != "https://t.co/abc123" {
		t.Errorf("original url = %q, want the shared t.co link", identity.OriginalURL)
	}
}

func TestTCoToSomethingElseIsRejected(t *testing.T) {
	resolver := &Resolver{Redirects: stubRedirects{destination: "https://example.com/landing"}}

	if _, err := resolver.Resolve(context.Background(), "https://t.co/abc123"); !errors.Is(err, ErrUnsupportedURL) {
		t.Fatalf("error = %v, want ErrUnsupportedURL", err)
	}
}

func TestRedditShareLinkUsesTheResolverOnlyForShareShapes(t *testing.T) {
	reddit := &stubReddit{canonical: "https://www.reddit.com/r/goa/comments/1abc234/title/"}
	resolver := &Resolver{Reddit: reddit}

	identity, err := resolver.Resolve(context.Background(), "https://www.reddit.com/r/goa/s/xYz123/")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if identity.ContentID != "1abc234" {
		t.Errorf("content id = %q, want 1abc234", identity.ContentID)
	}
	if reddit.calls != 1 {
		t.Errorf("share resolver called %d times, want 1", reddit.calls)
	}

	// A permalink must never trigger an authenticated request.
	if _, err := resolver.Resolve(context.Background(), "https://www.reddit.com/r/goa/comments/1abc234/title/"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if reddit.calls != 1 {
		t.Errorf("share resolver called %d times, want it left alone for permalinks", reddit.calls)
	}
}

func TestExtractURLCandidates(t *testing.T) {
	payload := "Loved this! https://www.instagram.com/reel/abc123/ " +
		"(more at https://bit.ly/xyz). also www.linktr.ee/me " +
		"https://www.instagram.com/reel/abc123/"

	got := ExtractURLCandidates(payload)
	want := []string{
		"https://www.instagram.com/reel/abc123/",
		"https://bit.ly/xyz",
		"www.linktr.ee/me",
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	}
	if len(ExtractURLCandidates("no links here")) != 0 {
		t.Error("text without links produced candidates")
	}
}

func TestSelectPrimaryURL(t *testing.T) {
	tests := []struct {
		name       string
		candidates []string
		want       string
	}{
		{name: "none", candidates: nil, want: ""},
		{name: "single", candidates: []string{"https://x.com/a/status/1"}, want: "https://x.com/a/status/1"},
		{
			name:       "a leading shortener is stepped past",
			candidates: []string{"https://bit.ly/xyz", "https://www.instagram.com/reel/abc/", "linktr.ee/me"},
			want:       "https://www.instagram.com/reel/abc/",
		},
		{
			name:       "order wins over a recognised host later in the payload",
			candidates: []string{"https://someblog.com/article", "https://instagram.com/mybio"},
			want:       "https://someblog.com/article",
		},
		{
			name:       "all shorteners falls back to the first",
			candidates: []string{"https://t.co/a", "https://lnkd.in/b"},
			want:       "https://t.co/a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SelectPrimaryURL(tt.candidates); got != tt.want {
				t.Errorf("SelectPrimaryURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveSharePayload(t *testing.T) {
	resolver := &Resolver{}

	t.Run("messy blob", func(t *testing.T) {
		response := resolver.ResolveSharePayload(context.Background(),
			"Great tips 👇 https://lnkd.in/tracking then "+
				"https://www.linkedin.com/feed/update/urn:li:activity:123456789012/ cheers")

		if !response.Supported {
			t.Fatalf("supported = false: %+v", response)
		}
		if response.Provider == nil || *response.Provider != "linkedin" {
			t.Errorf("provider = %v, want linkedin", response.Provider)
		}
		if response.NormalizedURL == nil || !strings.Contains(*response.NormalizedURL, "urn:li:activity:123456789012") {
			t.Errorf("normalized url = %v", response.NormalizedURL)
		}
	})

	t.Run("no link", func(t *testing.T) {
		response := resolver.ResolveSharePayload(context.Background(), "just some text, no link")
		if response.Supported {
			t.Fatal("supported = true for a payload with no link")
		}
		if response.ErrorMessage == nil || *response.ErrorMessage != "No link was found in the shared content." {
			t.Errorf("error message = %v", response.ErrorMessage)
		}
		if response.ExtractedURL != nil {
			t.Errorf("extracted url = %v, want null", response.ExtractedURL)
		}
	})

	t.Run("unsupported provider", func(t *testing.T) {
		response := resolver.ResolveSharePayload(context.Background(), "check https://t.co/abc123")
		if response.Supported {
			t.Fatal("a t.co link resolved without a redirect resolver")
		}
		if response.Provider == nil || *response.Provider != "web" {
			t.Errorf("provider = %v, want web", response.Provider)
		}
		if response.ExtractedURL == nil || *response.ExtractedURL != "https://t.co/abc123" {
			t.Errorf("extracted url = %v", response.ExtractedURL)
		}
	})
}
