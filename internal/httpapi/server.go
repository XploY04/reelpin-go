package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/auth"
	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/metrics"
	"github.com/XploY04/reelpin-go/internal/reels"
)

// Pinger is one bounded liveness check against a dependency this process owns.
// It is all the health endpoints need from the pool or from Redis, and it
// never reaches a paid provider.
type Pinger interface {
	Ping(ctx context.Context) error
}

// WorkerCounter reports how many workers are currently heartbeating.
type WorkerCounter interface {
	LiveWorkers(ctx context.Context) (int, error)
}

// Deps is everything the API needs from the outside world.
type Deps struct {
	DB    Pinger
	Auth  auth.Authenticator
	Reels reels.ReelReader
	Jobs  jobs.JobReader
	// Enqueue is the submission use case; ShareTokens authenticates and
	// manages the native extensions' credential.
	Enqueue     Submitter
	ShareTokens ShareTokenStore
	// Resolver previews shared text without processing it.
	Resolver ShareResolver
	// Collections owns a user's arrangement of their own saves.
	Collections Collections
	// Notifications owns device tokens and open receipts. Campaigns are an
	// operator command, not part of this surface.
	Notifications Notifications
	// Lifecycle owns deletion: one save, or a whole account.
	Lifecycle Lifecycle
	// Map answers what is on the user's map.
	Map MapView
	// Search is hybrid retrieval over one user's saves.
	Search Searcher
	// Limiter is nil outside production-shaped setups. Provider-costing
	// endpoints fail closed without a decision; reads never consult it.
	Limiter RateLimiter
	// Metrics is optional: nil means this process exports none and /metrics is
	// not served at all.
	Metrics *metrics.Metrics
	// AdminKey guards /metrics. Empty means the endpoint is not mounted.
	AdminKey string
	// IPBucketSecret verifies the bucket the web boundary forwards. Empty means
	// per-IP limits use the socket peer, which is the behaviour without a web.
	IPBucketSecret string
	// Redis and Workers are readiness inputs. Each is optional: a process
	// without one is ready without it.
	Redis   Pinger
	Workers WorkerCounter
	Logger  *slog.Logger
	Version string
	Now     func() time.Time
}

type Server struct {
	deps Deps
}

func New(deps Deps) (*Server, error) {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	server := &Server{deps: deps}
	if err := checkDependencies(deps, server.routeTable()); err != nil {
		return nil, err
	}
	return server, nil
}

// checkDependencies refuses to build a server whose routes are registered
// against something nil. Such a route panics on its first request, the
// recovery middleware turns that into a 500, and nothing else notices: the
// process is healthy, the contract check passes, and every client sees
// internal_error until someone looks. Startup is the only place it can be
// caught, so it is caught here and the process does not start.
//
// The names are resolved against Deps by reflection rather than against a list
// kept beside it, so the route table stays the only thing to maintain: a
// dependency that is renamed, removed, or misspelled in a route fails here too.
func checkDependencies(deps Deps, table []Route) error {
	fields := reflect.ValueOf(deps)

	unset := map[string][]string{}
	unknown := map[string][]string{}
	undeclared := []string{}

	for _, route := range table {
		label := route.Method + " " + route.Path
		if len(route.requires) == 0 {
			undeclared = append(undeclared, label)
			continue
		}
		for _, name := range route.requires {
			field := fields.FieldByName(name)
			switch {
			case !field.IsValid():
				unknown[name] = append(unknown[name], label)
			case !supplied(field):
				unset[name] = append(unset[name], label)
			}
		}
	}

	problems := append(describe(unset, "is nil"), describe(unknown, "is not a field of Deps")...)
	if len(undeclared) > 0 {
		problems = append(problems,
			"these routes declare no dependency at all: "+strings.Join(undeclared, ", "))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("the route table is registered against dependencies that cannot serve it:\n  %s",
		strings.Join(problems, "\n  "))
}

// supplied reports whether a dependency was actually handed over. Only the
// kinds that can be nil are judged: an empty string is a legitimate value for
// the ones that are optional by design.
func supplied(field reflect.Value) bool {
	switch field.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !field.IsNil()
	}
	return true
}

func describe(byName map[string][]string, problem string) []string {
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, fmt.Sprintf("%s %s; needed by %s",
			name, problem, strings.Join(byName[name], ", ")))
	}
	return lines
}

func (s *Server) now() time.Time { return s.deps.Now().UTC() }

