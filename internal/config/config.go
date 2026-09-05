package config

import (
	"fmt"
	"net/netip"
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
	RedisURL            string
	RedisKeyPrefix      string
	// TrustedProxyCIDRs are the only sources whose forwarding headers are
	// believed. Empty means every client is identified by its socket address.
	TrustedProxyCIDRs []netip.Prefix
}

// RateLimitPrefix, CachePrefix and WorkerPrefix keep the three uses of Redis in
// separate key spaces, so one can be flushed without touching the others.
func (c Config) RateLimitPrefix() string { return c.RedisKeyPrefix + ":ratelimit" }
func (c Config) CachePrefix() string     { return c.RedisKeyPrefix + ":cache" }
func (c Config) WorkerPrefix() string    { return c.RedisKeyPrefix + ":worker" }

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
		RedisURL:            strings.TrimSpace(os.Getenv("REDIS_URL")),
		RedisKeyPrefix:      envOr("REDIS_KEY_PREFIX", "reelpin"),
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

	proxies, err := parseCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS"))
	if err != nil {
		return Config{}, err
	}
	cfg.TrustedProxyCIDRs = proxies

	// Rate limits and caches need Redis, and production must not run without
	// the limits.
	if cfg.RedisURL == "" && cfg.Environment == "production" {
		return Config{}, fmt.Errorf("REDIS_URL is required in production")
	}

	// Tests inject a fake authenticator, so only a running service needs Supabase.
	if cfg.SupabaseURL == "" && cfg.Environment != "test" {
		return Config{}, fmt.Errorf("SUPABASE_URL is required in %s", cfg.Environment)
	}

	return cfg, nil
}

// parseCIDRs reads the proxy allowlist. A malformed entry stops startup rather
// than silently trusting nothing, because that would misattribute every client.
func parseCIDRs(raw string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, entry := range strings.Split(raw, ",") {
		cleaned := strings.TrimSpace(entry)
		if cleaned == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(cleaned)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS entry %q is not a CIDR: %w", cleaned, err)
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
