// Package safehttp is the one HTTP client this service points at URLs a user
// supplied. It resolves DNS itself, refuses every non-public address, pins the
// address it validated, and repeats the check on every redirect, so a shared
// link cannot reach the private network behind the service.
package safehttp

import (
	"bytes"
	"compress/gzip"
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

// Every limit is a separate budget: a slow resolver cannot spend the connect
// budget, and a body that trickles one byte per second cannot hold a worker
// for the whole total budget.
const (
	MaxRedirects   = 5
	MaxHeaderBytes = 32 << 10
	// MaxBodyBytes caps what comes off the wire; MaxDecompressedBytes caps what
	// a compressed body may expand to. Both hold at once, so a small gzip bomb
	// cannot buy a large allocation.
	MaxBodyBytes         = 2 << 20
	MaxDecompressedBytes = 8 << 20

	DNSTimeout            = 5 * time.Second
	ConnectTimeout        = 5 * time.Second
	ResponseHeaderTimeout = 10 * time.Second
	// IdleBodyTimeout is the longest the body may go without delivering a byte.
	IdleBodyTimeout    = 5 * time.Second
	TotalRequestBudget = 15 * time.Second
)

// The three failure classes handlers map to stable public codes. The wrapped
// reason is for logs, never for a response body.
var (
	// ErrUnsafeURL is a URL or address this service refuses on policy.
	ErrUnsafeURL = errors.New("unsafe url")
	// ErrTooLarge is a response over its wire or decompressed cap.
	ErrTooLarge = errors.New("response too large")
	// ErrFetchFailed is a network failure or timeout against an allowed URL.
	ErrFetchFailed = errors.New("fetch failed")
)

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
	Status   int
	FinalURL string
	Header   http.Header
	Body     []byte
}

func New(config Config) *Client {
	if config.UserAgent == "" {
		config.UserAgent = "reelpin/1.0 (+https://reelpin.in)"
	}
	client := &Client{config: config, resolver: net.DefaultResolver}

	transport := &http.Transport{
		// Proxy is nil on purpose: an environment proxy would carry a
		// user-supplied URL to a destination this client never validated.
		Proxy:                  nil,
		DialContext:            client.dial,
		MaxResponseHeaderBytes: MaxHeaderBytes,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    ConnectTimeout,
		ResponseHeaderTimeout:  ResponseHeaderTimeout,
		// Compression is handled in readBody with its own cap; transparent
		// decompression would hide the decompressed size.
		DisableCompression: true,
	}

	client.http = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= MaxRedirects {
				return fmt.Errorf("%w: more than %d redirects", ErrUnsafeURL, MaxRedirects)
			}
			// The dialer re-resolves and re-validates the address for the new
			// connection; this re-validates the URL shape.
			if err := client.validateURL(req.URL); err != nil {
				return err
			}
			// Credentials never follow a request to a different origin. Go
			// strips them for cross-domain hops; this strips them on any host
			// change at all.
			if previous := via[len(via)-1]; req.URL.Host != previous.URL.Host {
				req.Header.Del("Authorization")
				req.Header.Del("Cookie")
			}
			return nil
		},
	}
	return client
}

// Get fetches a URL under the whole budget.
func (c *Client) Get(ctx context.Context, rawURL string) (Response, error) {
	return c.GetWithHeaders(ctx, rawURL, nil)
}

// GetWithHeaders is Get with the request headers a platform handler needs.
func (c *Client) GetWithHeaders(ctx context.Context, rawURL string, headers http.Header) (Response, error) {
	parsed, err := c.parseURL(rawURL)
	if err != nil {
		return Response{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, TotalRequestBudget)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrUnsafeURL, err)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("User-Agent", c.config.UserAgent)
	req.Header.Set("Accept-Encoding", "gzip")

	response, err := c.http.Do(req)
	if err != nil {
		return Response{}, wrapRequestError(err)
	}
	defer response.Body.Close()

	body, err := readBody(ctx, cancel, response)
	if err != nil {
		return Response{}, err
	}

	return Response{
		Status:   response.StatusCode,
		FinalURL: response.Request.URL.String(),
		Header:   response.Header,
		Body:     body,
	}, nil
}

