package sourceidentity

import (
	"context"
	"errors"
	"net/url"
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
		wantPublic      bool
	}{
		{
			name:         "instagram reel drops tracking parameters",
			url:          "https://www.instagram.com/reel/C8abc123/?utm_source=ig_web_copy_link&igsh=abc",
			wantPlatform: "instagram", wantContentType: "reel", wantContentID: "C8abc123",
			wantNormalized: "https://www.instagram.com/reel/C8abc123/", wantPublic: true,
		},
		{
			name:         "instagram reels plural is the same identity",
			url:          "https://www.instagram.com/reels/C8abc123/",
			wantPlatform: "instagram", wantContentType: "reel", wantContentID: "C8abc123",
			wantNormalized: "https://www.instagram.com/reel/C8abc123/", wantPublic: true,
		},
		{
			name:         "instagram post through the short host",
			url:          "https://instagr.am/p/C8xyz999/?igsh=foo",
			wantPlatform: "instagram", wantContentType: "post", wantContentID: "C8xyz999",
			wantNormalized: "https://www.instagram.com/p/C8xyz999/", wantPublic: true,
		},
		{
			name:         "instagram tv",
			url:          "https://www.instagram.com/tv/C8tv0001/",
			wantPlatform: "instagram", wantContentType: "video", wantContentID: "C8tv0001",
			wantNormalized: "https://www.instagram.com/tv/C8tv0001/", wantPublic: true,
		},
		{
			name:         "instagram profile is a user-scoped page",
			url:          "https://www.instagram.com/somecreator/",
			wantPlatform: "instagram", wantContentType: "page", wantContentID: "",
			wantNormalized: "https://www.instagram.com/somecreator",
		},
		{
			name:         "youtube short",
			url:          "https://www.youtube.com/shorts/abc123XYZ09?si=share",
			wantPlatform: "youtube", wantContentType: "short", wantContentID: "abc123XYZ09",
			wantNormalized: "https://www.youtube.com/shorts/abc123XYZ09", wantPublic: true,
		},
		{
			name:         "youtu.be is treated as a short",
			url:          "https://youtu.be/abc123XYZ09?t=30",
			wantPlatform: "youtube", wantContentType: "short", wantContentID: "abc123XYZ09",
			wantNormalized: "https://www.youtube.com/shorts/abc123XYZ09", wantPublic: true,
		},
		{
			name:         "youtube watch keeps the video id",
			url:          "https://www.youtube.com/watch?v=abc123XYZ09&feature=share&t=12",
			wantPlatform: "youtube", wantContentType: "video", wantContentID: "abc123XYZ09",
			wantNormalized: "https://www.youtube.com/watch?v=abc123XYZ09", wantPublic: true,
		},
		{
			name:         "tiktok video",
			url:          "https://www.tiktok.com/@creator/video/1234567890?share=true",
			wantPlatform: "tiktok", wantContentType: "video", wantContentID: "1234567890",
			wantNormalized: "https://www.tiktok.com/@creator/video/1234567890?share=true", wantPublic: true,
		},
		{
			name:         "x post",
			url:          "https://x.com/someone/status/1234567890123456789?s=20",
			wantPlatform: "x", wantContentType: "post", wantContentID: "1234567890123456789",
			wantNormalized: "https://x.com/someone/status/1234567890123456789", wantPublic: true,
		},
		{
			name:         "twitter.com is the same identity as x.com",
			url:          "https://twitter.com/someone/status/1234567890123456789",
			wantPlatform: "x", wantContentType: "post", wantContentID: "1234567890123456789",
			wantNormalized: "https://x.com/someone/status/1234567890123456789", wantPublic: true,
		},
		{
			name:         "x photo suffix names the same post",
			url:          "https://x.com/someone/status/1234567890123456789/photo/1",
			wantPlatform: "x", wantContentType: "post", wantContentID: "1234567890123456789",
			wantNormalized: "https://x.com/someone/status/1234567890123456789", wantPublic: true,
		},
		{
			name:         "x web status without a username",
			url:          "https://x.com/i/web/status/1234567890123456789",
			wantPlatform: "x", wantContentType: "post", wantContentID: "1234567890123456789",
			wantNormalized: "https://x.com/i/web/status/1234567890123456789", wantPublic: true,
		},
		{
			name:         "linkedin activity post keeps its urn type",
			url:          "https://www.linkedin.com/feed/update/urn:li:activity:7123456789012345678/",
			wantPlatform: "linkedin", wantContentType: "post", wantContentID: "7123456789012345678",
			wantNormalized: "https://www.linkedin.com/feed/update/urn:li:activity:7123456789012345678/", wantPublic: true,
		},
		{
			name:         "linkedin ugcPost keeps its own urn type",
			url:          "https://www.linkedin.com/posts/someone_ugcPost-7123456789012345678-abcd/",
			wantPlatform: "linkedin", wantContentType: "post", wantContentID: "7123456789012345678",
			wantNormalized: "https://www.linkedin.com/feed/update/urn:li:ugcPost:7123456789012345678/", wantPublic: true,
		},
		{
			name:         "linkedin profile falls through to a user-scoped page",
			url:          "https://www.linkedin.com/in/someone/",
			wantPlatform: "linkedin", wantContentType: "profile", wantContentID: "someone",
			wantNormalized: "https://www.linkedin.com/in/someone",
		},
		{
			name:         "reddit comments permalink",
			url:          "https://www.reddit.com/r/goa/comments/1abc234/best_cafes/",
			wantPlatform: "reddit", wantContentType: "post", wantContentID: "1abc234",
			wantNormalized: "https://www.reddit.com/comments/1abc234/", wantPublic: true,
		},
		{
			name:         "redd.it short link",
			url:          "https://redd.it/1abc234",
			wantPlatform: "reddit", wantContentType: "post", wantContentID: "1abc234",
			wantNormalized: "https://www.reddit.com/comments/1abc234/", wantPublic: true,
		},
		{
			name:         "pinterest pin",
			url:          "https://www.pinterest.com/pin/1234567890/?utm_medium=share",
			wantPlatform: "pinterest", wantContentType: "pin", wantContentID: "1234567890",
			wantNormalized: "https://www.pinterest.com/pin/1234567890/", wantPublic: true,
		},
		{
			name:         "google maps place is a user-scoped place link",
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
			if identity.Scope.IsPublic() != tt.wantPublic {
				t.Errorf("scope public = %v, want %v", identity.Scope.IsPublic(), tt.wantPublic)
			}
		})
	}
}

