package httpapi

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/reels"
)

// -update rewrites the golden files instead of comparing against them.
var update = flag.Bool("update", false, "rewrite contract golden files")

const contractDir = "../../api/contract"

// volatileFields change on every run, so they are blanked before comparing.
// Their presence and type are still checked, because the blanking only happens
// when the key exists.
var volatileFields = map[string]bool{
	"checked_at": true,
	"latency_ms": true,
}

func TestRouteManifestIsUnchanged(t *testing.T) {
	manifest := New(testDeps(&fakePinger{})).RouteManifest()
	assertGolden(t, filepath.Join(contractDir, "go-routes.json"), mustEncode(t, map[string]any{
		"note":        "Registered Go routes. A change here is an API contract change.",
		"route_count": len(manifest),
		"routes":      manifest,
	}))
}

func TestResponseContract(t *testing.T) {
	fullReels := func() *fakeReels {
		record := sampleReel(testReelID, testUserID)
		return &fakeReels{
			records: []reels.ReelRecord{record},
			byID:    map[string]reels.ReelRecord{testReelID: record},
			facets:  facetFixture(),
			stats: reels.LibraryStats{
				TotalReels: 3, TotalPinnedLocations: 1, TotalTags: 2,
				TotalCategories: 2, TotalSubcategories: 2,
			},
		}
	}

	completedJob := sampleJob(testJobID, testUserID, jobs.StatusCompleted)
	completedJob.ResultReelID = stringPtr(testReelID)
	completedJob.CollectionIDs = []string{"fedcba98-0000-4000-8000-000000000001"}

	tests := []struct {
		name       string
		target     string
		method     string
		token      string
		wantStatus int
		deps       func() Deps
	}{
		{name: "health_live", target: "/api/v1/health/live", wantStatus: 200},
		{name: "health_ready_ok", target: "/api/v1/health/ready", wantStatus: 200},
		{
			name: "health_ready_degraded", target: "/api/v1/health/ready", wantStatus: 503,
			deps: func() Deps { return testDeps(&fakePinger{err: errFake}) },
		},
		{
			name: "health_compat_degraded", target: "/api/v1/health", wantStatus: 200,
			deps: func() Deps { return testDeps(&fakePinger{err: errFake}) },
		},
		{name: "reels_list", target: "/api/v1/reels?limit=1", token: "Bearer good.token", wantStatus: 200},
		{name: "reel_detail", target: "/api/v1/reels/" + testReelID, token: "Bearer good.token", wantStatus: 200},
		{name: "reels_platform_filters", target: "/api/v1/reels/filters?platform=instagram", token: "Bearer good.token", wantStatus: 200},
		{name: "reels_category_filters", target: "/api/v1/reels/category-filters?category=food", token: "Bearer good.token", wantStatus: 200},
		{
			name: "processing_jobs_list", target: "/api/v1/processing-jobs?active_only=false&limit=20",
			token: "Bearer good.token", wantStatus: 200,
			deps: func() Deps {
				deps := testDeps(&fakePinger{})
				deps.Reels = fullReels()
				deps.Jobs = &fakeJobs{records: []jobs.JobRecord{completedJob}}
				return deps
			},
		},
		{
			name: "processing_job_detail", target: "/api/v1/processing-jobs/" + testJobID,
			token: "Bearer good.token", wantStatus: 200,
			deps: func() Deps {
				deps := testDeps(&fakePinger{})
				deps.Reels = fullReels()
				deps.Jobs = &fakeJobs{records: []jobs.JobRecord{completedJob}}
				return deps
			},
		},
		{name: "account_library_stats", target: "/api/v1/account/library-stats", token: "Bearer good.token", wantStatus: 200},
		{name: "account_entitlements", target: "/api/v1/account/entitlements", token: "Bearer good.token", wantStatus: 200},
		{
			name: "account_entitlements_restricted", target: "/api/v1/account/entitlements",
			token: "Bearer good.token", wantStatus: 200,
			deps: func() Deps {
				deps := testDeps(&fakePinger{})
				deps.Reels = &fakeReels{err: errFake}
				return deps
			},
		},

		{name: "error_authentication_required", target: "/api/v1/reels", wantStatus: 401},
		{
			name: "error_invalid_auth_token", target: "/api/v1/reels", token: "Bearer bad.token", wantStatus: 401,
			deps: func() Deps {
				deps := testDeps(&fakePinger{})
				deps.Auth = fakeAuth{err: errFake}
				return deps
			},
		},
		{name: "error_not_found", target: "/api/v1/nope", wantStatus: 404},
		{name: "error_method_not_allowed", method: "POST", target: "/api/v1/health/live", wantStatus: 405},
		{name: "error_reel_not_found", target: "/api/v1/reels/not-a-uuid", token: "Bearer good.token", wantStatus: 404},
		{name: "error_validation", target: "/api/v1/reels?limit=0", token: "Bearer good.token", wantStatus: 422},
		{name: "error_invalid_platform", target: "/api/v1/reels?platform=myspace", token: "Bearer good.token", wantStatus: 400},
		{
			name: "error_reel_list_failed", target: "/api/v1/reels", token: "Bearer good.token", wantStatus: 500,
			deps: func() Deps {
				deps := testDeps(&fakePinger{})
				deps.Reels = &fakeReels{err: errFake}
				return deps
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := testDeps(&fakePinger{})
			if tt.deps != nil {
				deps = tt.deps()
			} else {
				deps.Reels = fullReels()
				deps.Jobs = &fakeJobs{records: []jobs.JobRecord{completedJob}}
			}

			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			req := httptest.NewRequest(method, tt.target, nil)
			if tt.token != "" {
				req.Header.Set("Authorization", tt.token)
			}
			rec := httptest.NewRecorder()
			New(deps).Routes().ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}

			var body any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
			}
			assertGolden(t, filepath.Join(contractDir, "fixtures", tt.name+".json"), mustEncode(t, map[string]any{
				"request":  map[string]any{"method": method, "target": tt.target},
				"status":   tt.wantStatus,
				"response": blankVolatile(body),
			}))
		})
	}
}

// blankVolatile replaces timestamps and latencies so golden files stay stable
// while still asserting the field exists.
func blankVolatile(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if volatileFields[key] && nested != nil {
				typed[key] = "<volatile>"
				continue
			}
			typed[key] = blankVolatile(nested)
		}
		return typed
	case []any:
		for i, nested := range typed {
			typed[i] = blankVolatile(nested)
		}
		return typed
	}
	return value
}

func mustEncode(t *testing.T, value any) []byte {
	t.Helper()
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		t.Fatalf("encoding golden value: %v", err)
	}
	return buf.Bytes()
}

func assertGolden(t *testing.T, path string, actual []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating golden directory: %v", err)
		}
		if err := os.WriteFile(path, actual, 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file (run `go test ./internal/httpapi -update`): %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(actual)) {
		t.Errorf("contract changed for %s\n--- want ---\n%s\n--- got ---\n%s", path, want, actual)
	}
}
