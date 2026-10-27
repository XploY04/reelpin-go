package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/collections"
	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/metrics"
	"github.com/XploY04/reelpin-go/internal/ratelimit"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/XploY04/reelpin-go/internal/search"
	"github.com/XploY04/reelpin-go/internal/spend"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpMetadataURL = "https://api.test/.well-known/oauth-protected-resource/mcp"

func mcpDeps() Deps {
	deps := testDeps(&fakePinger{})
	deps.PublicAPIBaseURL = "https://api.test"
	deps.OAuthIssuer = "https://auth.test/auth/v1"
	deps.Auth = fakeAuth{userID: testUserID, clientID: "client-1"}
	return deps
}

// bearerRoundTripper signs every request the SDK client sends.
type bearerRoundTripper struct{ token string }

func (b bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(req)
}

func mcpConnect(base, token string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	return client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             base + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerRoundTripper{token: token}},
		DisableStandaloneSSE: true,
	}, nil)
}

func mcpSession(t *testing.T, deps Deps, token string) *mcp.ClientSession {
	t.Helper()
	ts := httptest.NewServer(routes(deps))
	t.Cleanup(ts.Close)
	session, err := mcpConnect(ts.URL, token)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}
	return res
}

func toolText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("the result has no content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, want text", res.Content[0])
	}
	return text.Text
}

func structured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %s", toolText(t, res))
	}
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T", res.StructuredContent)
	}
	return out
}

func toolError(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if !res.IsError {
		t.Fatalf("got a result, want a tool error: %v", res.StructuredContent)
	}
	return toolText(t, res)
}

func object(t *testing.T, value any) map[string]any {
	t.Helper()
	out, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%v is %T, want an object", value, value)
	}
	return out
}

// first is the first element of a list that must not be empty.
func first(t *testing.T, value any) map[string]any {
	t.Helper()
	list, ok := value.([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("%v is not a non-empty list", value)
	}
	return object(t, list[0])
}

// refusingLimiter records every subject it is asked about and refuses one
// policy, for one subject when subject is set.
type refusingLimiter struct {
	policy     string
	subject    string
	retryAfter time.Duration
	subjects   []string
}

func (l *refusingLimiter) Allow(_ context.Context, policy ratelimit.Policy, subject string) (ratelimit.Decision, error) {
	l.subjects = append(l.subjects, subject)
	if policy.Name == l.policy && (l.subject == "" || l.subject == subject) {
		return ratelimit.Decision{Allowed: false, RetryAfter: l.retryAfter}, nil
	}
	return ratelimit.Decision{Allowed: true, Remaining: 1}, nil
}

func TestMCPIsNotServedWithoutAPublicURL(t *testing.T) {
	deps := testDeps(&fakePinger{})
	for _, call := range []struct{ method, target string }{
		{"POST", "/mcp"},
		{"GET", "/.well-known/oauth-protected-resource/mcp"},
	} {
		rec := request(deps, call.method, call.target, "", "Bearer good.token")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s: status = %d, want 404", call.method, call.target, rec.Code)
		}
		if code := decodeError(t, rec).Error.Code; code != "not_found" {
			t.Errorf("%s %s: code = %q, want not_found", call.method, call.target, code)
		}
	}
}

func TestMCPRefusesAPublicURLWithoutAnIssuer(t *testing.T) {
	deps := mcpDeps()
	deps.OAuthIssuer = ""
	if _, err := New(deps); err == nil {
		t.Fatal("New built a server whose MCP metadata names no authorization server")
	}
}

