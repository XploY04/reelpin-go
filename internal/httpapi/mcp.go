package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/XploY04/reelpin-go/internal/auth"
	"github.com/XploY04/reelpin-go/internal/metrics"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

const mcpInstructions = `ReelPin is where this user saves reels, videos, posts and links to find later. Every tool works on the signed-in user's own library. Use list_reels to browse, search_reels to find reels by meaning or words, get_reel for one reel in full, save_link to save a new link, and list_collections, get_collection and add_to_collection for the user's collections. Every reel carries web_url, the page to show the user.`

type mcpServer struct {
	handler  http.Handler
	metadata http.Handler
	origins  *http.CrossOriginProtection
	// metadataURL is what the 401 challenge points a client at.
	metadataURL string
}

func newMCPServer(s *Server) *mcpServer {
	base := strings.TrimSuffix(s.deps.PublicAPIBaseURL, "/")
	resourceURL := base + "/mcp"
	metadataURL := base + "/.well-known/oauth-protected-resource/mcp"

	// No scopes_supported: Supabase's scopes only govern OpenID claims, which no tool needs.
	document := mcpauth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               resourceURL,
		AuthorizationServers:   []string{s.deps.OAuthIssuer},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "ReelPin",
	})
	// The SDK handler answers only GET, and HEAD is the same read without a body.
	metadata := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			r = r.Clone(r.Context())
			r.Method = http.MethodGet
		}
		document.ServeHTTP(w, r)
	})

	// The transport spec requires refusing a browser request from another site.
	// The bearer token already stops CSRF; this answers it before any token work.
	origins := http.NewCrossOriginProtection()
	origins.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, errorBody{
			Code:    "cross_origin_forbidden",
			Message: "This endpoint does not accept requests from another site.",
		})
	}))

	// No Logger: the SDK logs every request at Info, which the request log already does.
	server := mcp.NewServer(&mcp.Implementation{
		Name:       "reelpin",
		Title:      "ReelPin",
		Version:    s.deps.Version,
		WebsiteURL: s.deps.PublicWebBaseURL,
	}, &mcp.ServerOptions{Instructions: mcpInstructions})
	s.addMCPTools(server)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		// The 2026-07-28 revision has no sessions, and a process behind nginx
		// must not need one.
		Stateless: true,
		// Every tool answers in one shot, and a plain JSON body passes nginx and
		// the server's 15 s write timeout the way every v2 response does.
		JSONResponse: true,
		// A client that goes away cancels the query.
		PropagateRequestCancellation: true,
		// nginx connects over loopback with the public Host header, which the
		// rebinding check would refuse with 403; the bearer token guards this.
		DisableLocalhostProtection: true,
		// The same bound every v2 JSON body has.
		MaxRequestBodyBytes: maxJSONBodyBytes,
	})

	return &mcpServer{handler: handler, metadata: metadata, origins: origins, metadataURL: metadataURL}
}

// mcpRequirements gives /mcp the route table's startup guard: a nil service fails New.
func (s *Server) mcpRequirements() []Route {
	return []Route{{
		Method: http.MethodPost, Path: "/mcp",
		requires: []string{"Auth", "Logger", "Reels", "Search", "Enqueue", "Collections"},
	}}
}

// mcpAuthenticated is authenticated plus the RFC 9728 challenge MCP clients discover OAuth by.
func (s *Server) mcpAuthenticated(next http.Handler) http.Handler {
	challenge := `Bearer resource_metadata="` + s.mcp.metadataURL + `"`
	invalidChallenge := `Bearer error="invalid_token", resource_metadata="` + s.mcp.metadataURL + `"`

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			w.Header().Set("WWW-Authenticate", challenge)
			writeError(w, http.StatusUnauthorized, errorBody{
				Code:    "authentication_required",
				Message: "Sign in is required.",
			})
			return
		}

		principal, err := s.deps.Auth.Authenticate(r.Context(), token)
		if err == nil && principal.ClientID == "" {
			// Only a token minted by Supabase's OAuth 2.1 server for a registered
			// client names its client_id; an app session token does not, and the
			// MCP spec requires this endpoint to accept only tokens issued for it.
			// It also means every MCP call lands in a per-client rate-limit bucket.
			err = errors.New("token was not minted by the OAuth server")
		}
		if err != nil {
			// The verification error stays in the log; the client learns nothing.
			s.deps.Logger.Info("token rejected",
				"route", routeLabel(r.Pattern),
				"request_id", w.Header().Get("X-Request-ID"),
				"error", err)
			w.Header().Set("WWW-Authenticate", invalidChallenge)
			writeError(w, http.StatusUnauthorized, errorBody{
				Code:    "invalid_auth_token",
				Message: "Your sign-in session is invalid or expired.",
			})
			return
		}

		// The log middleware runs outside this one and cannot see a context
		// value set here, so the hashed user is handed back through a holder.
		if holder, ok := r.Context().Value(logSubjectKey).(*logSubject); ok {
			holder.hashedUser = metrics.Hash(principal.UserID)
		}
		if s.deps.Migrations != nil {
			s.deps.Migrations.Seen(r.Context(), principal.UserID)
		}
		ctx := auth.WithClientID(auth.WithUserID(r.Context(), principal.UserID), principal.ClientID)
		ctx = withMCPCall(ctx, mcpCall{
			limitSubject: s.limitSubject(r),
			requestID:    w.Header().Get("X-Request-ID"),
		})
		// No invalidatingWrites: every call is a POST, so the two write tools invalidate instead.
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// mcpCall is the request state a tool handler needs but is never handed.
type mcpCall struct {
	limitSubject string
	requestID    string
}

type mcpCallKeyType struct{}

var mcpCallKey mcpCallKeyType

func withMCPCall(ctx context.Context, call mcpCall) context.Context {
	return context.WithValue(ctx, mcpCallKey, call)
}

func mcpCallFromContext(ctx context.Context) mcpCall {
	call, _ := ctx.Value(mcpCallKey).(mcpCall)
	return call
}
