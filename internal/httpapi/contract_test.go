package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/api"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/XploY04/reelpin-go/internal/search"
	"gopkg.in/yaml.v3"
)

// The contract and the router are two descriptions of one surface. These tests
// walk both directions, so neither can gain or lose an operation alone.

type specOperation struct {
	OperationID string           `yaml:"operationId"`
	Security    []map[string]any `yaml:"security"`
	Responses   map[string]any   `yaml:"responses"`
	Parameters  []map[string]any `yaml:"parameters"`
}

type openAPI struct {
	OpenAPI    string                              `yaml:"openapi"`
	Info       map[string]any                      `yaml:"info"`
	Security   []map[string]any                    `yaml:"security"`
	Paths      map[string]map[string]specOperation `yaml:"paths"`
	Components struct {
		SecuritySchemes map[string]any `yaml:"securitySchemes"`
	} `yaml:"components"`
}

func loadSpec(t *testing.T) openAPI {
	t.Helper()
	var spec openAPI
	if err := yaml.Unmarshal(api.Spec, &spec); err != nil {
		t.Fatalf("the contract does not parse: %v", err)
	}
	if spec.OpenAPI == "" || len(spec.Paths) == 0 {
		t.Fatal("the contract has no version or no paths")
	}
	return spec
}

// specKey is how an operation is addressed in both descriptions.
func specKey(method, path string) string {
	return strings.ToUpper(method) + " " + path
}

func specOperations(t *testing.T) map[string]specOperation {
	t.Helper()
	operations := map[string]specOperation{}
	for path, methods := range loadSpec(t).Paths {
		for method, operation := range methods {
			key := specKey(method, path)
			if _, duplicate := operations[key]; duplicate {
				t.Fatalf("%s is defined twice in the contract", key)
			}
			operations[key] = operation
		}
	}
	return operations
}

func TestEveryRouteIsInTheContract(t *testing.T) {
	operations := specOperations(t)

	for _, route := range newServer(testDeps(&fakePinger{})).RouteManifest() {
		key := specKey(route.Method, route.Path)
		operation, ok := operations[key]
		if !ok {
			t.Errorf("%s is registered but missing from the contract", key)
			continue
		}
		if operation.OperationID != route.OperationID {
			t.Errorf("%s has operationId %q in the contract and %q in the route table",
				key, operation.OperationID, route.OperationID)
		}
	}
}

func TestEveryContractOperationIsRegistered(t *testing.T) {
	registered := map[string]Route{}
	for _, route := range newServer(testDeps(&fakePinger{})).RouteManifest() {
		key := specKey(route.Method, route.Path)
		if _, duplicate := registered[key]; duplicate {
			t.Fatalf("%s is registered twice", key)
		}
		registered[key] = route
	}

	for key := range specOperations(t) {
		if _, ok := registered[key]; !ok {
			t.Errorf("%s is in the contract but not registered; a generated client would call a 404", key)
		}
	}
}

func TestOperationIDsAreUnique(t *testing.T) {
	seen := map[string]string{}
	for key, operation := range specOperations(t) {
		if operation.OperationID == "" {
			t.Errorf("%s has no operationId; a generator needs one", key)
			continue
		}
		if previous, duplicate := seen[operation.OperationID]; duplicate {
			t.Errorf("operationId %q is used by both %s and %s",
				operation.OperationID, previous, key)
		}
		seen[operation.OperationID] = key
	}
}

