package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// DatabasePinger is the only thing the health endpoints need from the pool.
type DatabasePinger interface {
	Ping(context.Context) error
}

type Server struct {
	db      DatabasePinger
	logger  *slog.Logger
	version string
}

func New(db DatabasePinger, logger *slog.Logger, version string) *Server {
	return &Server{db: db, logger: logger, version: version}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health/live", s.handleLive)
	mux.HandleFunc("GET /api/v1/health/ready", s.handleReady)
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)

	// The mux answers a wrong method with a plain-text 405, so claim the paths again
	// without a method and keep every body JSON.
	for _, path := range []string{"/api/v1/health/live", "/api/v1/health/ready", "/api/v1/health"} {
		mux.HandleFunc(path, methodNotAllowed)
	}
	mux.HandleFunc("/", notFound)

	return s.requestID(s.logRequest(s.recoverPanic(jsonContentType(mux))))
}

type errorResponse struct {
	Success   bool   `json:"success"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
	Detail    string `json:"detail"`
	Retryable bool   `json:"retryable"`
}

var internalError = errorResponse{
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
		json.NewEncoder(&buf).Encode(internalError)
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
		s.logger.Info("request",
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
				s.logger.Error("panic recovered",
					"panic", v,
					"path", r.URL.Path,
					"request_id", w.Header().Get("X-Request-ID"),
				)
				writeJSON(w, http.StatusInternalServerError, internalError)
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