// Routes registers the table and nothing else. A path that exists with other
// methods answers a JSON 405 naming them; anything unknown answers a JSON 404.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	methodsByPath := map[string][]string{}
	for _, route := range s.routeTable() {
		methodsByPath[route.Path] = append(methodsByPath[route.Path], route.Method)
	}

	claimed := map[string]bool{}
	for _, route := range s.routeTable() {
		mux.HandleFunc(route.Method+" "+route.Path, route.handler)
		if route.claimMethods && !claimed[route.Path] {
			claimed[route.Path] = true
			mux.HandleFunc(route.Path, s.methodNotAllowed(methodsByPath[route.Path]))
		}
	}

	// Prometheus scrapes this. It is deliberately outside the route table and
	// the contract: it answers the exposition format, not JSON, and no
	// generated client should ever call it.
	if s.deps.Metrics != nil && s.deps.AdminKey != "" {
		mux.Handle("GET /metrics", s.deps.Metrics.GuardedHandler(s.deps.AdminKey))
	}

	mux.HandleFunc("/", notFound)

	return s.requestID(s.observe(s.logRequest(s.recoverPanic(jsonContentType(mux)))))
}

func (s *Server) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			writeError(w, http.StatusUnauthorized, errorBody{
				Code:    "authentication_required",
				Message: "Sign in is required.",
			})
			return
		}

		userID, err := s.deps.Auth.Authenticate(r.Context(), token)
		if err != nil {
			// The verification error stays in the log; the client learns nothing.
			s.deps.Logger.Info("token rejected",
				"route", routeLabel(r.Pattern),
				"request_id", w.Header().Get("X-Request-ID"),
				"error", err)
			writeError(w, http.StatusUnauthorized, errorBody{
				Code:    "invalid_auth_token",
				Message: "Your sign-in session is invalid or expired.",
			})
			return
		}

		// The log middleware runs outside this one and cannot see a context
		// value set here, so the hashed user is handed back through a holder.
		if holder, ok := r.Context().Value(logSubjectKey).(*logSubject); ok {
			holder.hashedUser = metrics.Hash(userID)
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

// errorBody is the one error envelope every v2 response uses. Details are
// optional and carry only values the caller supplied or may choose from; a
// driver message never reaches it.
type errorBody struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

// writeError fills in the request id from the response headers, so no handler
// has to remember to correlate its own errors.
func writeError(w http.ResponseWriter, status int, body errorBody) {
	body.RequestID = w.Header().Get("X-Request-ID")
	writeJSON(w, status, errorResponse{Error: body})
}

var internalErrorBody = errorResponse{Error: errorBody{
	Code:      "internal_error",
	Message:   "The server could not finish this request.",
	Retryable: true,
}}

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
	writeError(w, http.StatusNotFound, errorBody{
		Code:    "not_found",
		Message: "This endpoint does not exist.",
	})
}

// methodNotAllowed answers for a path that exists with methods that do not.
// The allowed set comes from the route table, so it cannot drift.
func (s *Server) methodNotAllowed(allowed []string) http.HandlerFunc {
	sort.Strings(allowed)
	header := strings.Join(allowed, ", ")
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", header)
		writeError(w, http.StatusMethodNotAllowed, errorBody{
			Code:    "method_not_allowed",
			Message: "This endpoint does not accept that method.",
			Details: map[string]any{"allowed": allowed},
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

// logSubject carries the hashed user back out to the log middleware. One
// request is handled by one goroutine, so it needs no lock.
type logSubject struct{ hashedUser string }

type logSubjectKeyType struct{}

var logSubjectKey logSubjectKeyType

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), logSubjectKey, &logSubject{})))
	})
}

// observe records the request against the registry. It sits inside the
// request-id middleware and outside everything else, so it sees the status any
// handler or middleware wrote.
func (s *Server) observe(next http.Handler) http.Handler {
	if s.deps.Metrics == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.deps.Metrics.ObserveRequest(routeLabel(r.Pattern), r.Method, rec.status, time.Since(start))
	})
}

// routeLabel keeps a label to the shape of the route, not the request. The mux
// fills in r.Pattern during routing; the raw path carries reel and job ids and
// would grow a time series per id.
func routeLabel(pattern string) string {
	if pattern == "" {
		return "unmatched"
	}
	if _, path, found := strings.Cut(pattern, " "); found {
		return path
	}
	return pattern
}

func (s *Server) logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// The user is hashed and the path is the route pattern: a log line is
		// for correlating, never for identifying.
		fields := []any{
			"method", r.Method,
			"route", routeLabel(r.Pattern),
			"status", rec.status,
			"duration_ms", float64(time.Since(start).Microseconds()) / 1000,
			"request_id", w.Header().Get("X-Request-ID"),
		}
		if holder, ok := r.Context().Value(logSubjectKey).(*logSubject); ok && holder.hashedUser != "" {
			fields = append(fields, "user", holder.hashedUser)
		}
		s.deps.Logger.Info("request", fields...)
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