// TestAuthModeMatchesTheContract is the check that makes AuthMode worth having.
// A boolean could not tell these four apart, and a route that silently changed
// from a share token to a session would still look "authenticated".
func TestAuthModeMatchesTheContract(t *testing.T) {
	spec := loadSpec(t)

	schemeFor := func(operation specOperation) string {
		security := operation.Security
		if security == nil {
			security = spec.Security // the document default
		}
		if len(security) == 0 {
			return "" // explicitly public
		}
		for name := range security[0] {
			return name
		}
		return ""
	}

	wanted := map[AuthMode]string{
		AuthPublic:      "",
		AuthBearer:      "supabaseBearer",
		AuthShareToken:  "nativeShareToken",
		AuthPublicShare: "",
	}

	operations := specOperations(t)
	for _, route := range newServer(testDeps(&fakePinger{})).RouteManifest() {
		operation, ok := operations[specKey(route.Method, route.Path)]
		if !ok {
			continue // the missing-operation test reports this
		}
		if got, want := schemeFor(operation), wanted[route.Auth]; got != want {
			t.Errorf("%s %s: contract security %q, route AuthMode %q (wants %q)",
				route.Method, route.Path, got, route.Auth, want)
		}
	}
}

func TestDeclaredSecuritySchemesExist(t *testing.T) {
	spec := loadSpec(t)
	for _, name := range []string{"supabaseBearer", "nativeShareToken"} {
		if _, ok := spec.Components.SecuritySchemes[name]; !ok {
			t.Errorf("security scheme %q is referenced but not defined", name)
		}
	}
}

// TestNoV1RoutesRemain guards the rule that this service owns v2 and Python
// owns v1. A stray v1 route here would be served by whichever upstream the
// proxy happened to pick.
func TestNoV1RoutesRemain(t *testing.T) {
	for _, route := range newServer(testDeps(&fakePinger{})).RouteManifest() {
		if strings.HasPrefix(route.Path, "/api/v1") {
			t.Errorf("%s belongs to the Python service", route.Path)
		}
		if !strings.HasPrefix(route.Path, "/api/v2") {
			t.Errorf("%s is outside the versioned surface; a bare alias is not part of v2", route.Path)
		}
	}
}