// readBody reads under three separate limits: wire bytes, decompressed bytes,
// and an idle watchdog that cancels a body that stops delivering.
func readBody(ctx context.Context, cancel context.CancelFunc, response *http.Response) ([]byte, error) {
	watchdog := time.AfterFunc(IdleBodyTimeout, cancel)
	defer watchdog.Stop()

	wire, err := readCapped(&watchdogReader{reader: response.Body, watchdog: watchdog}, MaxBodyBytes)
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: the body stalled or timed out", ErrFetchFailed)
		}
		return nil, err
	}

	if !strings.EqualFold(response.Header.Get("Content-Encoding"), "gzip") {
		return wire, nil
	}

	decompressor, err := gzip.NewReader(bytes.NewReader(wire))
	if err != nil {
		return nil, fmt.Errorf("%w: the body claims gzip but is not", ErrFetchFailed)
	}
	defer decompressor.Close()

	return readCapped(decompressor, MaxDecompressedBytes)
}

// readCapped reads at most limit bytes and fails on more, rather than silently
// truncating: a parser fed half a page produces confident nonsense.
func readCapped(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading the body", ErrFetchFailed)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: over %d bytes", ErrTooLarge, limit)
	}
	return body, nil
}

// watchdogReader pushes the idle deadline forward on every delivered chunk.
type watchdogReader struct {
	reader   io.Reader
	watchdog *time.Timer
}

func (w *watchdogReader) Read(p []byte) (int, error) {
	n, err := w.reader.Read(p)
	if n > 0 {
		w.watchdog.Reset(IdleBodyTimeout)
	}
	return n, err
}

// ResolveRedirects follows a shortener to the URL it finally points at, without
// downloading a page body.
func (c *Client) ResolveRedirects(ctx context.Context, rawURL string) (string, error) {
	parsed, err := c.parseURL(rawURL)
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
// slip through between check and connect. TLS still verifies the original
// hostname, because the URL is untouched; only the dialed address is pinned.
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

	ctx, cancel := context.WithTimeout(ctx, DNSTimeout)
	defer cancel()

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

// IsPublicAddress is the address policy: loopback, link-local (which covers
// cloud metadata services), private, multicast, unspecified and the reserved
// ranges below are all refused.
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

// MaxURLLength bounds every URL this service accepts, before parsing.
const MaxURLLength = 2048

// parseURL is ParseURL under this client's policy: the test seam that allows
// private addresses also allows the random ports local test servers listen on.
func (c *Client) parseURL(rawURL string) (*url.URL, error) {
	trimmed := strings.TrimSpace(rawURL)
	if len(trimmed) > MaxURLLength {
		return nil, fmt.Errorf("%w: over %d characters", ErrUnsafeURL, MaxURLLength)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsafeURL, err)
	}
	if err := c.validateURL(parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

func (c *Client) validateURL(parsed *url.URL) error {
	if c.config.AllowPrivateAddresses {
		return validateURLShape(parsed)
	}
	return ValidateURL(parsed)
}

// ParseURL accepts only the shapes this service is willing to fetch.
func ParseURL(rawURL string) (*url.URL, error) {
	trimmed := strings.TrimSpace(rawURL)
	if len(trimmed) > MaxURLLength {
		return nil, fmt.Errorf("%w: over %d characters", ErrUnsafeURL, MaxURLLength)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsafeURL, err)
	}
	if err := ValidateURL(parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

func ValidateURL(parsed *url.URL) error {
	if err := validateURLShape(parsed); err != nil {
		return err
	}
	// ponytail: hard 80/443 rule; a per-provider port allowlist joins Config
	// when a reviewed provider actually needs one.
	if port := parsed.Port(); port != "" && port != "80" && port != "443" {
		return fmt.Errorf("%w: only ports 80 and 443 are allowed", ErrUnsafeURL)
	}
	return nil
}

func validateURLShape(parsed *url.URL) error {
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("%w: only http and https are allowed", ErrUnsafeURL)
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: credentials in a url are not allowed", ErrUnsafeURL)
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("%w: the url has no host", ErrUnsafeURL)
	}
	// An IPv6 zone names one machine's interface; a URL carrying one is aimed
	// at something no public service reaches.
	if strings.Contains(host, "%") {
		return fmt.Errorf("%w: an ipv6 zone identifier is not allowed", ErrUnsafeURL)
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
	sentinels := []error{ErrUnsafeURL, ErrTooLarge, ErrFetchFailed}
	for _, sentinel := range sentinels {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		for _, sentinel := range sentinels {
			if errors.Is(urlErr.Err, sentinel) {
				return urlErr.Err
			}
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: the request timed out", ErrFetchFailed)
	}
	return fmt.Errorf("%w: the request failed", ErrFetchFailed)
}
