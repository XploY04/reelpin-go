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
	Environment string
	Port        int
	// MetricsPort is where the worker exposes its Prometheus endpoint. The API
	// serves its own on the API port instead.
	MetricsPort         int
	DatabaseURL         string
	Version             string
	SupabaseURL         string
	SupabaseJWTAudience string

	// AdminKey guards the Prometheus endpoints. Empty means neither process
	// exposes one: queue depths and failure rates are operational detail, and
	// an unauthenticated scrape target is worse than none.
	AdminKey string

	// IPBucketSecret is shared with the Next.js boundary, which is the only hop
	// that sees a browser's real address. Empty means no forwarded bucket is
	// believed and per-IP limits fall back to the socket peer.
	IPBucketSecret string

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

	// Firebase sends push notifications. Absent outside production means a run
	// still completes and its notification records that there was nowhere to
	// send, rather than failing the job.
	FirebaseCredentialsJSON string
	FirebaseProjectID       string

	// EmbeddingModel and EmbeddingDimension are validated here rather than
	// spread as string literals: the index holds exactly one dimension, and a
	// mismatch is a corrupt vector set rather than a slow query.
	EmbeddingModel     string
	EmbeddingDimension int
	// The cost gate. These four are the only product values in this file, and
	// they are read as raw strings because parsing them belongs to
	// internal/spend, which config may not import. All four together enable the
	// gate; none of them leaves it off. Anything in between fails at startup,
	// because a limit assembled from half a decision is a limit nobody made.
	CostGateWarnUSD   string
	CostGateStopUSD   string
	CostGateStopOrder string
	CostGatePrices    string

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
		Environment:    envOr("ENVIRONMENT", "development"),
		Port:           8000,
		MetricsPort:    9100,
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		Version:        envOr("APP_VERSION", "dev"),
		AdminKey:       strings.TrimSpace(os.Getenv("ADMIN_KEY")),
		IPBucketSecret: strings.TrimSpace(os.Getenv("REELPIN_IP_BUCKET_SECRET")),

		SupabaseURL: strings.TrimSpace(os.Getenv("SUPABASE_URL")),
		RabbitMQURL: strings.TrimSpace(os.Getenv("RABBITMQ_URL")),
		WorkerID:    envOr("WORKER_ID", defaultWorkerID()),

		FirebaseCredentialsJSON: strings.TrimSpace(os.Getenv("FIREBASE_CREDENTIALS_JSON")),
		FirebaseProjectID:       strings.TrimSpace(os.Getenv("FIREBASE_PROJECT_ID")),

		// The default is repeated here rather than imported: config depends on
		// nothing internal, and embed.AssertConfigured catches any drift at
		// startup.
		EmbeddingModel:      envOr("EMBEDDING_MODEL", "gemini-embedding-2"),
		EmbeddingDimension:  768,
		SupabaseJWTAudience: envOr("SUPABASE_JWT_AUDIENCE", "authenticated"),

		RedisURL:       strings.TrimSpace(os.Getenv("REDIS_URL")),
		RedisKeyPrefix: envOr("REDIS_KEY_PREFIX", "reelpin"),
		RateLimitSalt:  strings.TrimSpace(os.Getenv("RATE_LIMIT_SALT")),

		GeminiAPIKey: strings.TrimSpace(os.Getenv("GEMINI_API_KEY")),

		CostGateWarnUSD:   strings.TrimSpace(os.Getenv("COST_GATE_WARN_USD")),
		CostGateStopUSD:   strings.TrimSpace(os.Getenv("COST_GATE_STOP_USD")),
		CostGateStopOrder: strings.TrimSpace(os.Getenv("COST_GATE_STOP_ORDER")),
		CostGatePrices:    strings.TrimSpace(os.Getenv("COST_GATE_PRICES")),
	}

	if err := cfg.checkCostGate(); err != nil {
		return Config{}, err
	}

	if !validEnvironments[cfg.Environment] {
		return Config{}, fmt.Errorf("ENVIRONMENT %q is not one of development, test, production", cfg.Environment)
	}

	port, err := portFrom("PORT", cfg.Port)
	if err != nil {
		return Config{}, err
	}
	cfg.Port = port

	metricsPort, err := portFrom("METRICS_PORT", cfg.MetricsPort)
	if err != nil {
		return Config{}, err
	}
	cfg.MetricsPort = metricsPort

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

// CostGateConfigured reports whether all four cost-gate variables are set.
func (c Config) CostGateConfigured() bool {
	return c.CostGateWarnUSD != "" && c.CostGateStopUSD != "" &&
		c.CostGateStopOrder != "" && c.CostGatePrices != ""
}

// checkCostGate refuses a partial gate. Set none of them and spending is simply
// not limited, which is visible in the startup log and on the zeroed gauges;
// set some of them and the service would enforce a limit assembled out of
// defaults nobody approved.
func (c Config) checkCostGate() error {
	set := map[string]bool{
		"COST_GATE_WARN_USD":   c.CostGateWarnUSD != "",
		"COST_GATE_STOP_USD":   c.CostGateStopUSD != "",
		"COST_GATE_STOP_ORDER": c.CostGateStopOrder != "",
		"COST_GATE_PRICES":     c.CostGatePrices != "",
	}
	missing := []string{}
	for _, name := range []string{
		"COST_GATE_WARN_USD", "COST_GATE_STOP_USD",
		"COST_GATE_STOP_ORDER", "COST_GATE_PRICES",
	} {
		if !set[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 || len(missing) == len(set) {
		return nil
	}
	return fmt.Errorf("the cost gate needs all four of its variables or none: %s %s not set",
		strings.Join(missing, ", "), plural(len(missing)))
}

func plural(count int) string {
	if count == 1 {
		return "is"
	}
	return "are"
}

func portFrom(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a number", key, raw)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s %d is outside 1..65535", key, port)
	}
	return port, nil
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
