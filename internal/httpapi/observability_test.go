package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/metrics"
)

const testAdminKey = "test-admin-key"

func observedDeps() Deps {
	deps := testDeps(&fakePinger{})
	deps.Metrics = metrics.New()
	deps.AdminKey = testAdminKey
	return deps
}

// scrape reads the exposition format back out of the registry.
func scrape(t *testing.T, deps Deps) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	routes(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d (%s)", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestRequestsAreCountedByRouteNotByPath(t *testing.T) {
	deps := observedDeps()
	server := routes(deps)

	// Two different reels must not become two time series.
	for _, id := range []string{testReelID, "55555555-5555-4555-8555-555555555555"} {
		req := httptest.NewRequest("GET", "/api/v2/reels/"+id, nil)
		req.Header.Set("Authorization", "Bearer good.token")
		server.ServeHTTP(httptest.NewRecorder(), req)
	}

	body := scrape(t, deps)
	if !strings.Contains(body, `route="/api/v2/reels/{reel_id}"`) {
		t.Fatalf("no route-shaped series in:\n%s", body)
	}
	if strings.Contains(body, testReelID) {
		t.Error("a reel id reached a metric label")
	}
}

func TestAnUnmatchedPathDoesNotGrowASeriesPerPath(t *testing.T) {
	deps := observedDeps()
	server := routes(deps)

	for _, path := range []string{"/nope", "/also-nope", "/definitely/not/here"} {
		server.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}

	body := scrape(t, deps)
	if strings.Contains(body, "definitely") {
		t.Error("an unmatched path became a label")
	}
	if !strings.Contains(body, `route="/"`) && !strings.Contains(body, `route="unmatched"`) {
		t.Errorf("unmatched requests were not counted:\n%s", body)
	}
}

func TestTheScrapeNeedsTheAdminKey(t *testing.T) {
	rec := request(observedDeps(), "GET", "/metrics", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: metrics are operational detail", rec.Code)
	}
}

func TestWithoutAnAdminKeyTheEndpointIsNotThere(t *testing.T) {
	deps := observedDeps()
	deps.AdminKey = ""

	rec := request(deps, "GET", "/metrics", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: an unconfigured scrape target should not exist", rec.Code)
	}
}

func TestALoggedRequestNeverCarriesTheUserID(t *testing.T) {
	deps := observedDeps()
	var logged bytes.Buffer
	deps.Logger = slog.New(slog.NewJSONHandler(&logged, nil))

	request(deps, "GET", "/api/v2/reels", "", "Bearer good.token")

	line := logged.String()
	if strings.Contains(line, testUserID) {
		t.Fatalf("the user id reached the log: %s", line)
	}
	if !strings.Contains(line, metrics.Hash(testUserID)) {
		t.Errorf("the hashed user is missing from: %s", line)
	}
}

func TestReadinessReportsEveryConfiguredDependency(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Redis = pingerFunc(func(context.Context) error { return errors.New("dial tcp: connection refused") })
	deps.Workers = workerCountFunc(func(context.Context) (int, error) { return 2, nil })

	rec := request(deps, "GET", "/api/v2/health/ready", "", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 with Redis down", rec.Code)
	}

	body := rec.Body.String()
	for _, want := range []string{`"redis"`, `"workers"`, `"live_workers":2`} {
		if !strings.Contains(body, want) {
			t.Errorf("%s missing from %s", want, body)
		}
	}
	if strings.Contains(body, "connection refused") {
		t.Error("the driver error reached the response")
	}
}

func TestReadinessIsFineWithoutOptionalDependencies(t *testing.T) {
	rec := request(testDeps(&fakePinger{}), "GET", "/api/v2/health/ready", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a process without Redis is still ready", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"redis"`) {
		t.Error("an unconfigured dependency was reported")
	}
}

func TestNoWorkersIsReportedButDoesNotFailTheAPI(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Workers = workerCountFunc(func(context.Context) (int, error) { return 0, nil })

	rec := request(deps, "GET", "/api/v2/health/ready", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: the API serves reads with no worker running", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"live_workers":0`) {
		t.Errorf("the empty fleet was not reported: %s", rec.Body.String())
	}
}

type pingerFunc func(context.Context) error

func (f pingerFunc) Ping(ctx context.Context) error { return f(ctx) }

type workerCountFunc func(context.Context) (int, error)

func (f workerCountFunc) LiveWorkers(ctx context.Context) (int, error) { return f(ctx) }
