// Package apify runs the configured actors that reach platforms a plain fetch
// cannot. Every call is bounded and every actor is configured, never derived
// from user input: an actor id is a deployment decision.
package apify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/spend"
)

// ErrNotConfigured means no token or no actor for this platform, so the caller
// should fall back rather than fail.
var ErrNotConfigured = errors.New("apify is not configured for this platform")

// ErrRateLimited is the provider refusing for now.
var ErrRateLimited = errors.New("apify is rate limiting")

const (
	// MaxResponseBytes bounds an actor's dataset.
	MaxResponseBytes = 8 << 20
	DefaultTimeout   = 120 * time.Second
)

type Config struct {
	Token   string
	Actors  map[string]string
	Timeout time.Duration
	// Usage receives one record per completed run. Nil means nothing is
	// recorded.
	Usage spend.Recorder
}

type Client struct {
	config Config
	client *http.Client
}

// baseURL is a variable so tests can point it at a local server.
var baseURL = "https://api.apify.com/v2"

func New(config Config) *Client {
	if config.Timeout <= 0 {
		config.Timeout = DefaultTimeout
	}
	return &Client{config: config, client: &http.Client{Timeout: config.Timeout}}
}

// Configured reports whether a platform has an actor, so a handler can choose
// its fallback before spending a request.
func (c *Client) Configured(platform string) bool {
	if strings.TrimSpace(c.config.Token) == "" {
		return false
	}
	return strings.TrimSpace(c.config.Actors[platform]) != ""
}

// Run executes an actor synchronously and returns its dataset items. Apify's
// run-sync endpoint is what keeps this one request rather than a poll loop.
func (c *Client) Run(ctx context.Context, platform string, input any) ([]json.RawMessage, error) {
	if !c.Configured(platform) {
		return nil, ErrNotConfigured
	}
	actor := strings.ReplaceAll(strings.TrimSpace(c.config.Actors[platform]), "/", "~")

	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encoding the actor input: %w", err)
	}

	endpoint := fmt.Sprintf("%s/acts/%s/run-sync-get-dataset-items?%s",
		baseURL, actor, url.Values{"timeout": {fmt.Sprintf("%d", int(c.config.Timeout.Seconds()))}}.Encode())

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("building the actor request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.config.Token)

	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("calling the actor: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("reading the actor response: %w", err)
	}

	switch {
	case response.StatusCode == http.StatusTooManyRequests:
		return nil, ErrRateLimited
	case response.StatusCode == http.StatusUnauthorized, response.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("the actor rejected our credentials")
	case response.StatusCode < 200 || response.StatusCode >= 300:
		// The actor's body can echo the input, so only the status is reported.
		return nil, fmt.Errorf("the actor returned HTTP %d", response.StatusCode)
	}

	// Apify reports nothing about what the run cost on this endpoint, so this
	// is a call count and not a measured cost: the price of one run is
	// configuration. A run that failed before returning is not counted, which
	// undercounts a run that was billed and then errored.
	if c.config.Usage != nil {
		c.config.Usage.Record(ctx, spend.Usage{
			Provider: "apify", Model: platform, Operation: "actor_run", Calls: 1,
		})
	}

	var items []json.RawMessage
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("decoding the actor dataset: %w", err)
	}
	return items, nil
}
