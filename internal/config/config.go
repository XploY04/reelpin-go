package config

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const localDatabaseURL = "postgres://reelpin:reelpin@localhost:5432/reelpin"

type Config struct {
	Environment             string
	Port                    int
	DatabaseURL             string
	Version                 string
	SupabaseURL             string
	SupabaseJWTAudience     string
	RedisURL                string
	RedisKeyPrefix          string
	RabbitMQURL             string
	WorkerID                string
	WorkerQueueConcurrency  int
	WorkerGlobalConcurrency int
	OutboxBatchSize         int
	GeminiAPIKey            string
	GoogleMapsAPIKey        string
	// WorkerTempRoot is where a run's media lives while it is being processed.
	WorkerTempRoot string
	ApifyToken     string
	// ApifyActors maps a platform to its configured actor id. An unconfigured
	// platform falls back instead of failing.
	ApifyActors map[string]string
	// InstagramCookies holds base64 Netscape cookie data per slot. The
	// deprecated single-slot variables are deliberately not read.
	InstagramCookies   map[string]string
	SupabaseServiceKey string
	StorageBucket      string
	RedditClientID     string
	RedditClientSecret string
	RedditUserAgent    string
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
		RabbitMQURL:         strings.TrimSpace(os.Getenv("RABBITMQ_URL")),
		WorkerID:            envOr("WORKER_ID", defaultWorkerID()),
		GeminiAPIKey:        strings.TrimSpace(os.Getenv("GEMINI_API_KEY")),
		GoogleMapsAPIKey:    strings.TrimSpace(os.Getenv("GOOGLE_MAPS_API_KEY")),
		WorkerTempRoot:      envOr("WORKER_TEMP_ROOT", filepath.Join(os.TempDir(), "reelpin-runs")),
		ApifyToken:          strings.TrimSpace(os.Getenv("APIFY_TOKEN")),
		SupabaseServiceKey:  strings.TrimSpace(os.Getenv("SUPABASE_SERVICE_KEY")),
		StorageBucket:       envOr("SUPABASE_STORAGE_BUCKET", "reel-thumbnails"),
		RedditClientID:      strings.TrimSpace(os.Getenv("REDDIT_CLIENT_ID")),
		RedditClientSecret:  strings.TrimSpace(os.Getenv("REDDIT_CLIENT_SECRET")),
		RedditUserAgent:     envOr("REDDIT_USER_AGENT", "reelpin/1.0"),
		ApifyActors: map[string]string{
			"instagram": strings.TrimSpace(os.Getenv("APIFY_INSTAGRAM_ACTOR")),
			"youtube":   strings.TrimSpace(os.Getenv("APIFY_YOUTUBE_ACTOR")),
			"linkedin":  strings.TrimSpace(os.Getenv("APIFY_LINKEDIN_ACTOR")),
			"x":         strings.TrimSpace(os.Getenv("APIFY_X_ACTOR")),
		},
		InstagramCookies: map[string]string{
			"active":   strings.TrimSpace(os.Getenv("INSTAGRAM_COOKIES_ACTIVE_B64")),
			"backup":   strings.TrimSpace(os.Getenv("INSTAGRAM_COOKIES_BACKUP_B64")),
			"tertiary": strings.TrimSpace(os.Getenv("INSTAGRAM_COOKIES_TERTIARY_B64")),
		},
	}

	for _, setting := range []struct {
		key      string
		fallback int
		minimum  int
		maximum  int
		target   *int
	}{
		{"WORKER_QUEUE_CONCURRENCY", 4, 1, 256, &cfg.WorkerQueueConcurrency},
		{"WORKER_GLOBAL_CONCURRENCY", 16, 1, 1024, &cfg.WorkerGlobalConcurrency},
		{"OUTBOX_BATCH_SIZE", 100, 1, 1000, &cfg.OutboxBatchSize},
	} {
		value, err := intEnv(setting.key, setting.fallback, setting.minimum, setting.maximum)
		if err != nil {
			return Config{}, err
		}
		*setting.target = value
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

// defaultWorkerID identifies one process in logs and heartbeats. The hostname
// is what a container orchestrator already makes unique.
func defaultWorkerID() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "worker"
	}
	return host
}

func intEnv(key string, fallback, minimum, maximum int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a number", key, raw)
	}
	if value < minimum || value > maximum {
		return 0, fmt.Errorf("%s %d is outside %d..%d", key, value, minimum, maximum)
	}
	return value, nil
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
