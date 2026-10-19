package db

import (
	"strings"
	"testing"
	"time"
)

func TestThePoolLetsAnIdleDatabaseSuspend(t *testing.T) {
	config, err := Config("postgres://reelpin@localhost:5432/reelpin?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if config.MinConns != 0 || config.MinIdleConns != 0 {
		t.Errorf("min conns = %d, min idle = %d: a held connection keeps the database awake",
			config.MinConns, config.MinIdleConns)
	}
	if config.MaxConnIdleTime >= 5*time.Minute {
		t.Errorf("idle connections live %s, want under the five-minute suspend window", config.MaxConnIdleTime)
	}
	if config.MaxConns != 4 {
		t.Errorf("max conns = %d, want 4", config.MaxConns)
	}
	if config.MaxConnLifetime != time.Hour || config.HealthCheckPeriod != time.Minute {
		t.Errorf("lifetime %s, health check %s", config.MaxConnLifetime, config.HealthCheckPeriod)
	}
	if config.ConnConfig.ConnectTimeout != 10*time.Second {
		t.Errorf("connect timeout = %s, want 10s for a cold start", config.ConnConfig.ConnectTimeout)
	}
}

func TestConnectTimeoutIsAddedOnlyWhenAbsent(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{"url", "postgres://u@host/db?sslmode=require", "connect_timeout=10"},
		{"postgresql url", "postgresql://u@host/db", "connect_timeout=10"},
		{"keyword", "host=localhost dbname=reelpin", "host=localhost dbname=reelpin connect_timeout=10"},
		{"already set", "postgres://u@host/db?connect_timeout=3", "connect_timeout=3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := withConnectTimeout(test.dsn)
			if !strings.Contains(got, test.want) {
				t.Errorf("withConnectTimeout(%q) = %q, want it to contain %q", test.dsn, got, test.want)
			}
			if strings.Count(got, "connect_timeout") != 1 {
				t.Errorf("withConnectTimeout(%q) = %q, want exactly one connect_timeout", test.dsn, got)
			}
		})
	}

	if got := withConnectTimeout("postgres://u@host/db?sslmode=require"); !strings.Contains(got, "sslmode=require") {
		t.Errorf("an existing parameter was lost: %q", got)
	}
}
