package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"time"
)

// call is one request the driver knows how to send.
type call struct {
	Name   string
	Method string
	Path   string
	Body   string
	// Weight is how often this call is picked out of the plan.
	Weight int
}

type plan struct {
	Name  string
	Calls []call
}

func planFor(scenario, token, shareURL string) (plan, error) {
	needsToken := func() error {
		if token == "" {
			return fmt.Errorf("scenario %q needs --token", scenario)
		}
		return nil
	}

	switch scenario {
	case "health":
		return plan{Name: scenario, Calls: []call{
			{Name: "live", Method: "GET", Path: "/api/v1/health/live", Weight: 1},
			{Name: "ready", Method: "GET", Path: "/api/v1/health/ready", Weight: 1},
		}}, nil

	case "reads":
		if err := needsToken(); err != nil {
			return plan{}, err
		}
		return plan{Name: scenario, Calls: []call{
			{Name: "list", Method: "GET", Path: "/api/v1/reels?limit=20", Weight: 5},
			{Name: "filters", Method: "GET", Path: "/api/v1/reels/filters", Weight: 2},
			{Name: "category-filters", Method: "GET", Path: "/api/v1/reels/category-filters", Weight: 1},
			{Name: "stats", Method: "GET", Path: "/api/v1/account/library-stats", Weight: 1},
		}}, nil

	case "enqueue":
		if err := needsToken(); err != nil {
			return plan{}, err
		}
		if shareURL == "" {
			return plan{}, fmt.Errorf("scenario enqueue needs --share-url")
		}
		return plan{Name: scenario, Calls: []call{{
			Name: "enqueue", Method: "POST", Path: "/api/v1/processing-jobs/reels",
			Body: fmt.Sprintf(`{"url":%q}`, shareURL), Weight: 1,
		}}}, nil

	case "poll":
		if err := needsToken(); err != nil {
			return plan{}, err
		}
		return plan{Name: scenario, Calls: []call{
			{Name: "jobs", Method: "GET", Path: "/api/v1/processing-jobs", Weight: 1},
		}}, nil

	case "search":
		if err := needsToken(); err != nil {
			return plan{}, err
		}
		return plan{Name: scenario, Calls: []call{
			{Name: "search", Method: "POST", Path: "/api/v1/search",
				Body: `{"query":"quiet cafe with good coffee","limit":5}`, Weight: 1},
		}}, nil

	case "mixed":
		if err := needsToken(); err != nil {
			return plan{}, err
		}
		// Roughly what the app does: mostly reads, some polling, a little search.
		return plan{Name: scenario, Calls: []call{
			{Name: "list", Method: "GET", Path: "/api/v1/reels?limit=20", Weight: 10},
			{Name: "filters", Method: "GET", Path: "/api/v1/reels/filters", Weight: 3},
			{Name: "jobs", Method: "GET", Path: "/api/v1/processing-jobs", Weight: 4},
			{Name: "map", Method: "GET", Path: "/api/v1/map", Weight: 2},
			{Name: "search", Method: "POST", Path: "/api/v1/search",
				Body: `{"query":"quiet cafe with good coffee","limit":5}`, Weight: 1},
			{Name: "live", Method: "GET", Path: "/api/v1/health/live", Weight: 1},
		}}, nil

	default:
		return plan{}, fmt.Errorf("unknown scenario %q", scenario)
	}
}

// pick chooses a call by weight.
func (p plan) pick() call {
	total := 0
	for _, c := range p.Calls {
		total += c.Weight
	}
	if total <= 0 {
		return p.Calls[0]
	}
	n := rand.IntN(total)
	for _, c := range p.Calls {
		n -= c.Weight
		if n < 0 {
			return c
		}
	}
	return p.Calls[len(p.Calls)-1]
}

type Driver struct {
	BaseURL string
	Token   string
	Timeout time.Duration
	Workers int
	Rate    float64
}

type sample struct {
	name    string
	status  int
	elapsed time.Duration
	err     bool
}

func (d *Driver) Run(ctx context.Context, p plan) *Report {
	client := &http.Client{Timeout: d.Timeout}
	samples := make(chan sample, 1024)

	var wait sync.WaitGroup
	for i := 0; i < d.Workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			d.drive(ctx, client, p, samples)
		}()
	}

	go func() {
		wait.Wait()
		close(samples)
	}()

	report := newReport(p.Name)
	started := time.Now()
	for s := range samples {
		report.add(s)
	}
	report.Elapsed = time.Since(started)
	return report
}

func (d *Driver) drive(ctx context.Context, client *http.Client, p plan, out chan<- sample) {
	var wait time.Duration
	if d.Rate > 0 {
		wait = time.Duration(float64(time.Second) / d.Rate)
	}

	for ctx.Err() == nil {
		c := p.pick()
		out <- d.send(ctx, client, c)

		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
}

func (d *Driver) send(ctx context.Context, client *http.Client, c call) sample {
	var body io.Reader
	if c.Body != "" {
		body = bytes.NewReader([]byte(c.Body))
	}

	req, err := http.NewRequestWithContext(ctx, c.Method, d.BaseURL+c.Path, body)
	if err != nil {
		return sample{name: c.Name, err: true}
	}
	if c.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if d.Token != "" {
		req.Header.Set("Authorization", "Bearer "+d.Token)
	}

	started := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(started)
	if err != nil {
		return sample{name: c.Name, elapsed: elapsed, err: true}
	}
	// The body must be drained for the connection to be reused, or every
	// request opens a new one and the numbers measure the dial, not the API.
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	return sample{name: c.Name, status: resp.StatusCode, elapsed: elapsed}
}

func percentile(sorted []time.Duration, percent int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := (len(sorted)*percent + 99) / 100
	if index < 1 {
		index = 1
	}
	if index > len(sorted) {
		index = len(sorted)
	}
	return sorted[index-1]
}

func sortDurations(values []time.Duration) []time.Duration {
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted
}
