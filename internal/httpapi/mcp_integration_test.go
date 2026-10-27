//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/auth"
	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/entitlements"
	"github.com/XploY04/reelpin-go/internal/httpapi"
	"github.com/XploY04/reelpin-go/internal/lifecycle"
	"github.com/XploY04/reelpin-go/internal/mapview"
	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/XploY04/reelpin-go/internal/notify"
	"github.com/XploY04/reelpin-go/internal/pipeline"
	"github.com/XploY04/reelpin-go/internal/postgres"
	"github.com/XploY04/reelpin-go/internal/pricetracks"
	"github.com/XploY04/reelpin-go/internal/profiles"
	"github.com/XploY04/reelpin-go/internal/ratelimit"
	"github.com/XploY04/reelpin-go/internal/reminders"
	"github.com/XploY04/reelpin-go/internal/search"
	"github.com/XploY04/reelpin-go/internal/sharetoken"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/XploY04/reelpin-go/internal/website"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

// tokenAuth stands in for the JWKS verifier: two OAuth-client tokens, one per
// user, and nothing else.
type tokenAuth struct{}

func (tokenAuth) Authenticate(_ context.Context, token string) (auth.Principal, error) {
	switch token {
	case "a.token":
		return auth.Principal{UserID: userA, ClientID: "client-a"}, nil
	case "b.token":
		return auth.Principal{UserID: userB, ClientID: "client-b"}, nil
	}
	return auth.Principal{}, auth.ErrUnauthenticated
}

type allowAll struct{}

func (allowAll) Allow(context.Context, ratelimit.Policy, string) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: true, Remaining: 1}, nil
}

