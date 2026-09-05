package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/auth"
	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/reels"
)

// DatabasePinger is the only thing the health endpoints need from the pool.
type DatabasePinger interface {
	Ping(context.Context) error
}

// Deps is everything the API needs from the outside world.
type Deps struct {
	DB      DatabasePinger
	Auth    auth.Authenticator
	Reels   reels.ReelReader
	Jobs    jobs.JobReader
	Logger  *slog.Logger
	Version string
	Now     func() time.Time
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

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	for path, handler := range map[string]http.HandlerFunc{
		"/api/v1/health/live":  s.handleLive,
		"/api/v1/health/ready": s.handleReady,
	} {
		mux.HandleFunc("GET "+path, handler)
		mux.HandleFunc(path, methodNotAllowed)
	}

	for path, handler := range map[string]http.HandlerFunc{
		"/api/v1/reels":                    s.handleListReels,
		"/api/v1/reels/filters":            s.handlePlatformFilters,
		"/api/v1/reels/category-filters":   s.handleCategoryFilters,
		"/api/v1/reels/{reel_id}":          s.handleGetReel,
		"/api/v1/processing-jobs":          s.handleListJobs,
		"/api/v1/processing-jobs/{job_id}": s.handleGetJob,
		"/api/v1/account/library-stats":    s.handleLibraryStats,
	} {
		guarded := s.authenticated(handler)
		mux.HandleFunc("GET "+path, guarded)
	}

	// The mux answers a wrong method with a plain-text 405, so the paths are
	// claimed again without a method to keep every body JSON. The two literal
	// children of /reels/{reel_id} are left out: that wildcard already covers
	// them, and claiming them again conflicts with it.
	for _, path := range []string{
		"/api/v1/reels",
		"/api/v1/reels/{reel_id}",
		"/api/v1/processing-jobs",
		"/api/v1/processing-jobs/{job_id}",
		"/api/v1/account/library-stats",
	} {
		mux.HandleFunc(path, methodNotAllowed)
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

func methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", http.MethodGet)
	writeJSON(w, http.StatusMethodNotAllowed, errorResponse{
		ErrorCode: "method_not_allowed",
		Message:   "This endpoint does not accept that method.",
		Detail:    "Only GET is allowed",
	})
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
