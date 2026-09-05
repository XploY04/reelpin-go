package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/auth"
	"github.com/XploY04/reelpin-go/internal/cache"
	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/ratelimit"
	"github.com/XploY04/reelpin-go/internal/reels"
)

// RateLimiter is the slice of the limiter the API needs.
type RateLimiter interface {
	Allow(ctx context.Context, policy ratelimit.Policy, subject string) (ratelimit.Decision, error)
}

// DatabasePinger is the only thing the health endpoints need from the pool.
type DatabasePinger interface {
	Ping(context.Context) error
}

// Deps is everything the API needs from the outside world.
type Deps struct {
	DB          DatabasePinger
	Auth        auth.Authenticator
	Reels       reels.ReelReader
	Jobs        jobs.JobReader
	Share       ShareResolver
	Enqueue     Enqueuer
	ShareTokens ShareTokens
	Limiter     RateLimiter
	Cache       *cache.Cache
	// TrustedProxies are the only sources whose forwarding headers are believed.
	TrustedProxies []netip.Prefix
	Logger         *slog.Logger
	Version        string
	Now            func() time.Time
}

type Server struct {
	deps Deps
}

func New(deps Deps) *Server {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Server{deps: deps}
}

func (s *Server) now() time.Time { return s.deps.Now().UTC() }

// Route is one endpoint this server serves. The table is the single source of
// truth for both registration and the contract manifest.
type Route struct {
	Method        string `json:"method"`
	Path          string `json:"path"`
	Alias         string `json:"alias,omitempty"`
	Authenticated bool   `json:"authenticated"`

	handler http.HandlerFunc
	limit   routeLimit
	// claimMethods registers the path again without a method, so a wrong method
	// gets a JSON 405 instead of the mux's plain text. It is off for a literal
	// path already covered by a sibling wildcard, which would conflict.
	claimMethods bool
}

func (s *Server) routeTable() []Route {
	guarded := func(method, path string, handler http.HandlerFunc, limit routeLimit, claimMethods bool) Route {
		return Route{
			Method:        method,
			Path:          "/api/v1" + path,
			Alias:         path,
			Authenticated: true,
			handler:       s.authenticated(s.rateLimited(limit, handler)),
			claimMethods:  claimMethods,
			limit:         limit,
		}
	}
	read := func(path string, handler http.HandlerFunc, claimMethods bool) Route {
		return Route{
			Method:        http.MethodGet,
			Path:          "/api/v1" + path,
			Alias:         path,
			Authenticated: true,
			handler:       s.authenticated(handler),
			claimMethods:  claimMethods,
		}
	}

	return []Route{
		{Method: http.MethodGet, Path: "/api/v1/health/live", handler: s.handleLive, claimMethods: true},
		{Method: http.MethodGet, Path: "/api/v1/health/ready", handler: s.handleReady, claimMethods: true},
		{Method: http.MethodGet, Path: "/api/v1/health", handler: s.handleHealth, claimMethods: true},

		read("/reels", s.handleListReels, true),
		read("/reels/filters", s.handlePlatformFilters, false),
		read("/reels/category-filters", s.handleCategoryFilters, false),
		read("/reels/{reel_id}", s.handleGetReel, true),
		read("/processing-jobs", s.handleListJobs, true),
		read("/processing-jobs/{job_id}", s.handleGetJob, true),
		read("/account/library-stats", s.handleLibraryStats, true),
		read("/account/entitlements", s.handleEntitlements, true),

		// Share resolution is a read that calls no provider, so its limit
		// fails open: a Redis outage must not stop people sharing.
		// The native share path may present a device token instead of a session,
		// and only here.
		{
			Method:        http.MethodPost,
			Path:          "/api/v1/processing-jobs/reels",
			Alias:         "/processing-jobs/reels",
			Authenticated: true,
			handler: s.shareTokenOrJWT(s.rateLimited(routeLimit{
				User: &ratelimit.Enqueue,
				IP:   &ratelimit.EnqueueIP,
			}, s.handleEnqueueReel)),
			limit: routeLimit{User: &ratelimit.Enqueue, IP: &ratelimit.EnqueueIP},
		},
		// The older alias the shipped app still calls for the same service.
		{
			Method:        http.MethodPost,
			Path:          "/api/v1/process-reel",
			Alias:         "/process-reel",
			Authenticated: true,
			handler: s.shareTokenOrJWT(s.rateLimited(routeLimit{
				User: &ratelimit.Enqueue,
				IP:   &ratelimit.EnqueueIP,
			}, s.handleEnqueueReel)),
			limit: routeLimit{User: &ratelimit.Enqueue, IP: &ratelimit.EnqueueIP},
		},

		guarded(http.MethodPost, "/share-tokens", s.handleMintShareToken, routeLimit{
			User: &ratelimit.ShareTokenMint,
			IP:   &ratelimit.ShareTokenMintIP,
		}, false),
		guarded(http.MethodDelete, "/share-tokens", s.handleRevokeShareTokens, routeLimit{
			User: &ratelimit.ShareTokenMint,
			IP:   &ratelimit.ShareTokenMintIP,
		}, false),

		guarded(http.MethodPost, "/share/resolve", s.handleResolveShare, routeLimit{
			User:     &ratelimit.ShareResolve,
			IP:       &ratelimit.ShareResolveIP,
			FailOpen: true,
		}, true),
	}
}