// Forced cleanup keeps a failed test's open connection from blocking database removal.
func mcpPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer admin.Close()

	name := "reelpin_mcp_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	if len(name) > 60 {
		name = name[:60]
	}
	for _, statement := range []string{
		`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`,
		`CREATE DATABASE ` + name,
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatalf("preparing %s: %v", name, err)
		}
	}

	parsed, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name

	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connecting to %s: %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanup, err := pgxpool.New(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})

	if _, err := pool.Exec(ctx, `
		CREATE SCHEMA auth;
		CREATE TABLE auth.users (
			id         UUID PRIMARY KEY,
			email      TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatalf("creating the production-shaped auth schema: %v", err)
	}
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	var noLegacyReels bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.reels') IS NULL`).Scan(&noLegacyReels); err != nil {
		t.Fatalf("checking the canonical-only schema: %v", err)
	}
	if !noLegacyReels {
		t.Fatal("public.reels exists, want a canonical-only schema")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth.users (id) VALUES ($1), ($2)`, userA, userB); err != nil {
		t.Fatalf("seeding users: %v", err)
	}
	return pool
}

func seedSave(t *testing.T, pool *pgxpool.Pool, userID, sourceID string) string {
	t.Helper()
	ctx := context.Background()

	var contentID, versionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id,
			 normalized_url, normalized_url_hash, access_scope_hash)
		VALUES ('instagram', 'reel', $1, 'https://www.instagram.com/reel/'||$1||'/', $1, 'public')
		RETURNING id::text`, sourceID).Scan(&contentID); err != nil {
		t.Fatalf("seeding content: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, prompt_version, schema_version, model_version, title, summary,
			 raw_extraction, media)
		VALUES ($1, 'v1', 'p1', 's1', 'm1', 'Best cafes in Goa', 'Three cafes worth the ride.',
		        '{"category":"food_and_drink","subcategory":"cafes"}'::jsonb,
		        '{"thumbnail_url":"https://cdn/x.jpg"}'::jsonb)
		RETURNING id::text`, contentID).Scan(&versionID); err != nil {
		t.Fatalf("seeding a version: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.contents SET current_version_id = $2 WHERE id = $1`, contentID, versionID); err != nil {
		t.Fatalf("pointing at the version: %v", err)
	}

	var saveID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.user_saves (user_id, content_id) VALUES ($1, $2)
		RETURNING id::text`, userID, contentID).Scan(&saveID); err != nil {
		t.Fatalf("seeding a save: %v", err)
	}
	return saveID
}

// realStack builds the API the way cmd/api does, minus what no tool reaches:
// the services the route table requires but MCP never calls are the cheapest
// values New accepts.
func realStack(pool *pgxpool.Pool) (*httpapi.Server, error) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	searchService := search.NewService(pool, nil, logger, time.Now)
	searchService.WebBaseURL = "https://reelpin.in"
	mapService := mapview.New(pool, time.Now)

	return httpapi.New(httpapi.Deps{
		DB:            pool,
		Auth:          tokenAuth{},
		Reels:         postgres.NewReels(pool),
		Jobs:          postgres.NewJobs(pool),
		Entitlements:  entitlements.NewCache(postgres.NewEntitlements(pool), time.Minute, time.Now, logger),
		Enqueue:       enqueue.New(postgres.NewEnqueue(pool), &sourceidentity.Resolver{}, nil),
		ShareTokens:   sharetoken.NewStore(pool),
		Resolver:      &sourceidentity.Resolver{},
		Collections:   postgres.NewCollections(pool, "https://reelpin.in", time.Now),
		Notifications: notify.NewService(pool, nil, logger, time.Now),
		Lifecycle:     lifecycle.New(pool, nil, nil, logger),
		Map:           mapService,
		MapPlaces:     mapview.NewPlaces(mapService, nil, logger),
		Profiles:      profiles.New(pool),
		Website:       website.New(pool),
		Search:        searchService,
		Reminders:     reminders.NewService(postgres.NewReminders(pool), nil, logger),
		PriceTracks:   pricetracks.NewService(postgres.NewPriceTracks(pool)),
		Limiter:       allowAll{},
		Logger:        logger,
		Version:       "test",

		PublicWebBaseURL: "https://reelpin.in",
		PublicAPIBaseURL: "https://api.test",
		OAuthIssuer:      "https://auth.test/auth/v1",
		StageBudgets:     pipeline.StageBudgets(),
	})
}

type bearer struct{ token string }

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func connect(base, token string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	return client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             base + "/mcp",
		HTTPClient:           &http.Client{Transport: bearer{token: token}},
		DisableStandaloneSSE: true,
	}, nil)
}

func session(t *testing.T, base, token string) *mcp.ClientSession {
	t.Helper()
	s, err := connect(base, token)
	if err != nil {
		t.Fatalf("connecting with %s: %v", token, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}
	return res
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("the result has no content")
	}
	content, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, want text", res.Content[0])
	}
	return content.Text
}

func result(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %s", text(t, res))
	}
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T", res.StructuredContent)
	}
	return out
}

func list(t *testing.T, value any) []map[string]any {
	t.Helper()
	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("%v is %T, want a list", value, value)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		object, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%v is %T, want an object", item, item)
		}
		out = append(out, object)
	}
	return out
}

func TestMCPServesTheRealStack(t *testing.T) {
	pool := mcpPool(t)
	saveID := seedSave(t, pool, userA, "MCPSEED1")
	otherSaveID := seedSave(t, pool, userB, "MCPSEED2")

	api, err := realStack(pool)
	if err != nil {
		t.Fatalf("building the API: %v", err)
	}
	ts := httptest.NewServer(api.Routes())
	t.Cleanup(ts.Close)
	a := session(t, ts.URL, "a.token")
	b := session(t, ts.URL, "b.token")

	t.Run("lists the seven tools", func(t *testing.T) {
		res, err := a.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("listing tools: %v", err)
		}
		names := []string{}
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		slices.Sort(names)
		want := []string{"add_to_collection", "get_collection", "get_reel", "list_collections", "list_reels", "save_link", "search_reels"}
		if !slices.Equal(names, want) {
			t.Errorf("tools = %v, want %v", names, want)
		}
	})

	t.Run("save_link starts one job per attempt", func(t *testing.T) {
		args := map[string]any{
			"url":             "https://www.instagram.com/reel/MCPTEST1/",
			"idempotency_key": "9e0d4f3a-1111-4222-8333-444455556666",
		}
		first := result(t, call(t, a, "save_link", args))
		if first["status"] != "processing" {
			t.Fatalf("status = %v, want processing", first["status"])
		}
		job, _ := first["job"].(map[string]any)
		if id, _ := job["id"].(string); id == "" {
			t.Fatalf("job = %v, want an id", first["job"])
		}

		again := result(t, call(t, a, "save_link", args))
		replayed, _ := again["job"].(map[string]any)
		if replayed["id"] != job["id"] {
			t.Errorf("a retry with the same key answered job %v, want %v", replayed["id"], job["id"])
		}
	})

	t.Run("list_reels answers the seeded save", func(t *testing.T) {
		reels := list(t, result(t, call(t, a, "list_reels", map[string]any{}))["reels"])
		if len(reels) != 1 || reels[0]["id"] != saveID {
			t.Fatalf("reels = %v, want the seeded save", reels)
		}
		if reels[0]["web_url"] != "https://reelpin.in/r/"+saveID {
			t.Errorf("web_url = %v", reels[0]["web_url"])
		}
		if reels[0]["category"] != "food_and_drink" || reels[0]["sub_category"] != "cafes" {
			t.Errorf("category = %v, sub_category = %v", reels[0]["category"], reels[0]["sub_category"])
		}
		otherReels := list(t, result(t, call(t, b, "list_reels", map[string]any{}))["reels"])
		if len(otherReels) != 1 || otherReels[0]["id"] != otherSaveID {
			t.Errorf("another user's reels = %v, want only their save", otherReels)
		}
	})

	t.Run("get_reel answers the owner only", func(t *testing.T) {
		out := result(t, call(t, a, "get_reel", map[string]any{"reel_id": saveID}))
		if out["title"] != "Best cafes in Goa" {
			t.Errorf("title = %v", out["title"])
		}

		res := call(t, b, "get_reel", map[string]any{"reel_id": saveID})
		if !res.IsError || text(t, res) != "reel not found" {
			t.Errorf("another user's get_reel = %v (%v), want the tool error reel not found", res.StructuredContent, res.Content)
		}
	})

	t.Run("search_reels finds the save by its words", func(t *testing.T) {
		results := list(t, result(t, call(t, a, "search_reels", map[string]any{"query": "cafes"}))["results"])
		if len(results) != 1 {
			t.Fatalf("results = %v, want one", results)
		}
		reel, _ := results[0]["reel"].(map[string]any)
		if reel["id"] != saveID {
			t.Errorf("results[0].reel.id = %v, want %s", reel["id"], saveID)
		}
		otherResults := list(t, result(t, call(t, b, "search_reels", map[string]any{"query": "cafes"}))["results"])
		if len(otherResults) != 1 {
			t.Fatalf("another user's results = %v, want one", otherResults)
		}
		otherReel, _ := otherResults[0]["reel"].(map[string]any)
		if otherReel["id"] != otherSaveID {
			t.Errorf("another user's results[0].reel.id = %v, want %s", otherReel["id"], otherSaveID)
		}
	})

	t.Run("collections file the save", func(t *testing.T) {
		if collections := list(t, result(t, call(t, a, "list_collections", map[string]any{}))["collections"]); len(collections) != 0 {
			t.Fatalf("collections = %v, want none yet", collections)
		}

		created, _, err := postgres.NewCollections(pool, "https://reelpin.in", time.Now).Create(context.Background(), userA, "Goa", "", nil)
		if err != nil {
			t.Fatalf("creating a collection: %v", err)
		}
		if collections := list(t, result(t, call(t, a, "list_collections", map[string]any{}))["collections"]); len(collections) != 1 {
			t.Fatalf("collections = %v, want the new one", collections)
		}

		change := result(t, call(t, a, "add_to_collection",
			map[string]any{"collection_id": created.ID, "reel_ids": []string{saveID}}))
		if change["added_count"] != float64(1) {
			t.Errorf("added_count = %v, want 1", change["added_count"])
		}

		page := result(t, call(t, a, "get_collection", map[string]any{"collection_id": created.ID}))
		items := list(t, page["items"])
		if len(items) != 1 || items[0]["id"] != saveID {
			t.Errorf("items = %v, want the seeded save", items)
		}
		if page["can_edit"] != true {
			t.Errorf("can_edit = %v, want true for the owner", page["can_edit"])
		}
	})

	t.Run("a client without a token is sent to the metadata", func(t *testing.T) {
		if s, err := connect(ts.URL, ""); err == nil {
			s.Close()
			t.Fatal("connected without a token")
		} else if !strings.Contains(err.Error(), "Unauthorized") {
			t.Errorf("error = %v, want it to say Unauthorized", err)
		}

		refused, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("posting without a token: %v", err)
		}
		refused.Body.Close()
		const challenge = `Bearer resource_metadata="https://api.test/.well-known/oauth-protected-resource/mcp"`
		if got := refused.Header.Get("WWW-Authenticate"); got != challenge {
			t.Fatalf("WWW-Authenticate = %q, want %q", got, challenge)
		}

		resp, err := http.Get(ts.URL + "/.well-known/oauth-protected-resource/mcp")
		if err != nil {
			t.Fatalf("fetching the metadata: %v", err)
		}
		defer resp.Body.Close()
		var metadata struct {
			Resource string `json:"resource"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
			t.Fatalf("decoding the metadata: %v", err)
		}
		if metadata.Resource != "https://api.test/mcp" {
			t.Errorf("resource = %q, want https://api.test/mcp", metadata.Resource)
		}
	})
}
