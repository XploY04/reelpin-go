package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const localDatabaseURL = "postgres://reelpin:reelpin@localhost:5432/reelpin"

type Config struct {
	Environment         string
	Port                int
	DatabaseURL         string
	Version             string
	SupabaseURL         string
	SupabaseJWTAudience string

	// RedisURL is required in production: rate limits fail closed without it,
	// which would refuse every submission. Outside production it may be empty,
	// and everything Redis-backed is simply off.
	RedisURL       string
	RedisKeyPrefix string
	// RateLimitSalt keys the hash that keeps user ids and IPs out of Redis keys
	// and logs. Rotating it resets every rate window, which is acceptable:
	// rate state is disposable. In development an absent salt is generated per
	// process for the same reason.
	RateLimitSalt string

	// RabbitMQURL is the broker the worker consumes and the outbox publishes
	// to. Required in production for the worker; the API never touches it.
	RabbitMQURL string
	// WorkerID names this process in leases and consumer tags.
	WorkerID string
	// GeminiAPIKey is the worker's model credential. Empty means every
	// provider call fails as unconfigured, which the pipeline surfaces as a
	// retryable provider error rather than a crash.
	GeminiAPIKey string
}

// RedisOptions parses RedisURL and applies the service's timeouts. One place,
// so the API and the worker cannot drift apart on how long they wait.
func (c Config) RedisOptions() (*redis.Options, error) {
	options, err := redis.ParseURL(c.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("REDIS_URL: %w", err)
	}
	options.DialTimeout = 5 * time.Second
	options.ReadTimeout = 3 * time.Second
	options.WriteTimeout = 3 * time.Second
	return options, nil
}

var validEnvironments = map[string]bool{
	"development": true,
	"test":        true,
	"production":  true,
}

func Load() (Config, error) {
	cfg := Config{
		Environment: envOr("ENVIRONMENT", "development"),
		Port:        8000,
		DatabaseURL: os.Getenv("DATABASE_URL"),
		Version:     envOr("APP_VERSION", "dev"),

		SupabaseURL:         strings.TrimSpace(os.Getenv("SUPABASE_URL")),
		RabbitMQURL:         strings.TrimSpace(os.Getenv("RABBITMQ_URL")),
		WorkerID:            envOr("WORKER_ID", defaultWorkerID()),
		SupabaseJWTAudience: envOr("SUPABASE_JWT_AUDIENCE", "authenticated"),

		RedisURL:       strings.TrimSpace(os.Getenv("REDIS_URL")),
		RedisKeyPrefix: envOr("REDIS_KEY_PREFIX", "reelpin"),
		RateLimitSalt:  strings.TrimSpace(os.Getenv("RATE_LIMIT_SALT")),

		GeminiAPIKey: strings.TrimSpace(os.Getenv("GEMINI_API_KEY")),
	}

	if !validEnvironments[cfg.Environment] {
		return Config{}, fmt.Errorf("ENVIRONMENT %q is not one of development, test, production", cfg.Environment)
	}

	if raw := os.Getenv("PORT"); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("PORT %q is not a number", raw)
		}
		if port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("PORT %d is outside 1..65535", port)
		}
		cfg.Port = port
	}

	if cfg.DatabaseURL == "" {
		if cfg.Environment == "production" {
			return Config{}, fmt.Errorf("DATABASE_URL is required in production")
		}
		cfg.DatabaseURL = localDatabaseURL
	}

	// Tests inject a fake authenticator, so only a running service needs Supabase.
	if cfg.SupabaseURL == "" && cfg.Environment != "test" {
		return Config{}, fmt.Errorf("SUPABASE_URL is required in %s", cfg.Environment)
	}

	if cfg.RedisURL == "" && cfg.Environment == "production" {
		return Config{}, fmt.Errorf("REDIS_URL is required in production: submissions fail closed without rate limits")
	}
	if cfg.RedisURL != "" {
		if _, err := redis.ParseURL(cfg.RedisURL); err != nil {
			return Config{}, fmt.Errorf("REDIS_URL: %w", err)
		}
	}

	if cfg.RateLimitSalt == "" {
		if cfg.Environment == "production" {
			return Config{}, fmt.Errorf("RATE_LIMIT_SALT is required in production: it keeps user ids and IPs out of Redis keys and logs")
		}
		// A per-process salt outside production: windows reset on restart,
		// which disposable rate state is allowed to do.
		generated := make([]byte, 16)
		if _, err := rand.Read(generated); err != nil {
			return Config{}, fmt.Errorf("generating a development salt: %w", err)
		}
		cfg.RateLimitSalt = hex.EncodeToString(generated)
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// defaultWorkerID is stable for the process and distinct across hosts, so two
// dev workers do not claim leases under one name.
func defaultWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}
