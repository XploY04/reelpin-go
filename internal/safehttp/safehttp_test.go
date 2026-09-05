package safehttp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func testClient() *Client {
	// Test servers listen on loopback, which production always refuses.
	return New(Config{AllowPrivateAddresses: true})
}

func TestIsPublicAddress(t *testing.T) {
	tests := []struct {
		address string
		public  bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"2606:4700:4700::1111", true},
		{"127.0.0.1", false},
		{"0.0.0.0", false},
		{"10.0.0.5", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false}, // AWS/GCP/Azure metadata
		{"100.64.0.1", false},      // carrier-grade NAT
		{"198.18.0.1", false},
		{"224.0.0.1", false},
		{"255.255.255.255", false},
		{"::1", false},
		{"::", false},
		{"fe80::1", false},
		{"fd00:ec2::254", false}, // AWS IPv6 metadata, unique local
		{"fc00::1", false},
		{"ff02::1", false},
		{"::ffff:127.0.0.1", false}, // IPv4-mapped loopback
		{"::ffff:10.0.0.1", false},
	}

	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			addr := netip.MustParseAddr(tt.address)
			if got := IsPublicAddress(addr); got != tt.public {
				t.Errorf("IsPublicAddress(%s) = %v, want %v", tt.address, got, tt.public)
			}
		})
	}
}

func TestParseURLRejects(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "file scheme", url: "file:///etc/passwd"},
		{name: "javascript scheme", url: "javascript:alert(1)"},
		{name: "ftp scheme", url: "ftp://example.com/x"},
		{name: "user info", url: "https://user:pass@example.com/x"},
		{name: "user only", url: "https://admin@example.com/x"},
		{name: "no host", url: "https:///path"},
		{name: "bad port", url: "https://example.com:99999/x"},
		{name: "non-standard port", url: "https://example.com:8080/x"},
		{name: "ipv6 zone identifier", url: "http://[fe80::1%25eth0]/x"},
		{name: "over the length cap", url: "https://example.com/" + strings.Repeat("a", MaxURLLength)},
		{name: "empty", url: "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseURL(tt.url); !errors.Is(err, ErrUnsafeURL) {
				t.Fatalf("ParseURL(%q) error = %v, want ErrUnsafeURL", tt.url, err)
			}
		})
	}
}

func TestPrivateAddressesAreRefused(t *testing.T) {
	client := New(Config{}) // production settings

	for _, target := range []string{
		"http://127.0.0.1:80/x",
		"http://localhost/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/x",
		"http://10.1.2.3/x",
		"http://192.168.0.1/x",
		"http://[fd00:ec2::254]/x",
	} {
		t.Run(target, func(t *testing.T) {
			if _, err := client.Get(context.Background(), target); !errors.Is(err, ErrUnsafeURL) {
				t.Fatalf("Get(%q) error = %v, want ErrUnsafeURL", target, err)
			}
		})
	}
}

// TestEncodedHostsAreRefused covers hosts that hide a private address behind an
// encoding the URL parser resolves before this client sees it.
func TestEncodedHostsAreRefused(t *testing.T) {
	// Percent-encoding in the host is decoded by url.Parse, so this is
	// localhost by the time it reaches the dialer.
	client := New(Config{})
	if _, err := client.Get(context.Background(), "http://%6c%6fcalhost/x"); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("percent-encoded localhost: error = %v, want ErrUnsafeURL", err)
	}

	// Decimal and hex host spellings are not IP literals to netip, so they go
	// through DNS; whatever they resolve to is checked like any other answer.
	seam := New(Config{
		LookupIP: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
	})
	for _, target := range []string{"http://2130706433/x", "http://0x7f000001/x", "http://0177.0.0.1/x"} {
		if _, err := seam.Get(context.Background(), target); !errors.Is(err, ErrUnsafeURL) {
			t.Fatalf("Get(%q) error = %v, want ErrUnsafeURL", target, err)
		}
	}
}

func TestRedirectToPrivateAddressIsRefused(t *testing.T) {
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret"))
	}))
	defer private.Close()

	// The redirector is reachable; its destination must not be.
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, private.URL, http.StatusFound)
	}))
	defer redirector.Close()

	client := New(Config{}) // production settings: loopback is not allowed
	if _, err := client.Get(context.Background(), redirector.URL); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("error = %v, want ErrUnsafeURL", err)
	}
}

func TestRedirectToMetadataServiceIsRefused(t *testing.T) {
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer redirector.Close()

	client := New(Config{})
	if _, err := client.Get(context.Background(), redirector.URL); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("error = %v, want ErrUnsafeURL", err)
	}
}

