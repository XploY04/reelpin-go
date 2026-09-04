package safehttp

import (
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
		{"169.254.169.254", false}, // cloud metadata
		{"100.64.0.1", false},      // carrier-grade NAT
		{"198.18.0.1", false},
		{"224.0.0.1", false},
		{"255.255.255.255", false},
		{"::1", false},
		{"::", false},
		{"fe80::1", false},
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
		"http://127.0.0.1:8080/x",
		"http://localhost:8080/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/x",
		"http://10.1.2.3/x",
		"http://192.168.0.1/x",
	} {
		t.Run(target, func(t *testing.T) {
			if _, err := client.Get(context.Background(), target); !errors.Is(err, ErrUnsafeURL) {
				t.Fatalf("Get(%q) error = %v, want ErrUnsafeURL", target, err)
			}
		})
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

func TestBodyIsCapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("a", 64<<10)
		for written := 0; written < MaxBodyBytes+(1<<20); written += len(chunk) {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	response, err := testClient().Get(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(response.Body) != MaxBodyBytes {
		t.Errorf("body = %d bytes, want the %d byte cap", len(response.Body), MaxBodyBytes)
	}
	if !response.BodyTrimmed {
		t.Error("BodyTrimmed = false on a body over the cap")
	}
}

func TestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := testClient().Get(ctx, server.URL); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("error = %v, want ErrUnsafeURL", err)
	}
	if elapsed := time.Since(start); elapsed > TotalRequestBudget {
		t.Errorf("took %s, want the caller's deadline to win", elapsed)
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

func TestDNSRebindingIsRefused(t *testing.T) {
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