func TestAccessScope(t *testing.T) {
	public, err := PublicScope().Hash()
	if err != nil || public != "public" {
		t.Fatalf("public hash = %q, %v", public, err)
	}
	// Public content deduplicates regardless of who shared it.
	if got := PublicScope().ForUser("user-1"); !got.IsPublic() {
		t.Error("qualifying a public scope with a user made it private")
	}

	// A user scope with no user is an error, never a shared bucket.
	if _, err := UserScope("").Hash(); err == nil {
		t.Fatal("an unqualified user scope produced a hash; every such save would share one identity")
	}

	one, err := UserScope("").ForUser("user-1").Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	two, err := UserScope("").ForUser("user-2").Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if one == two {
		t.Error("two users share one scope hash: private content would deduplicate across them")
	}
	if one == "public" || strings.Contains(one, "user-1") {
		t.Errorf("scope hash %q leaks its input or collides with the public scope", one)
	}

	same, _ := UserScope("user-1").Hash()
	if same != one {
		t.Error("the same user produced two different scope hashes")
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
		{name: "over the length cap", url: "https://example.com/" + strings.Repeat("a", MaxURLLength)},
		{name: "credentials in the url", url: "https://user:secret@example.com/x"},
		{name: "ipv6 zone identifier", url: "http://[fe80::1%25eth0]/x"},
		{name: "non-standard port", url: "https://example.com:8080/x"},
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
}

func TestPercentEncodingIsCanonical(t *testing.T) {
	// %7E and ~ are the same path; the identity must not depend on which form
	// the sharing app produced.
	encoded, err := Resolve("https://example.com/%7Euser/page")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	plain, err := Resolve("https://example.com/~user/page")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if encoded.NormalizedURL != plain.NormalizedURL || encoded.ContentID != plain.ContentID {
		t.Errorf("%q and %q are two identities", encoded.NormalizedURL, plain.NormalizedURL)
	}
}

func TestDefaultPortsAndFragmentsAreRemoved(t *testing.T) {
	withPort, err := Resolve("https://example.com:443/page#section")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	bare, err := Resolve("https://example.com/page")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if withPort.NormalizedURL != bare.NormalizedURL {
		t.Errorf("%q and %q differ", withPort.NormalizedURL, bare.NormalizedURL)
	}
	if strings.Contains(withPort.NormalizedURL, "#") || strings.Contains(withPort.NormalizedURL, ":443") {
		t.Errorf("normalized url %q keeps a default port or fragment", withPort.NormalizedURL)
	}
}

func TestQueryValuesAreSorted(t *testing.T) {
	first, _ := Resolve("https://example.com/p?tag=b&tag=a")
	second, _ := Resolve("https://example.com/p?tag=a&tag=b")
	if first.NormalizedURL != second.NormalizedURL {
		t.Errorf("repeated query values in a different order changed the identity: %q vs %q",
			first.NormalizedURL, second.NormalizedURL)
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

func TestResolvePayload(t *testing.T) {
	resolver := &Resolver{}

	t.Run("messy blob", func(t *testing.T) {
		identity, err := resolver.ResolvePayload(context.Background(),
			"Great tips 👇 https://lnkd.in/tracking then "+
				"https://www.linkedin.com/feed/update/urn:li:activity:123456789012/ cheers")
		if err != nil {
			t.Fatalf("ResolvePayload: %v", err)
		}
		if identity.Platform != "linkedin" || identity.ContentID != "123456789012" {
			t.Fatalf("identity = %+v", identity)
		}
	})

	t.Run("no link", func(t *testing.T) {
		if _, err := resolver.ResolvePayload(context.Background(), "just some text, no link"); !errors.Is(err, ErrUnsupportedURL) {
			t.Fatalf("error = %v, want ErrUnsupportedURL", err)
		}
	})

	t.Run("unresolvable shortener", func(t *testing.T) {
		if _, err := resolver.ResolvePayload(context.Background(), "check https://t.co/abc123"); !errors.Is(err, ErrUnsupportedURL) {
			t.Fatalf("error = %v, want ErrUnsupportedURL", err)
		}
	})
}

// FuzzResolve holds two properties over arbitrary input: never panic, and any
// accepted identity is a well-formed https URL with a platform.
func FuzzResolve(f *testing.F) {
	for _, seed := range []string{
		"https://www.instagram.com/reel/C8abc123/",
		"https://x.com/i/web/status/123",
		"someblog.com/article?a=1&a=2",
		"http://[fe80::1%25eth0]/x",
		"https://user:pass@example.com",
		"https://example.com:8080/x",
		"ftp://example.com",
		"://///", "%%%", strings.Repeat("a", 3000),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		identity, err := Resolve(raw)
		if err != nil {
			return
		}
		parsed, parseErr := url.Parse(identity.NormalizedURL)
		if parseErr != nil {
			t.Fatalf("accepted %q but normalized to unparseable %q", raw, identity.NormalizedURL)
		}
		if parsed.Scheme != "https" {
			t.Fatalf("normalized scheme = %q for %q", parsed.Scheme, raw)
		}
		if parsed.User != nil {
			t.Fatalf("normalized url %q kept credentials", identity.NormalizedURL)
		}
		if identity.Platform == "" {
			t.Fatalf("accepted %q with no platform", raw)
		}
		if len(raw) > 0 && len(strings.TrimSpace(raw)) > MaxURLLength {
			t.Fatalf("accepted a url over the length cap: %d", len(raw))
		}
	})
}

// FuzzExtractURLCandidates: share payloads come straight from other apps, so
// the extractor must never panic and never return an empty candidate.
func FuzzExtractURLCandidates(f *testing.F) {
	f.Add("check https://x.com/a/status/1 and www.b.com)")
	f.Add(strings.Repeat("www.a.com ", 2000))
	f.Add("")

	f.Fuzz(func(t *testing.T, payload string) {
		for _, candidate := range ExtractURLCandidates(payload) {
			if candidate == "" {
				t.Fatal("an empty candidate")
			}
		}
	})
}