// TestErrorEnvelopeIsUniform sends every failure this package can produce
// without a database and proves each one is the one envelope, with a request id
// a person can quote.
func TestErrorEnvelopeIsUniform(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		target  string
		token   string
		status  int
		code    string
		body    string
		headers map[string]string
	}{
		{name: "unknown route", method: "GET", target: "/api/v2/nope", status: 404, code: "not_found"},
		{name: "wrong method", method: "POST", target: "/api/v2/health/live", status: 405, code: "method_not_allowed"},
		{name: "no token", method: "GET", target: "/api/v2/reels", status: 401, code: "authentication_required"},
		{
			name: "bad cursor", method: "GET", target: "/api/v2/reels?cursor=25",
			token: "Bearer good.token", status: 422, code: "validation_error",
		},
		{
			name: "unknown platform", method: "GET", target: "/api/v2/reels?platform=myspace",
			token: "Bearer good.token", status: 400, code: "invalid_platform",
		},
		{
			// A valid submission with no limiter configured fails closed: a
			// provider call must never be unmetered.
			name: "fails closed without a limiter", method: "POST", target: "/api/v2/processing-jobs/reels",
			token: "Bearer good.token", status: 503, code: "processing_unavailable",
			headers: map[string]string{"Idempotency-Key": "9e0d4f3a-1111-4222-8333-444455556666"},
			body:    `{"url":"https://www.instagram.com/reel/C8abc123/"}`,
		},
		{
			name: "no share token", method: "POST", target: "/api/v2/native-shares/reels",
			status: 401, code: "share_token_required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := tt.body
			if payload == "" {
				payload = "{}"
			}
			req := httptest.NewRequest(tt.method, tt.target, strings.NewReader(payload))
			if tt.token != "" {
				req.Header.Set("Authorization", tt.token)
			}
			for name, value := range tt.headers {
				req.Header.Set(name, value)
			}
			rec := httptest.NewRecorder()
			newServer(testDeps(&fakePinger{})).Routes().ServeHTTP(rec, req)

			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.status, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q", got)
			}

			var body struct {
				Error struct {
					Code      string         `json:"code"`
					Message   string         `json:"message"`
					RequestID string         `json:"request_id"`
					Retryable bool           `json:"retryable"`
					Details   map[string]any `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not the error envelope: %v (%s)", err, rec.Body.String())
			}
			if body.Error.Code != tt.code {
				t.Errorf("code = %q, want %q", body.Error.Code, tt.code)
			}
			if body.Error.Message == "" {
				t.Error("the envelope has no message for a person to read")
			}
			if body.Error.RequestID == "" {
				t.Error("the envelope has no request id to quote")
			}
			if body.Error.RequestID != rec.Header().Get("X-Request-ID") {
				t.Error("the request id in the body does not match the header")
			}
		})
	}
}

// TestFixturesMatchTheContract captures representative responses from the real
// handlers and compares them against the checked-in examples in api/fixtures/.
// They are what a reviewer reads, and what a client's tests are written
// against, so they come from the handlers rather than from anyone's memory.
// Regenerate with -update.
func TestFixturesMatchTheContract(t *testing.T) {
	record := sampleReel(testReelID, testUserID)
	reader := &fakeReels{
		records: []reels.ReelRecord{record},
		byID:    map[string]reels.ReelRecord{testReelID: record},
	}

	tests := []struct {
		name    string
		method  string
		target  string
		token   string
		share   string
		body    string
		headers map[string]string
		status  int
		// searchResponse installs a search service answering with exactly this,
		// so a ranking fixture does not need a database.
		searchResponse *search.Response
	}{
		{name: "health_live", method: "GET", target: "/api/v2/health/live", status: 200},
		{name: "reel_page", method: "GET", target: "/api/v2/reels?limit=25", token: "Bearer good.token", status: 200},
		{name: "reel_detail", method: "GET", target: "/api/v2/reels/" + testReelID, token: "Bearer good.token", status: 200},
		{name: "job_list", method: "GET", target: "/api/v2/processing-jobs", token: "Bearer good.token", status: 200},
		{name: "library_stats", method: "GET", target: "/api/v2/account/library-stats", token: "Bearer good.token", status: 200},
		{name: "error_unauthorized", method: "GET", target: "/api/v2/reels", status: 401},
		{name: "error_not_found", method: "GET", target: "/api/v2/reels/00000000-0000-4000-8000-000000000000", token: "Bearer good.token", status: 404},
		{name: "error_validation", method: "GET", target: "/api/v2/reels?cursor=25", token: "Bearer good.token", status: 422},
		{
			name: "error_processing_unavailable", method: "POST", target: "/api/v2/processing-jobs/reels",
			token: "Bearer good.token", status: 503,
			headers: map[string]string{"Idempotency-Key": "9e0d4f3a-1111-4222-8333-444455556666"},
			body:    `{"url":"https://www.instagram.com/reel/C8abc123/"}`,
		},
		{name: "error_share_token_required", method: "POST", target: "/api/v2/native-shares/reels", status: 401},
		{
			name: "submission_accepted", method: "POST", target: "/api/v2/processing-jobs/reels",
			token: "Bearer good.token", status: 202,
			headers: map[string]string{"Idempotency-Key": "9e0d4f3a-1111-4222-8333-444455556666"},
			body:    `{"url":"https://www.instagram.com/reel/C8abc123/"}`,
		},
		{
			name: "error_idempotency_conflict", method: "POST", target: "/api/v2/processing-jobs/reels",
			token: "Bearer good.token", status: 409,
			headers: map[string]string{"Idempotency-Key": "conflict"},
			body:    `{"url":"https://www.instagram.com/reel/C8abc123/"}`,
		},
		{name: "share_token_minted", method: "POST", target: "/api/v2/share-tokens", token: "Bearer good.token", status: 200},
		{
			name: "share_resolved", method: "POST", target: "/api/v2/share/resolve",
			token: "Bearer good.token", status: 200,
			body: `{"raw_payload_text":"look at this https://www.instagram.com/reel/C8abc123/"}`,
		},
		{
			name: "search_results", method: "POST", target: "/api/v2/search",
			token: "Bearer good.token", status: 200,
			body: `{"query":"artjuna cafe","limit":5}`,
			searchResponse: &search.Response{
				Query:      "artjuna cafe",
				SearchMode: "dense+sparse+fuzzy",
				Total:      1,
				Results: []search.Result{{
					Reel:              reels.BuildDisplayReel(record, testNow),
					RelevanceScore:    0.0492,
					RelevancePercent:  100,
					DisplayScoreLabel: "Strong match",
				}},
			},
		},
		{
			name: "search_empty", method: "POST", target: "/api/v2/search",
			token: "Bearer good.token", status: 200,
			body: `{"query":"scuba diving in switzerland"}`,
			searchResponse: &search.Response{
				Query: "scuba diving in switzerland", SearchMode: "empty",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := testDeps(&fakePinger{})
			deps.Reels = reader
			if tt.searchResponse != nil {
				deps.Search = &fakeSearch{response: *tt.searchResponse}
			}
			// Fixture cases that exercise submissions need the metered path
			// open and deterministic outcomes.
			if tt.status != 503 {
				deps.Limiter = allowAllLimiter{}
			}

			payload := tt.body
			if payload == "" {
				payload = "{}"
			}
			req := httptest.NewRequest(tt.method, tt.target, strings.NewReader(payload))
			// A fixed id keeps the fixture byte-stable across runs.
			req.Header.Set("X-Request-ID", "fixture-request-id")
			if tt.token != "" {
				req.Header.Set("Authorization", tt.token)
			}
			if tt.share != "" {
				req.Header.Set("X-Share-Token", tt.share)
			}
			for name, value := range tt.headers {
				req.Header.Set(name, value)
			}
			rec := httptest.NewRecorder()
			routes(deps).ServeHTTP(rec, req)

			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.status, rec.Body.String())
			}

			var body any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			rendered := mustJSON(t, map[string]any{
				"request":  map[string]any{"method": tt.method, "target": tt.target},
				"status":   tt.status,
				"response": blankVolatile(body),
			})

			path := filepath.Join("..", "..", "api", "fixtures", tt.name+".json")
			if *update {
				if err := os.WriteFile(path, []byte(rendered), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			current, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v; run: go test ./internal/httpapi -update", err)
			}
			if string(current) != rendered {
				t.Fatalf("fixture %s is stale; run: go test ./internal/httpapi -update", tt.name)
			}
		})
	}
}

// blankVolatile blanks the values that change per run, so a fixture diff means
// the contract changed and never that the clock moved.
func blankVolatile(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			switch key {
			case "checked_at", "latency_ms":
				typed[key] = ""
			default:
				typed[key] = blankVolatile(nested)
			}
		}
		return typed
	case []any:
		for index, item := range typed {
			typed[index] = blankVolatile(item)
		}
		return typed
	default:
		return value
	}
}

// TestRouteManifestIsStable writes the manifest the Python contract check and
// the web client read. Regenerate with `go test ./internal/httpapi -update`.
func TestRouteManifestIsStable(t *testing.T) {
	manifest := newServer(testDeps(&fakePinger{})).RouteManifest()
	sort.Slice(manifest, func(i, j int) bool {
		if manifest[i].Path != manifest[j].Path {
			return manifest[i].Path < manifest[j].Path
		}
		return manifest[i].Method < manifest[j].Method
	})

	encoded, err := json.MarshalIndent(map[string]any{
		"note":        "Generated from Server.routeTable(). A change here is an API change.",
		"route_count": len(manifest),
		"routes":      manifest,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')

	path := filepath.Join("..", "..", "api", "routes.json")
	if *update {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; run: go test ./internal/httpapi -update", err)
	}
	if string(current) != string(encoded) {
		t.Fatalf("api/routes.json is stale; run: go test ./internal/httpapi -update\n\nwant:\n%s", encoded)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s\n", encoded)
}
