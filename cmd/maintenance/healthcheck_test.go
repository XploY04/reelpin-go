package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTheDefaultHealthcheckFollowsTheConfiguredPort(t *testing.T) {
	t.Setenv("HEALTHCHECK_URL", "")
	t.Setenv("PORT", "")
	if got := defaultHealthcheckURL(); got != "http://127.0.0.1:8000/api/v1/health/live" {
		t.Errorf("default = %q", got)
	}

	// A service moved off 8000 must not be reported unhealthy by its own check.
	t.Setenv("PORT", "9000")
	if got := defaultHealthcheckURL(); !strings.Contains(got, ":9000/") {
		t.Errorf("with PORT=9000 the check calls %q", got)
	}

	// The worker serves liveness somewhere else entirely, so it overrides.
	t.Setenv("HEALTHCHECK_URL", "http://127.0.0.1:9100/health/live")
	if got := defaultHealthcheckURL(); got != "http://127.0.0.1:9100/health/live" {
		t.Errorf("the override was ignored: %q", got)
	}
}

func TestHealthcheckPassesOnlyOn200(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	defer server.Close()

	args := []string{"--url", server.URL + "/api/v1/health/live"}
	if err := runHealthcheck(context.Background(), args); err != nil {
		t.Fatalf("a 200 failed the check: %v", err)
	}

	status = http.StatusServiceUnavailable
	if err := runHealthcheck(context.Background(), args); err == nil {
		t.Fatal("a 503 passed the check")
	}
}

func TestHealthcheckFailsWhenNothingIsListening(t *testing.T) {
	if err := runHealthcheck(context.Background(), []string{"--url", "http://127.0.0.1:1/x"}); err == nil {
		t.Fatal("a dead process passed its own health check")
	}
}