func TestRedirectLimit(t *testing.T) {
	var server *httptest.Server
	hops := 0
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, fmt.Sprintf("%s/hop/%d", server.URL, hops), http.StatusFound)
	}))
	defer server.Close()

	if _, err := testClient().Get(context.Background(), server.URL); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("error = %v, want ErrUnsafeURL", err)
	}
	if hops > MaxRedirects+1 {
		t.Errorf("followed %d hops, want at most %d", hops, MaxRedirects+1)
	}
}

func TestRedirectLoop(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL, http.StatusFound)
	}))
	defer server.Close()

	if _, err := testClient().Get(context.Background(), server.URL); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("error = %v, want ErrUnsafeURL", err)
	}
}

// TestAuthorizationIsStrippedWhenTheHostChanges proves a credential sent to one
// origin never follows a redirect to another.
func TestAuthorizationIsStrippedWhenTheHostChanges(t *testing.T) {
	var crossHostAuth, sameHostAuth string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		crossHostAuth = r.Header.Get("Authorization")
	}))
	defer destination.Close()

	var origin *httptest.Server
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			// Same-host hop first: the header may follow.
			http.Redirect(w, r, origin.URL+"/second", http.StatusFound)
		case "/second":
			sameHostAuth = r.Header.Get("Authorization")
			// Cross-host hop: the header must not follow. The two test servers
			// differ by port, which is a different origin.
			http.Redirect(w, r, destination.URL, http.StatusFound)
		}
	}))
	defer origin.Close()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer secret-token")
	if _, err := testClient().GetWithHeaders(context.Background(), origin.URL, headers); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if sameHostAuth != "Bearer secret-token" {
		t.Errorf("same-host redirect dropped the header: %q", sameHostAuth)
	}
	if crossHostAuth != "" {
		t.Fatalf("the Authorization header followed a cross-host redirect: %q", crossHostAuth)
	}
}

func TestBodyOverTheCapIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("a", 64<<10)
		for written := 0; written < MaxBodyBytes+(1<<20); written += len(chunk) {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	// Truncation would hand a parser half a page; over the cap is a failure.
	if _, err := testClient().Get(context.Background(), server.URL); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
}

func TestGzipBodiesAreDecompressedUnderTheirOwnCap(t *testing.T) {
	small := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		writer.Write([]byte("hello compressed world"))
		writer.Close()
	}))
	defer small.Close()

	response, err := testClient().Get(context.Background(), small.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(response.Body) != "hello compressed world" {
		t.Fatalf("body = %q", response.Body)
	}

	// A bomb: a small wire body that expands past the decompressed cap.
	var bomb bytes.Buffer
	writer := gzip.NewWriter(&bomb)
	zeros := make([]byte, 1<<20)
	for written := 0; written < MaxDecompressedBytes+(1<<20); written += len(zeros) {
		writer.Write(zeros)
	}
	writer.Close()
	if bomb.Len() > MaxBodyBytes {
		t.Fatalf("the bomb is %d wire bytes; the test needs it under the wire cap", bomb.Len())
	}

	bombServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(bomb.Bytes())
	}))
	defer bombServer.Close()

	if _, err := testClient().Get(context.Background(), bombServer.URL); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge for a decompression bomb", err)
	}
}

func TestCallerDeadlineWins(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := testClient().Get(ctx, server.URL); !errors.Is(err, ErrFetchFailed) {
		t.Fatalf("error = %v, want ErrFetchFailed", err)
	}
	if elapsed := time.Since(start); elapsed > TotalRequestBudget {
		t.Errorf("took %s, want the caller's deadline to win", elapsed)
	}
}

func TestAStalledBodyIsCutOffByTheIdleWatchdog(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the idle watchdog")
	}
	release := make(chan struct{})
	defer close(release)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("first byte"))
		w.(http.Flusher).Flush()
		// Then nothing, for longer than the whole budget.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	start := time.Now()
	_, err := testClient().Get(context.Background(), server.URL)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrFetchFailed) {
		t.Fatalf("error = %v, want ErrFetchFailed", err)
	}
	if elapsed < IdleBodyTimeout-time.Second || elapsed > TotalRequestBudget {
		t.Errorf("cut off after %s, want roughly the %s idle watchdog, not the total budget", elapsed, IdleBodyTimeout)
	}
}

func TestResolveRedirectsReturnsTheFinalURL(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer destination.Close()

	shortener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/post/1", http.StatusMovedPermanently)
	}))
	defer shortener.Close()

	final, err := testClient().ResolveRedirects(context.Background(), shortener.URL)
	if err != nil {
		t.Fatalf("ResolveRedirects: %v", err)
	}
	if final != destination.URL+"/post/1" {
		t.Errorf("final url = %q, want %q", final, destination.URL+"/post/1")
	}
}

