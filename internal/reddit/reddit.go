// Package reddit mints the application-only token the Reddit API wants.
//
// It is its own package rather than part of internal/platform/social because
// minting is a form POST: internal/safehttp guards URLs a user supplied and
// speaks only GET, so this endpoint, which is ours and constant, is the
// standard library client with a timeout instead.
package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// tokenURL is a variable so a test can serve it locally. Minting is on
// www.reddit.com even though every read that uses the token is on
// oauth.reddit.com.
var tokenURL = "https://www.reddit.com/api/v1/access_token"

const (
	requestTimeout = 10 * time.Second

	// maxResponseBytes bounds the token response. It is a small JSON object,
	// and an error page from a proxy in front of Reddit is not.
	maxResponseBytes = 64 << 10

	// refreshMargin is how early a token is replaced. Reddit issues these for
	// a day, so spending a few minutes of that is free, and a token that
	// lapses between the check and the read Reddit answers costs a whole job.
	refreshMargin = 5 * time.Minute

	// userAgent is required: Reddit refuses requests without one. It matches
	// what the handler sends on the read that follows.
	userAgent = "reelpin/1.0"
)

// TokenSource is the same single method internal/platform/social consumes. It
// is declared here as well so that "nothing is configured" can be a nil
// interface rather than a nil *Client inside a non-nil one, which the handler
// cannot tell apart from a working minter and would only discover by panicking
// mid-run.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
}

// Client holds the application credentials and the token they last bought.
type Client struct {
	id     string
	secret string
	client *http.Client

	// mu is held across the mint, not just around the fields, so several
	// worker goroutines arriving at once buy one token between them: the
	// second one in blocks, then finds the first one's token already cached.
	mu      sync.Mutex
	token   string
	expires time.Time
}

var _ TokenSource = (*Client)(nil)

// New returns nil when either credential is missing, which is the state the
// Reddit handler already knows how to report: it reads the public JSON view
// instead, and logs that it did.
func New(clientID, clientSecret string) TokenSource {
	id, secret := strings.TrimSpace(clientID), strings.TrimSpace(clientSecret)
	if id == "" || secret == "" {
		return nil
	}
	return &Client{id: id, secret: secret, client: &http.Client{Timeout: requestTimeout}}
}

// AccessToken returns the cached token, minting a new one when there is none
// or the one there is has come within refreshMargin of lapsing.
func (c *Client) AccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}

	token, lifetime, err := c.mint(ctx)
	if err != nil {
		// The caller falls back to the public endpoint on any error, so the
		// stale token is dropped rather than handed out past its life.
		c.token, c.expires = "", time.Time{}
		return "", err
	}

	c.token, c.expires = token, time.Now().Add(lifetime-refreshMargin)
	return token, nil
}

func (c *Client) mint(ctx context.Context) (string, time.Duration, error) {
	form := url.Values{"grant_type": {"client_credentials"}}.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form))
	if err != nil {
		return "", 0, fmt.Errorf("reddit token: building the request: %w", err)
	}
	request.SetBasicAuth(c.id, c.secret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", userAgent)

	response, err := c.client.Do(request)
	if err != nil {
		return "", 0, fmt.Errorf("reddit token: minting: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		response.Body.Close()
	}()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// The body is not quoted: a provider error can echo the request back,
		// and this request carries the application credentials.
		return "", 0, fmt.Errorf("reddit token: minting answered %d", response.StatusCode)
	}

	var minted struct {
		AccessToken string  `json:"access_token"`
		ExpiresIn   float64 `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&minted); err != nil {
		return "", 0, errors.New("reddit token: the response was not a token")
	}
	if minted.AccessToken == "" {
		// A 200 carrying no token is Reddit refusing in its own way. Returning
		// it as an empty string would send the handler at the API with no
		// credential, which answers 401.
		return "", 0, errors.New("reddit token: the response carried no token")
	}
	return minted.AccessToken, time.Duration(minted.ExpiresIn * float64(time.Second)), nil
}
