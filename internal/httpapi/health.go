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
	err := s.deps.DB.Ping(ctx)
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

// dependencyCheck runs one ping with a bounded timeout. It never calls a paid
// provider: readiness is about the infrastructure this process owns.
func (s *Server) dependencyCheck(ctx context.Context, checkedAt, name string, ping func(context.Context) error) ServiceHealthCheck {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	start := time.Now()
	err := ping(ctx)
	latency := float64(time.Since(start).Microseconds()) / 1000

	check := ServiceHealthCheck{
		Latency:   &latency,
		CheckedAt: checkedAt,
		Details:   map[string]any{},
	}
	if err != nil {
		// The driver error can carry credentials, so it never reaches the response.
		check.Status = "degraded"
		check.Message = name + " is not reachable."
		return check
	}
	check.Healthy = true
	check.Status = "ok"
	check.Message = name + " responded."
	return check
}

// workerCheck reports the fleet from heartbeats. No live worker means queued
// work is not moving, which is a readiness problem for the system even though
// the API itself is fine, so it is reported without failing the API's own
// readiness.
func (s *Server) workerCheck(ctx context.Context, checkedAt string) ServiceHealthCheck {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	count, err := s.deps.Workers.LiveWorkers(ctx)
	check := ServiceHealthCheck{CheckedAt: checkedAt, Details: map[string]any{}}
	if err != nil {
		check.Status = "unknown"
		check.Message = "Worker heartbeats could not be read."
		return check
	}
	check.Details["live_workers"] = count
	check.Healthy = count > 0
	if count == 0 {
		check.Status = "degraded"
		check.Message = "No worker has sent a heartbeat."
		return check
	}
	check.Status = "ok"
	check.Message = "Workers are reporting."
	return check
}

func (s *Server) readiness(ctx context.Context) HealthResponse {
	checkedAt := nowUTC()
	db := s.databaseCheck(ctx, checkedAt)
	checks := map[string]ServiceHealthCheck{
		"api": apiCheck(checkedAt),
		// legacy key: Python called Supabase, Go talks to the same Postgres directly.
		"supabase": db,
	}

	// Redis and RabbitMQ are only required where they are configured. A
	// development process without them is ready, and says so honestly.
	ready := db.Healthy
	if s.deps.Redis != nil {
		check := s.dependencyCheck(ctx, checkedAt, "Redis", s.deps.Redis.Ping)
		checks["redis"] = check
		ready = ready && check.Healthy
	}
	if s.deps.Queue != nil {
		check := s.dependencyCheck(ctx, checkedAt, "RabbitMQ", s.deps.Queue.Ping)
		checks["rabbitmq"] = check
		ready = ready && check.Healthy
	}
	if s.deps.Workers != nil {
		checks["workers"] = s.workerCheck(ctx, checkedAt)
	}

	resp := HealthResponse{
		Status:    "ok",
		Ready:     ready,
		Version:   s.deps.Version,
		Service:   serviceName,
		CheckedAt: checkedAt,
		Checks:    checks,
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
		Version:   s.deps.Version,
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
