package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const localDatabaseURL = "postgres://reelpin:reelpin@localhost:5432/reelpin"

type Config struct {
	Environment         string
	Port                int
	DatabaseURL         string
	Version             string
	SupabaseURL         string
	SupabaseJWTAudience string
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
		SupabaseJWTAudience: envOr("SUPABASE_JWT_AUDIENCE", "authenticated"),
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

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