func TestMCPChallengesAnUnauthenticatedRequest(t *testing.T) {
	const challenge = `Bearer resource_metadata="` + mcpMetadataURL + `"`
	const invalid = `Bearer error="invalid_token", resource_metadata="` + mcpMetadataURL + `"`

	tests := []struct {
		name          string
		auth          fakeAuth
		authorization string
		wantCode      string
		wantChallenge string
	}{
		{"missing header", fakeAuth{userID: testUserID, clientID: "client-1"}, "", "authentication_required", challenge},
		{"not a bearer token", fakeAuth{userID: testUserID, clientID: "client-1"}, "Basic abc", "authentication_required", challenge},
		{"rejected token", fakeAuth{err: errFake}, "Bearer bad.token", "invalid_auth_token", invalid},
		{"an app session token", fakeAuth{userID: testUserID}, "Bearer good.token", "invalid_auth_token", invalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := mcpDeps()
			deps.Auth = tt.auth

			rec := request(deps, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, tt.authorization)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (%s)", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if code := decodeError(t, rec).Error.Code; code != tt.wantCode {
				t.Errorf("code = %q, want %q", code, tt.wantCode)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != tt.wantChallenge {
				t.Errorf("WWW-Authenticate = %q, want %q", got, tt.wantChallenge)
			}
		})
	}

	t.Run("the client sees it", func(t *testing.T) {
		deps := mcpDeps()
		deps.Auth = fakeAuth{err: errFake}
		ts := httptest.NewServer(routes(deps))
		defer ts.Close()

		session, err := mcpConnect(ts.URL, "bad.token")
		if err == nil {
			session.Close()
			t.Fatal("connected with a rejected token")
		}
		if !strings.Contains(err.Error(), "Unauthorized") {
			t.Errorf("error = %v, want it to say Unauthorized", err)
		}
	})
}

