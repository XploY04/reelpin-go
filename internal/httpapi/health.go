package httpapi

import (
	"context"
	"net/http"
	"time"
)

const serviceName = "ReelMind API"

type ServiceHealthCheck struct {
	Healthy   bool           `json:"healthy"`
	Status    string         `json:"status"`
	Latency   *float64       `json:"latency_ms,omitempty"`
	Message   string         `json:"message,omitempty"`
	CheckedAt string         `json:"checked_at,omitempty"`
	Details   map[string]any `json:"details"`
}

type HealthResponse struct {
	Status    string                        `json:"status"`
	Ready     bool                          `json:"ready"`
	Version   string                        `json:"version"`
	Service   string                        `json:"service"`
	CheckedAt string                        `json:"checked_at"`
	Checks    map[string]ServiceHealthCheck `json:"checks"`
}

func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func apiCheck(checkedAt string) ServiceHealthCheck {
	return ServiceHealthCheck{
		Healthy:   true,
		Status:    "ok",
		Message:   "API process is reachable.",
		CheckedAt: checkedAt,
		Details:   map[string]any{},
	}
}

func (s *Server) databaseCheck(ctx context.Context, checkedAt string) ServiceHealthCheck {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	start := time.Now()
	err := s.db.Ping(ctx)
	latency := float64(time.Since(start).Microseconds()) / 1000

	check := ServiceHealthCheck{
		Latency:   &latency,
		CheckedAt: checkedAt,
		Details:   map[string]any{},
	}
	if err != nil {
		// ponytail: the driver error can carry the DSN, so it never reaches the response.
		check.Healthy = false
		check.Status = "degraded"
		check.Message = "Database is not reachable."
		return check
	}
	check.Healthy = true
	check.Status = "ok"
	check.Message = "Database responded to ping."
	return check
}

func (s *Server) readiness(ctx context.Context) HealthResponse {
	checkedAt := nowUTC()
	db := s.databaseCheck(ctx, checkedAt)
	resp := HealthResponse{
		Status:    "ok",
		Ready:     db.Healthy,
		Version:   s.version,
		Service:   serviceName,
		CheckedAt: checkedAt,
		Checks: map[string]ServiceHealthCheck{
			"api": apiCheck(checkedAt),
			// legacy key: Python called Supabase, Go talks to the same Postgres directly.
			"supabase": db,
		},
	}
	if !resp.Ready {
		resp.Status = "degraded"
	}
	return resp
}

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	checkedAt := nowUTC()
	writeJSON(w, http.StatusOK, HealthResponse{
		Status:    "ok",
		Ready:     true,
		Version:   s.version,
		Service:   serviceName,
		CheckedAt: checkedAt,
		Checks:    map[string]ServiceHealthCheck{"api": apiCheck(checkedAt)},
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	resp := s.readiness(r.Context())
	status := http.StatusOK
	if !resp.Ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}

// handleHealth keeps the old contract: always 200, even when degraded.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.readiness(r.Context()))
}
