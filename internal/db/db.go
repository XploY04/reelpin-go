package db

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The pool is sized for a serverless database that suspends after five idle
// minutes. No connection is kept open for its own sake, an idle one closes
// before the suspend window, and the first query after a suspend may wait for
// a cold start, which the connect timeout leaves room for.
const (
	maxConns          = 4
	maxConnIdleTime   = 4 * time.Minute
	maxConnLifetime   = time.Hour
	healthCheckPeriod = time.Minute
	connectTimeout    = "10"
	startupPing       = 15 * time.Second
)

func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	config, err := Config(databaseURL)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("creating pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, startupPing)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging db: %w", err)
	}
	return pool, nil
}

// Config parses the URL and applies the idle-friendly pool settings.
func Config(databaseURL string) (*pgxpool.Config, error) {
	config, err := pgxpool.ParseConfig(withConnectTimeout(databaseURL))
	if err != nil {
		return nil, fmt.Errorf("creating pool: %w", err)
	}
	config.MinConns = 0
	config.MinIdleConns = 0
	config.MaxConns = maxConns
	config.MaxConnIdleTime = maxConnIdleTime
	config.MaxConnLifetime = maxConnLifetime
	config.HealthCheckPeriod = healthCheckPeriod
	return config, nil
}

// withConnectTimeout adds connect_timeout to a URL or keyword/value DSN that
// does not already set one.
func withConnectTimeout(dsn string) string {
	if strings.Contains(dsn, "connect_timeout") {
		return dsn
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			// Left alone, ParseConfig reports the malformed URL.
			return dsn
		}
		query := parsed.Query()
		query.Set("connect_timeout", connectTimeout)
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	if strings.TrimSpace(dsn) == "" {
		return dsn
	}
	return dsn + " connect_timeout=" + connectTimeout
}
