package load

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAScenarioWithoutATokenIsRefusedBeforeAnyTraffic(t *testing.T) {
	for _, scenario := range []string{"reads", "enqueue", "poll", "mixed"} {
		if _, err := PlanFor(scenario, "", "https://example.com/x"); err == nil {
			t.Errorf("scenario %q ran without a token", scenario)
		}
	}
	if _, err := PlanFor("health", "", ""); err != nil {
		t.Errorf("health needs no token: %v", err)
	}
	if _, err := PlanFor("enqueue", "token", ""); err == nil {
		t.Error("the enqueue scenario ran without a URL to share")
	}
	if _, err := PlanFor("nonsense", "token", ""); err == nil {
		t.Error("an unknown scenario was accepted")
	}
}

func TestEveryPlannedPathIsOnTheV2Surface(t *testing.T) {
	for _, scenario := range []string{"health", "reads", "enqueue", "poll", "mixed"} {
		plan, err := PlanFor(scenario, "token", "https://example.com/x")
		if err != nil {
			t.Fatalf("scenario %q: %v", scenario, err)
		}
		for _, call := range plan.Calls {
			if !strings.HasPrefix(call.Path, "/api/v2/") {
				t.Errorf("%s drives %s, which this service does not serve", scenario, call.Path)
			}
		}
	}
}

func TestPickHonoursWeights(t *testing.T) {
	p := Plan{Calls: []Call{{Name: "common", Weight: 99}, {Name: "rare", Weight: 1}}}

	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		counts[p.pick().Name]++
	}
	if counts["common"] <= counts["rare"] {
		t.Fatalf("weights were ignored: %v", counts)
	}
}

func TestTheDriverReportsWhatTheAPIReturned(t *testing.T) {
	var requests atomic.Int64
	var mu sync.Mutex
	keys := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("request arrived without the token")
		}
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		// Every third request is rate limited, every tenth fails.
		switch n := requests.Add(1); {
		case n%10 == 0:
			w.WriteHeader(http.StatusInternalServerError)
		case n%3 == 0:
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusOK)
		}
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	driver := &Driver{BaseURL: server.URL, Token: "test-token", Timeout: 5 * time.Second, Workers: 4}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	report := driver.Run(ctx, Plan{Name: "test", Calls: []Call{
		{Name: "enqueue", Method: "POST", Path: "/api/v2/processing-jobs/reels",
			Body: `{"url":"https://example.com/x"}`, Weight: 1, Idempotent: true},
	}})

	if report.Requests == 0 {
		t.Fatal("no request was made")
	}
	if report.Rejected == 0 {
		t.Error("rate-limited responses were not counted")
	}
	if report.Errors == 0 {
		t.Error("server errors were not counted")
	}
	if report.Statuses[http.StatusOK] == 0 {
		t.Error("successful responses were not counted")
	}

	// A reused key replays the first stored answer, so the run would measure
	// nothing after its first request.
	server.Close()
	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, key := range keys {
		if key == "" {
			t.Fatal("a submission was sent without an Idempotency-Key")
		}
		if seen[key] {
			t.Fatalf("Idempotency-Key %s was reused", key)
		}
		seen[key] = true
	}

	var out bytes.Buffer
	report.Elapsed = time.Second
	report.Print(&out)
	for _, want := range []string{"scenario test", "latency", "by status:", "by call:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("%q missing from the report:\n%s", want, out.String())
		}
	}
}

func TestAnUnreachableAPIIsAnErrorNotACrash(t *testing.T) {
	driver := &Driver{BaseURL: "http://127.0.0.1:1", Timeout: 100 * time.Millisecond, Workers: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	report := driver.Run(ctx, Plan{Name: "test", Calls: []Call{
		{Name: "live", Method: "GET", Path: "/api/v2/health/live", Weight: 1},
	}})
	if report.Errors == 0 {
		t.Fatal("a refused connection was not counted as an error")
	}
}

func TestAnEmptyReportSaysSoInsteadOfDividingByZero(t *testing.T) {
	var out bytes.Buffer
	NewReport("test").Print(&out)
	if !strings.Contains(out.String(), "no requests") {
		t.Fatalf("report = %q", out.String())
	}
}

func TestPercentilesAreOrdered(t *testing.T) {
	values := sortDurations([]time.Duration{50, 10, 30, 20, 40})
	if percentile(values, 50) > percentile(values, 95) {
		t.Fatal("p50 came out above p95")
	}
	if percentile(nil, 50) != 0 {
		t.Error("an empty set should be zero, not a panic")
	}
}