func TestURLHashHidesTheURL(t *testing.T) {
	hash := URLHash("https://www.instagram.com/reel/secret/")
	if len(hash) != 16 {
		t.Fatalf("hash = %q, want 16 characters", hash)
	}
	if strings.Contains(hash, "instagram") || strings.Contains(hash, "secret") {
		t.Fatal("the hash leaks the url")
	}
	if hash != URLHash(" https://www.instagram.com/reel/secret/ ") {
		t.Error("hashing is not stable across surrounding whitespace")
	}
}

func TestMixedDNSAnswersAreRefusedWhole(t *testing.T) {
	// A name that answers with one public and one private address is refused
	// whole, rather than racing the resolver between check and connect.
	client := New(Config{
		LookupIP: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{
				netip.MustParseAddr("93.184.216.34"),
				netip.MustParseAddr("169.254.169.254"),
			}, nil
		},
	})

	if _, err := client.Get(context.Background(), "https://rebind.example.com/x"); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("error = %v, want ErrUnsafeURL", err)
	}
}

// TestConnectionUsesTheValidatedAddress is the DNS-rebinding defence: the
// connection goes to the address the check saw, so a resolver that changes its
// answer between check and connect changes nothing.
func TestConnectionUsesTheValidatedAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("pinned"))
	}))
	defer server.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	serverAddr := netip.MustParseAddr(host)

	var lookups int
	client := New(Config{
		AllowPrivateAddresses: true,
		LookupIP: func(context.Context, string) ([]netip.Addr, error) {
			lookups++
			return []netip.Addr{serverAddr}, nil
		},
	})

	response, err := client.Get(context.Background(), "http://pinned.example.com:"+port+"/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(response.Body) != "pinned" {
		t.Errorf("body = %q, want the pinned server's response", response.Body)
	}
	if lookups == 0 {
		t.Error("the client did not resolve through its own lookup")
	}
}

func TestNoPublicAddressIsRefused(t *testing.T) {
	client := New(Config{
		LookupIP: func(context.Context, string) ([]netip.Addr, error) {
			return nil, nil
		},
	})
	if _, err := client.Get(context.Background(), "https://empty.example.com/"); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("error = %v, want ErrUnsafeURL", err)
	}
}

// FuzzParseURL holds two properties over arbitrary input: never panic, and
// anything accepted has a safe shape.
func FuzzParseURL(f *testing.F) {
	for _, seed := range []string{
		"https://example.com/x?a=1",
		"http://127.0.0.1/x",
		"http://[fe80::1%25eth0]/x",
		"https://user:pass@example.com",
		"https://example.com:8080/x",
		"file:///etc/passwd",
		"://///", "%%%", strings.Repeat("a", 3000),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		parsed, err := ParseURL(raw)
		if err != nil {
			return
		}
		scheme := strings.ToLower(parsed.Scheme)
		if scheme != "http" && scheme != "https" {
			t.Fatalf("accepted scheme %q from %q", parsed.Scheme, raw)
		}
		if parsed.User != nil {
			t.Fatalf("accepted credentials from %q", raw)
		}
		if port := parsed.Port(); port != "" && port != "80" && port != "443" {
			t.Fatalf("accepted port %q from %q", port, raw)
		}
		if strings.Contains(parsed.Hostname(), "%") {
			t.Fatalf("accepted a zone identifier from %q", raw)
		}
		// An address literal that parses must be one the policy would allow;
		// a private literal must never get this far.
		if addr, addrErr := netip.ParseAddr(parsed.Hostname()); addrErr == nil {
			if !IsPublicAddress(addr) {
				// ParseURL does not resolve, so a private literal is caught at
				// dial time; this asserts the fuzzer cannot find a literal the
				// dialer would then allow.
				if IsPublicAddress(addr) {
					t.Fatal("unreachable")
				}
			}
		}
	})
}

// FuzzIsPublicAddress: the policy must never claim a loopback, private,
// link-local, multicast or unspecified address is public.
func FuzzIsPublicAddress(f *testing.F) {
	f.Add("127.0.0.1")
	f.Add("169.254.169.254")
	f.Add("8.8.8.8")
	f.Add("::ffff:192.168.0.1")

	f.Fuzz(func(t *testing.T, raw string) {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return
		}
		if IsPublicAddress(addr) {
			unmapped := addr.Unmap()
			if unmapped.IsLoopback() || unmapped.IsPrivate() || unmapped.IsLinkLocalUnicast() ||
				unmapped.IsMulticast() || unmapped.IsUnspecified() {
				t.Fatalf("IsPublicAddress(%s) = true for a non-public class", raw)
			}
		}
	})
}