func TestMCPMetadataNamesTheAuthorizationServer(t *testing.T) {
	rec := serve(mcpDeps(), "GET", "/.well-known/oauth-protected-resource/mcp", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}

	var metadata struct {
		Resource               string   `json:"resource"`
		AuthorizationServers   []string `json:"authorization_servers"`
		BearerMethodsSupported []string `json:"bearer_methods_supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &metadata); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	if metadata.Resource != "https://api.test/mcp" {
		t.Errorf("resource = %q", metadata.Resource)
	}
	if !slices.Equal(metadata.AuthorizationServers, []string{"https://auth.test/auth/v1"}) {
		t.Errorf("authorization_servers = %v", metadata.AuthorizationServers)
	}
	if !slices.Equal(metadata.BearerMethodsSupported, []string{"header"}) {
		t.Errorf("bearer_methods_supported = %v", metadata.BearerMethodsSupported)
	}
}

func TestMCPMetadataAnswersHeadAndRefusesOtherMethods(t *testing.T) {
	const path = "/.well-known/oauth-protected-resource/mcp"
	if rec := serve(mcpDeps(), "HEAD", path, ""); rec.Code != http.StatusOK {
		t.Errorf("HEAD status = %d, want 200", rec.Code)
	}

	rec := serve(mcpDeps(), "POST", path, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want GET, HEAD", got)
	}
}

// RFC 9728 has a root document name the origin, which is not a protected
// resource here, so a client is never handed one naming /mcp.
func TestMCPServesNoRootMetadata(t *testing.T) {
	rec := serve(mcpDeps(), "GET", "/.well-known/oauth-protected-resource", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestMCPRefusesARequestFromAnotherSite(t *testing.T) {
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer good.token")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	routes(mcpDeps()).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "cross_origin_forbidden" {
		t.Errorf("code = %q, want cross_origin_forbidden", code)
	}
}

func TestMCPAnswersAJSON405ToGet(t *testing.T) {
	rec := serve(mcpDeps(), "GET", "/mcp", "Bearer good.token")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "method_not_allowed" {
		t.Errorf("code = %q, want method_not_allowed", code)
	}
	if got := rec.Header().Get("Allow"); got != "POST" {
		t.Errorf("Allow = %q, want POST", got)
	}
}

func TestMCPListsTheTools(t *testing.T) {
	session := mcpSession(t, mcpDeps(), "good.token")
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("listing tools: %v", err)
	}

	names := []string{}
	required := map[string][]any{}
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		schema := object(t, tool.InputSchema)
		list, _ := schema["required"].([]any)
		required[tool.Name] = list
	}
	slices.Sort(names)
	want := []string{"add_to_collection", "get_collection", "get_reel", "list_collections", "list_reels", "save_link", "search_reels"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	if !slices.Equal(required["get_reel"], []any{"reel_id"}) {
		t.Errorf("get_reel requires %v, want reel_id", required["get_reel"])
	}
	if len(required["list_reels"]) != 0 {
		t.Errorf("list_reels requires %v, want nothing", required["list_reels"])
	}
}

func TestMCPListReelsPagesTheUsersLibrary(t *testing.T) {
	deps := mcpDeps()
	fake := &fakeReels{records: []reels.ReelRecord{sampleReel(testReelID, testUserID)}}
	deps.Reels = fake

	out := structured(t, callTool(t, mcpSession(t, deps, "good.token"), "list_reels", map[string]any{"limit": 25}))
	reel := first(t, out["reels"])
	if reel["id"] != testReelID {
		t.Errorf("id = %v", reel["id"])
	}
	if reel["web_url"] != "https://reelpin.in/r/"+testReelID {
		t.Errorf("web_url = %v", reel["web_url"])
	}
	if reel["platform"] != "instagram" {
		t.Errorf("platform = %v", reel["platform"])
	}
	if out["has_more"] != false {
		t.Errorf("has_more = %v", out["has_more"])
	}
	if fake.lastUserID != testUserID {
		t.Errorf("listed as %q, want the token subject", fake.lastUserID)
	}
	if fake.lastOptions.Limit != 26 {
		t.Errorf("read limit = %d, want 26: one extra row answers has_more", fake.lastOptions.Limit)
	}
}

func TestMCPListReelsRejectsAnUnknownPlatform(t *testing.T) {
	text := toolError(t, callTool(t, mcpSession(t, mcpDeps(), "good.token"), "list_reels", map[string]any{"platform": "myspace"}))
	if !strings.Contains(text, "myspace") {
		t.Errorf("error = %q, want it to name the platform", text)
	}
}

func TestMCPListReelsRejectsAForeignCursor(t *testing.T) {
	text := toolError(t, callTool(t, mcpSession(t, mcpDeps(), "good.token"), "list_reels", map[string]any{"cursor": "25"}))
	if !strings.Contains(text, "cursor") {
		t.Errorf("error = %q, want it to name the cursor", text)
	}
}

func TestMCPGetReelAnswersTheFullReel(t *testing.T) {
	deps := mcpDeps()
	deps.Reels = &fakeReels{byID: map[string]reels.ReelRecord{testReelID: sampleReel(testReelID, testUserID)}}

	out := structured(t, callTool(t, mcpSession(t, deps, "good.token"), "get_reel", map[string]any{"reel_id": testReelID}))
	if out["transcript"] != "spoken words" {
		t.Errorf("transcript = %v", out["transcript"])
	}
	if out["web_url"] != "https://reelpin.in/r/"+testReelID {
		t.Errorf("web_url = %v", out["web_url"])
	}
	if name := first(t, out["locations"])["name"]; name != "Artjuna Cafe" {
		t.Errorf("locations[0].name = %v", name)
	}
	facts, _ := out["key_facts"].([]any)
	if len(facts) == 0 || facts[0] != "Opens at 8am" {
		t.Errorf("key_facts = %v", out["key_facts"])
	}
}

func TestMCPGetReelHidesAnotherUsersReel(t *testing.T) {
	deps := mcpDeps()
	deps.Reels = &fakeReels{byID: map[string]reels.ReelRecord{testReelID: sampleReel(testReelID, otherUserID)}}
	session := mcpSession(t, deps, "good.token")

	foreign := toolError(t, callTool(t, session, "get_reel", map[string]any{"reel_id": testReelID}))
	if foreign != "reel not found" {
		t.Errorf("error = %q, want reel not found", foreign)
	}
	malformed := toolError(t, callTool(t, session, "get_reel", map[string]any{"reel_id": "not-a-uuid"}))
	if malformed != foreign {
		t.Errorf("a malformed id answered %q, a foreign one %q: the two must not be told apart", malformed, foreign)
	}
}

func TestMCPSearchRunsTheUsersQuery(t *testing.T) {
	deps := mcpDeps()
	deps.Limiter = allowAllLimiter{}
	record := sampleReel(testReelID, testUserID)
	fake := &fakeSearch{response: search.Response{
		Query: "cafes", SearchMode: "sparse",
		Results: []search.Result{{
			Reel:              reels.BuildDisplayReel(record, reels.Display{Now: testNow, WebBaseURL: testWebBaseURL}),
			RelevancePercent:  100,
			DisplayScoreLabel: "Strong match",
		}},
	}}
	deps.Search = fake

	out := structured(t, callTool(t, mcpSession(t, deps, "good.token"), "search_reels",
		map[string]any{"query": "cafes", "platform": "instagram", "limit": 5}))
	if fake.lastUserID != testUserID {
		t.Errorf("searched as %q, want the token subject", fake.lastUserID)
	}
	if fake.lastQuery != "cafes" {
		t.Errorf("query = %q", fake.lastQuery)
	}
	if !slices.Equal(fake.lastFilters.Platforms, []string{"instagram"}) {
		t.Errorf("platforms = %v", fake.lastFilters.Platforms)
	}
	if fake.lastView != reels.ViewConcise {
		t.Errorf("view = %q, want concise", fake.lastView)
	}

	hit := first(t, out["results"])
	if web := object(t, hit["reel"])["web_url"]; web != "https://reelpin.in/r/"+testReelID {
		t.Errorf("results[0].reel.web_url = %v", web)
	}
	if hit["relevance_percent"] != float64(100) {
		t.Errorf("relevance_percent = %v", hit["relevance_percent"])
	}
	if hit["match_label"] != "Strong match" {
		t.Errorf("match_label = %v", hit["match_label"])
	}
}

func TestMCPSearchFailsClosedWithoutALimiter(t *testing.T) {
	text := toolError(t, callTool(t, mcpSession(t, mcpDeps(), "good.token"), "search_reels", map[string]any{"query": "cafes"}))
	if !strings.Contains(text, "unavailable") {
		t.Errorf("error = %q, want it to say search is unavailable", text)
	}
}

func TestMCPSearchIsRateLimitedPerClient(t *testing.T) {
	clientBucket := testUserID + "|client:client-1"
	limiter := &refusingLimiter{policy: ratelimit.Search.Name, subject: clientBucket, retryAfter: 30 * time.Second}
	deps := mcpDeps()
	deps.Limiter = limiter

	text := toolError(t, callTool(t, mcpSession(t, deps, "good.token"), "search_reels", map[string]any{"query": "cafes"}))
	if !strings.Contains(text, "30 seconds") {
		t.Errorf("error = %q, want the retry delay", text)
	}
	if !slices.Equal(limiter.subjects, []string{clientBucket}) {
		t.Errorf("checked %v, want the client's bucket first and nothing after the refusal", limiter.subjects)
	}
}

func TestMCPSaveLinkStartsAJob(t *testing.T) {
	submitter := &fakeSubmitter{}
	deps := mcpDeps()
	deps.Limiter = allowAllLimiter{}
	deps.Enqueue = submitter

	out := structured(t, callTool(t, mcpSession(t, deps, "good.token"), "save_link",
		map[string]any{"url": "https://www.instagram.com/reel/C8abc123/"}))
	if out["status"] != "processing" || out["already_saved"] != false {
		t.Errorf("status = %v, already_saved = %v", out["status"], out["already_saved"])
	}
	job := object(t, out["job"])
	if job["id"] != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" || job["status"] != "queued" {
		t.Errorf("job = %v", job)
	}
	if message, _ := job["status_message"].(string); message == "" {
		t.Error("the job has no status message")
	}

	if submitter.lastRequest.UserID != testUserID {
		t.Errorf("submitted as %q, want the token subject", submitter.lastRequest.UserID)
	}
	if submitter.lastRequest.Endpoint != "mcp/save_link" {
		t.Errorf("endpoint = %q, want mcp/save_link", submitter.lastRequest.Endpoint)
	}
	if _, err := uuid.Parse(submitter.lastRequest.IdempotencyKey); err != nil {
		t.Errorf("idempotency key %q is not a UUID", submitter.lastRequest.IdempotencyKey)
	}
}

func TestMCPSaveLinkKeepsTheCallersIdempotencyKey(t *testing.T) {
	submitter := &fakeSubmitter{}
	deps := mcpDeps()
	deps.Limiter = allowAllLimiter{}
	deps.Enqueue = submitter
	session := mcpSession(t, deps, "good.token")

	structured(t, callTool(t, session, "save_link",
		map[string]any{"url": "https://www.instagram.com/reel/C8abc123/", "idempotency_key": validKey}))
	if submitter.lastRequest.IdempotencyKey != validKey {
		t.Errorf("key = %q, want the caller's %q", submitter.lastRequest.IdempotencyKey, validKey)
	}

	text := toolError(t, callTool(t, session, "save_link",
		map[string]any{"url": "https://www.instagram.com/reel/C8abc123/", "idempotency_key": "not-a-uuid"}))
	if !strings.Contains(text, "idempotency_key") {
		t.Errorf("error = %q, want it to name idempotency_key", text)
	}
}

func TestMCPSaveLinkAnswersAnExistingSave(t *testing.T) {
	record := sampleReel(testReelID, testUserID)
	deps := mcpDeps()
	deps.Limiter = allowAllLimiter{}
	deps.Enqueue = &fakeSubmitter{result: enqueue.Result{Kind: enqueue.AlreadySaved, Reel: &record}}

	out := structured(t, callTool(t, mcpSession(t, deps, "good.token"), "save_link",
		map[string]any{"url": "https://www.instagram.com/reel/abc/"}))
	if out["status"] != "saved" || out["already_saved"] != true {
		t.Errorf("status = %v, already_saved = %v", out["status"], out["already_saved"])
	}
	reel := object(t, out["reel"])
	if reel["id"] != testReelID || reel["web_url"] != "https://reelpin.in/r/"+testReelID {
		t.Errorf("reel = %v", reel)
	}
}

func TestMCPSaveLinkReplaysAFinishedJobAsItStands(t *testing.T) {
	deps := mcpDeps()
	deps.Limiter = allowAllLimiter{}
	deps.Enqueue = &fakeSubmitter{result: enqueue.Result{Kind: enqueue.Accepted, Replayed: true, Job: &enqueue.Job{
		ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Status: "failed", FailureCode: stringPtr("no_audio"),
		CollectionIDs: []string{},
	}}}

	out := structured(t, callTool(t, mcpSession(t, deps, "good.token"), "save_link",
		map[string]any{"url": "https://www.instagram.com/reel/abc/"}))
	if out["status"] != "failed" {
		t.Errorf("status = %v, want failed: the job is no longer processing", out["status"])
	}
	// The reason, not the generic sentence, is what lets the assistant tell the user why.
	if got, want := object(t, out["job"])["status_message"], jobs.StatusMessage(jobs.JobRecord{Status: "failed", FailureCode: stringPtr("no_audio")}); got != want {
		t.Errorf("status_message = %v, want %q", got, want)
	}
}

func TestMCPSaveLinkMapsTheSubmissionErrors(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{enqueue.ErrActiveJobLimit, "processing"},
		{enqueue.ErrIdempotencyMismatch, "idempotency_key"},
		{enqueue.ErrUnsupported, "not a link"},
		{enqueue.ErrCollectionUnreachable, "collection_ids"},
		{spend.ErrLimitReached, "paused"},
		{errFake, "right now"},
	}
	for _, tt := range tests {
		t.Run(tt.err.Error(), func(t *testing.T) {
			deps := mcpDeps()
			deps.Limiter = allowAllLimiter{}
			deps.Enqueue = &fakeSubmitter{err: tt.err}

			text := toolError(t, callTool(t, mcpSession(t, deps, "good.token"), "save_link",
				map[string]any{"url": "https://www.instagram.com/reel/C8abc123/"}))
			if !strings.Contains(text, tt.want) {
				t.Errorf("error = %q, want it to contain %q", text, tt.want)
			}
			if strings.Contains(text, errFake.Error()) {
				t.Errorf("the internal error reached the tool result: %q", text)
			}
		})
	}
}

func TestMCPSaveLinkFailsClosedWithoutALimiter(t *testing.T) {
	text := toolError(t, callTool(t, mcpSession(t, mcpDeps(), "good.token"), "save_link",
		map[string]any{"url": "https://www.instagram.com/reel/C8abc123/"}))
	if !strings.Contains(text, "unavailable") {
		t.Errorf("error = %q, want it to say saving is unavailable", text)
	}
}

func TestMCPSaveLinkIsRateLimited(t *testing.T) {
	deps := mcpDeps()
	deps.Limiter = &refusingLimiter{policy: ratelimit.Submission.Name, retryAfter: 60 * time.Second}

	text := toolError(t, callTool(t, mcpSession(t, deps, "good.token"), "save_link",
		map[string]any{"url": "https://www.instagram.com/reel/C8abc123/"}))
	if !strings.Contains(text, "60 seconds") {
		t.Errorf("error = %q, want the retry delay", text)
	}
}

func TestMCPWritesDropTheUsersCachedReads(t *testing.T) {
	cache := &fakeCache{}
	fake := newFakeCollections()
	fake.added = 1
	deps := mcpDeps()
	deps.Limiter = allowAllLimiter{}
	deps.Cache = cache
	deps.Collections = fake
	session := mcpSession(t, deps, "good.token")

	structured(t, callTool(t, session, "save_link", map[string]any{"url": "https://www.instagram.com/reel/C8abc123/"}))
	if !slices.Equal(cache.users, []string{testUserID}) {
		t.Fatalf("after save_link invalidated %v, want the token subject once", cache.users)
	}
	structured(t, callTool(t, session, "add_to_collection",
		map[string]any{"collection_id": testCollectionID, "reel_ids": []string{testReelID}}))
	if !slices.Equal(cache.users, []string{testUserID, testUserID}) {
		t.Fatalf("after add_to_collection invalidated %v, want the token subject once more", cache.users)
	}
	structured(t, callTool(t, session, "list_reels", map[string]any{}))
	if len(cache.users) != 2 {
		t.Errorf("a read invalidated: %v", cache.users)
	}
}

func TestMCPListCollectionsAnswersTheUsersCollections(t *testing.T) {
	out := structured(t, callTool(t, mcpSession(t, mcpDeps(), "good.token"), "list_collections", map[string]any{}))
	collection := first(t, out["collections"])
	if collection["id"] != testCollectionID {
		t.Errorf("id = %v", collection["id"])
	}
	if collection["web_url"] != "https://reelpin.in/collections/"+testCollectionID {
		t.Errorf("web_url = %v", collection["web_url"])
	}
	if collection["role"] != "owner" {
		t.Errorf("role = %v", collection["role"])
	}
	if collection["item_count"] != float64(1) {
		t.Errorf("item_count = %v", collection["item_count"])
	}
}

func TestMCPGetCollectionPagesItsItems(t *testing.T) {
	fake := newFakeCollections()
	fake.items = fixtureCollectionItems(sampleReel(testReelID, testUserID))
	deps := mcpDeps()
	deps.Collections = fake
	session := mcpSession(t, deps, "good.token")

	out := structured(t, callTool(t, session, "get_collection", map[string]any{"collection_id": testCollectionID}))
	item := first(t, out["items"])
	if item["id"] != testReelID {
		t.Errorf("items[0].id = %v", item["id"])
	}
	if added, _ := item["added_at"].(string); added == "" {
		t.Errorf("items[0].added_at = %v", item["added_at"])
	}
	if out["can_edit"] != true {
		t.Errorf("can_edit = %v", out["can_edit"])
	}
	if fake.lastQuery.Transcripts {
		t.Error("the page read transcripts it never sends")
	}
	if fake.lastQuery.Limit != 25 {
		t.Errorf("limit = %d, want the default 25", fake.lastQuery.Limit)
	}

	fake.err = collections.ErrNotFound
	if text := toolError(t, callTool(t, session, "get_collection", map[string]any{"collection_id": testCollectionID})); text != "collection not found" {
		t.Errorf("missing collection: error = %q", text)
	}
	if text := toolError(t, callTool(t, session, "get_collection", map[string]any{"collection_id": "not-a-uuid"})); text != "collection not found" {
		t.Errorf("malformed id: error = %q", text)
	}
}

func TestMCPAddToCollectionFilesTheUsersReels(t *testing.T) {
	fake := newFakeCollections()
	fake.added = 1
	deps := mcpDeps()
	deps.Collections = fake

	out := structured(t, callTool(t, mcpSession(t, deps, "good.token"), "add_to_collection",
		map[string]any{"collection_id": testCollectionID, "reel_ids": []string{testReelID}}))
	if out["added_count"] != float64(1) {
		t.Errorf("added_count = %v", out["added_count"])
	}
	if id := object(t, out["collection"])["id"]; id != testCollectionID {
		t.Errorf("collection.id = %v", id)
	}
	if fake.lastAction != "add_items" || !slices.Equal(fake.lastSaveIDs, []string{testReelID}) {
		t.Errorf("action = %q, ids = %v", fake.lastAction, fake.lastSaveIDs)
	}
	if fake.lastUserID != testUserID {
		t.Errorf("filed as %q, want the token subject", fake.lastUserID)
	}
}

func TestMCPAddToCollectionMapsTheErrors(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		reelIDs []string
		want    string
	}{
		{"read-only collection", collections.ErrForbidden, []string{testReelID}, "read-only"},
		{"someone else's reel", reels.ErrNotFound, []string{testReelID}, "reel not found"},
		{"no reels", nil, []string{}, "reel_ids"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeCollections()
			fake.err = tt.err
			deps := mcpDeps()
			deps.Collections = fake

			text := toolError(t, callTool(t, mcpSession(t, deps, "good.token"), "add_to_collection",
				map[string]any{"collection_id": testCollectionID, "reel_ids": tt.reelIDs}))
			if !strings.Contains(text, tt.want) {
				t.Errorf("error = %q, want it to contain %q", text, tt.want)
			}
		})
	}
}

func TestMCPRequestsAreCountedByRouteNotByTool(t *testing.T) {
	deps := observedDeps()
	deps.PublicAPIBaseURL = "https://api.test"
	deps.OAuthIssuer = "https://auth.test/auth/v1"
	deps.Auth = fakeAuth{userID: testUserID, clientID: "client-1"}

	// The request is observed after its response is written, so the server
	// has to drain before the registry is read.
	ts := httptest.NewServer(routes(deps))
	session, err := mcpConnect(ts.URL, "good.token")
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	structured(t, callTool(t, session, "list_reels", map[string]any{}))
	session.Close()
	ts.Close()

	body := scrape(t, deps)
	if !strings.Contains(body, `route="/mcp"`) {
		t.Fatalf("no /mcp series in:\n%s", body)
	}
	if strings.Contains(body, "list_reels") {
		t.Error("a tool name became a label")
	}
}

func TestMCPLogLinesCarryTheHashedUser(t *testing.T) {
	deps := mcpDeps()
	var logged bytes.Buffer
	deps.Logger = slog.New(slog.NewJSONHandler(&logged, nil))

	// The request line is logged after the response is written, so the server
	// has to drain before the buffer is read.
	ts := httptest.NewServer(routes(deps))
	session, err := mcpConnect(ts.URL, "good.token")
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	structured(t, callTool(t, session, "list_reels", map[string]any{}))
	session.Close()
	ts.Close()

	line := logged.String()
	if strings.Contains(line, testUserID) {
		t.Fatalf("the user id reached the log: %s", line)
	}
	if !strings.Contains(line, metrics.Hash(testUserID)) {
		t.Errorf("the hashed user is missing from: %s", line)
	}
	if !strings.Contains(line, `"/mcp"`) {
		t.Errorf("the route is missing from: %s", line)
	}
}

func TestMCPRecordsTheUserMigration(t *testing.T) {
	seen := &seenUsers{}
	deps := mcpDeps()
	deps.Migrations = seen

	session := mcpSession(t, deps, "good.token")
	// Connecting is a request of its own, recorded the same way.
	seen.users = nil

	structured(t, callTool(t, session, "list_reels", map[string]any{}))
	if !slices.Equal(seen.users, []string{testUserID}) {
		t.Errorf("seen = %v, want just the verified user", seen.users)
	}
}
