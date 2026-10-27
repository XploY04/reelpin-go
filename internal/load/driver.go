// Package load drives a running API and reports what it did. It is a
// measurement tool, not a test: it needs a real base URL and a real token, and
// it never runs in CI.
package load

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

	"github.com/google/uuid"
)

// Call is one request the driver knows how to send.
type Call struct {
	Name   string
	Method string
	Path   string
	Body   string
	// Weight is how often this call is picked out of the plan.
	Weight int
	// Idempotent marks a submission, which the contract requires to carry a
	// fresh Idempotency-Key. Reusing one replays the first stored answer and
	// measures nothing.
	Idempotent bool
}

type Plan struct {
	Name  string
	Calls []Call
}

// PlanFor builds the plan for a named scenario, refusing before any traffic if
// it is missing what it needs.
func PlanFor(scenario, token, shareURL string) (Plan, error) {
	needsToken := func() error {
		if token == "" {
			return fmt.Errorf("scenario %q needs a token", scenario)
		}
		return nil
	}

	switch scenario {
	case "health":
		return Plan{Name: scenario, Calls: []Call{
			{Name: "live", Method: "GET", Path: "/api/v2/health/live", Weight: 1},
			{Name: "ready", Method: "GET", Path: "/api/v2/health/ready", Weight: 1},
		}}, nil

	case "reads":
		if err := needsToken(); err != nil {
			return Plan{}, err
		}
		return Plan{Name: scenario, Calls: []Call{
			{Name: "list", Method: "GET", Path: "/api/v2/reels?limit=20", Weight: 5},
			{Name: "filters", Method: "GET", Path: "/api/v2/reels/filters", Weight: 2},
			{Name: "category-filters", Method: "GET", Path: "/api/v2/reels/category-filters", Weight: 1},
			{Name: "stats", Method: "GET", Path: "/api/v2/account/library-stats", Weight: 1},
		}}, nil

	case "enqueue":
		if err := needsToken(); err != nil {
			return Plan{}, err
		}
		if shareURL == "" {
			return Plan{}, fmt.Errorf("scenario enqueue needs a URL to share")
		}
		return Plan{Name: scenario, Calls: []Call{{
			Name: "enqueue", Method: "POST", Path: "/api/v2/processing-jobs/reels",
			Body: fmt.Sprintf(`{"url":%q}`, shareURL), Weight: 1, Idempotent: true,
		}}}, nil

	case "poll":
		if err := needsToken(); err != nil {
			return Plan{}, err
		}
		return Plan{Name: scenario, Calls: []Call{
			{Name: "jobs", Method: "GET", Path: "/api/v2/processing-jobs", Weight: 1},
		}}, nil

	case "mixed":
		if err := needsToken(); err != nil {
			return Plan{}, err
		}
		// Roughly what the app does: mostly reads, some polling.
		return Plan{Name: scenario, Calls: []Call{
			{Name: "list", Method: "GET", Path: "/api/v2/reels?limit=20", Weight: 10},
			{Name: "filters", Method: "GET", Path: "/api/v2/reels/filters", Weight: 3},
			{Name: "jobs", Method: "GET", Path: "/api/v2/processing-jobs", Weight: 4},
			{Name: "collections", Method: "GET", Path: "/api/v2/collections", Weight: 2},
			{Name: "live", Method: "GET", Path: "/api/v2/health/live", Weight: 1},
		}}, nil

	default:
		return Plan{}, fmt.Errorf("unknown scenario %q", scenario)
	}
}

// pick chooses a call by weight.
func (p Plan) pick() Call {
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
	// Rate is requests per second per worker. Zero means as fast as the API
	// answers.
	Rate float64
}

type sample struct {
	name    string
	status  int
	elapsed time.Duration
	err     bool
}

func (d *Driver) Run(ctx context.Context, p Plan) *Report {
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

	report := NewReport(p.Name)
	started := time.Now()
	for s := range samples {
		report.add(s)
	}
	report.Elapsed = time.Since(started)
	return report
}

func (d *Driver) drive(ctx context.Context, client *http.Client, p Plan, out chan<- sample) {
	var wait time.Duration
	if d.Rate > 0 {
		wait = time.Duration(float64(time.Second) / d.Rate)
	}

	for ctx.Err() == nil {
		out <- d.send(ctx, client, p.pick())

		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
}

func (d *Driver) send(ctx context.Context, client *http.Client, c Call) sample {
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
	if c.Idempotent {
		req.Header.Set("Idempotency-Key", uuid.NewString())
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
