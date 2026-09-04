// Package safehttp is the one HTTP client this service points at URLs a user
// supplied. It resolves DNS itself, refuses every non-public address, pins the
// address it validated, and repeats the check on every redirect, so a shared
// link cannot reach the private network behind the service.
package safehttp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const (
	MaxRedirects       = 5
	MaxHeaderBytes     = 32 << 10
	MaxBodyBytes       = 2 << 20
	ConnectTimeout     = 5 * time.Second
	TotalRequestBudget = 10 * time.Second
)

// ErrUnsafeURL covers every rejected URL and address. The reason is for logs,
// never for a response body.
var ErrUnsafeURL = errors.New("unsafe url")

type Config struct {
	// AllowPrivateAddresses is for tests only. A local test server listens on
	// loopback, which production must always refuse.
	AllowPrivateAddresses bool
	// UserAgent is sent on every request.
	UserAgent string
	// LookupIP replaces DNS resolution. Tests use it to prove rebinding is
	// refused; production leaves it nil and uses the system resolver.
	LookupIP func(ctx context.Context, host string) ([]netip.Addr, error)
}

type Client struct {
	http     *http.Client
	config   Config
	resolver *net.Resolver
}

type Response struct {
	Status      int
	FinalURL    string
	Header      http.Header
	Body        []byte
	BodyTrimmed bool
}

func New(config Config) *Client {
	if config.UserAgent == "" {
		config.UserAgent = "reelpin/1.0 (+https://reelpin.in)"
	}
	client := &Client{config: config, resolver: net.DefaultResolver}

	transport := &http.Transport{
		DialContext:            client.dial,
		MaxResponseHeaderBytes: MaxHeaderBytes,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    ConnectTimeout,
		ResponseHeaderTimeout:  TotalRequestBudget,
		DisableCompression:     false,
	}

	client.http = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= MaxRedirects {
				return fmt.Errorf("%w: more than %d redirects", ErrUnsafeURL, MaxRedirects)
			}
			// The dialer re-validates the address; this re-validates the URL.
			if err := ValidateURL(req.URL); err != nil {
				return err
			}
			return nil
		},
	}
	return client
}

// Get fetches a URL under the whole budget: 10 seconds, 5 redirects, 2 MiB.
func (c *Client) Get(ctx context.Context, rawURL string) (Response, error) {
	parsed, err := ParseURL(rawURL)
	if err != nil {
		return Response{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, TotalRequestBudget)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrUnsafeURL, err)
	}
	req.Header.Set("User-Agent", c.config.UserAgent)

	response, err := c.http.Do(req)
	if err != nil {
		return Response{}, wrapRequestError(err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, MaxBodyBytes+1))
	if err != nil {
		return Response{}, fmt.Errorf("%w: reading body", ErrUnsafeURL)
	}
	trimmed := len(body) > MaxBodyBytes
	if trimmed {
		body = body[:MaxBodyBytes]
	}

	return Response{
		Status:      response.StatusCode,
		FinalURL:    response.Request.URL.String(),
		Header:      response.Header,
		Body:        body,
		BodyTrimmed: trimmed,
	}, nil
}

// ResolveRedirects follows a shortener to the URL it finally points at, without
// downloading a page body.
func (c *Client) ResolveRedirects(ctx context.Context, rawURL string) (string, error) {
	parsed, err := ParseURL(rawURL)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, TotalRequestBudget)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnsafeURL, err)
	}
	req.Header.Set("User-Agent", c.config.UserAgent)

	response, err := c.http.Do(req)
	if err != nil {
		return "", wrapRequestError(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		response.Body.Close()
	}()

	return response.Request.URL.String(), nil
}

// dial resolves the host itself and connects only to an address it validated,
// so a name that answers with a public address and then a private one cannot
// slip through between check and connect.
func (c *Client) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsafeURL, err)
	}

	addresses, err := c.publicAddresses(ctx, host)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: ConnectTimeout}
	var lastErr error
	for _, addr := range addresses {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("%w: no usable address for the host", ErrUnsafeURL)
}

func (c *Client) publicAddresses(ctx context.Context, host string) ([]netip.Addr, error) {
	if literal, err := netip.ParseAddr(host); err == nil {
		if !c.allowed(literal) {
			return nil, fmt.Errorf("%w: %s is not a public address", ErrUnsafeURL, literal)
		}
		return []netip.Addr{literal}, nil
	}

	lookup := c.config.LookupIP
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return c.resolver.LookupNetIP(ctx, "ip", host)
		}
	}
	resolved, err := lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%w: could not resolve the host", ErrUnsafeURL)
	}

	addresses := make([]netip.Addr, 0, len(resolved))
	for _, addr := range resolved {
		if !c.allowed(addr.Unmap()) {
			// One private answer poisons the name: refuse the whole request
			// rather than racing the resolver.
			return nil, fmt.Errorf("%w: the host resolves to a non-public address", ErrUnsafeURL)
		}
		addresses = append(addresses, addr.Unmap())
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("%w: the host resolves to no addresses", ErrUnsafeURL)
	}
	return addresses, nil
}

func (c *Client) allowed(addr netip.Addr) bool {
	if c.config.AllowPrivateAddresses {
		return addr.IsValid()
	}
	return IsPublicAddress(addr)
}

// IsPublicAddress mirrors Python's ipaddress.is_global.
func IsPublicAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() ||
		addr.IsPrivate() || addr.IsMulticast() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() {
		return false
	}

	for _, reserved := range reservedPrefixes {
		if reserved.Contains(addr) {
			return false
		}
	}
	return true
}

// reservedPrefixes are the ranges net/netip does not already classify.
var reservedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),   // documentation
	netip.MustParsePrefix("192.88.99.0/24"), // 6to4 relay anycast
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), // reserved, includes broadcast
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use translation
	netip.MustParsePrefix("100::/64"),       // discard-only
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("fc00::/7"),       // unique local
}

// ParseURL accepts only the shapes this service is willing to fetch.
func ParseURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsafeURL, err)
	}
	if err := ValidateURL(parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

func ValidateURL(parsed *url.URL) error {
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("%w: only http and https are allowed", ErrUnsafeURL)
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: credentials in a url are not allowed", ErrUnsafeURL)
	}
	if parsed.Hostname() == "" {
		return fmt.Errorf("%w: the url has no host", ErrUnsafeURL)
	}
	if port := parsed.Port(); port != "" {
		if _, err := net.LookupPort("tcp", port); err != nil {
			return fmt.Errorf("%w: the url port is invalid", ErrUnsafeURL)
		}
	}
	return nil
}

// URLHash is what goes in a log line instead of the URL itself.
func URLHash(rawURL string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(rawURL)))
	return hex.EncodeToString(sum[:])[:16]
}

func wrapRequestError(err error) error {
	if errors.Is(err, ErrUnsafeURL) {
		return err
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && errors.Is(urlErr.Err, ErrUnsafeURL) {
		return urlErr.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: the request timed out", ErrUnsafeURL)
	}
	return fmt.Errorf("%w: the request failed", ErrUnsafeURL)
}