// RouteManifest is the registered surface, without handlers, for contract tests.
func (s *Server) RouteManifest() []Route {
	table := s.routeTable()
	manifest := make([]Route, 0, len(table))
	for _, route := range table {
		manifest = append(manifest, Route{
			Method:        route.Method,
			Path:          route.Path,
			Alias:         route.Alias,
			Authenticated: route.Authenticated,
		})
	}
	sort.Slice(manifest, func(i, j int) bool {
		if manifest[i].Path != manifest[j].Path {
			return manifest[i].Path < manifest[j].Path
		}
		return manifest[i].Method < manifest[j].Method
	})
	return manifest
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Every endpoint is served twice: the canonical path and the bare alias the
	// shipped app still calls.
	for _, route := range s.routeTable() {
		paths := []string{route.Path}
		if route.Alias != "" {
			paths = append(paths, route.Alias)
		}
		for _, path := range paths {
			mux.HandleFunc(route.Method+" "+path, route.handler)
			if route.claimMethods {
				mux.HandleFunc(path, methodNotAllowed(route.Method))
			}
		}
	}

	mux.HandleFunc("/", notFound)

	return s.requestID(s.logRequest(s.recoverPanic(jsonContentType(mux))))
}

func (s *Server) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			writeError(w, http.StatusUnauthorized, errorResponse{
				ErrorCode: "authentication_required",
				Message:   "Sign in is required.",
				Detail:    "Missing Authorization bearer token.",
			})
			return
		}

		userID, err := s.deps.Auth.Authenticate(r.Context(), token)
		if err != nil {
			// The verification error stays in the log; the client learns nothing.
			s.deps.Logger.Info("token rejected", "path", r.URL.Path, "error", err)
			writeError(w, http.StatusUnauthorized, errorResponse{
				ErrorCode: "invalid_auth_token",
				Message:   "Your sign-in session is invalid or expired.",
				Detail:    "The access token could not be verified.",
			})
			return
		}

		next(w, r.WithContext(auth.WithUserID(r.Context(), userID)))
	}
}

func bearerToken(header string) string {
	value := strings.TrimSpace(header)
	const prefix = "bearer "
	if len(value) <= len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(value[len(prefix):])
}

type errorResponse struct {
	Success   bool     `json:"success"`
	ErrorCode string   `json:"error_code"`
	Message   string   `json:"message"`
	Detail    string   `json:"detail"`
	Retryable bool     `json:"retryable"`
	Allowed   []string `json:"allowed,omitempty"`
}

func writeError(w http.ResponseWriter, status int, body errorResponse) {
	writeJSON(w, status, body)
}

var internalErrorBody = errorResponse{
	ErrorCode: "internal_error",
	Message:   "The server could not finish this request.",
	Detail:    "Unhandled server error",
	Retryable: true,
}

// writeJSON encodes before touching the status line, so a broken payload still
// leaves room for the standard 500.
func writeJSON(w http.ResponseWriter, status int, data any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(data); err != nil {
		buf.Reset()
		json.NewEncoder(&buf).Encode(internalErrorBody)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func notFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, errorResponse{
		ErrorCode: "not_found",
		Message:   "This endpoint does not exist.",
		Detail:    "Unknown route",
	})
}

func methodNotAllowed(allowed string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allowed)
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{
			ErrorCode: "method_not_allowed",
			Message:   "This endpoint does not accept that method.",
			Detail:    "Only " + allowed + " is allowed",
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// rand.Text panics rather than handing back a predictable id if entropy fails.
func newRequestID() string {
	return rand.Text()
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range []byte(id) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.deps.Logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", float64(time.Since(start).Microseconds())/1000,
			"request_id", w.Header().Get("X-Request-ID"),
		)
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.deps.Logger.Error("panic recovered",
					"panic", v,
					"path", r.URL.Path,
					"request_id", w.Header().Get("X-Request-ID"),
				)
				writeJSON(w, http.StatusInternalServerError, internalErrorBody)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}
