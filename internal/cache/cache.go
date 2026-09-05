// Package cache is a read-through cache for responses the database can always
// rebuild. Redis is never the source of truth here: every miss, error, or
// timeout falls back to PostgreSQL and the request still succeeds.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// schemaVersion is part of every key. Bump it and every cached value is
// abandoned at once, with no scan and no delete.
const schemaVersion = "v1"

type Cache struct {
	client *redis.Client
	prefix string
	group  singleflight.Group
}

func New(client *redis.Client, prefix string) *Cache {
	if prefix == "" {
		prefix = "reelpin:cache"
	}
	return &Cache{client: client, prefix: prefix}
}

// GetOrLoad returns the cached value for a user-scoped key, or loads it. A
// Redis failure is not an error: load runs and its value is returned.
func GetOrLoad[T any](
	ctx context.Context,
	c *Cache,
	userID, name, variant string,
	ttl time.Duration,
	load func(ctx context.Context) (T, error),
) (T, error) {
	var zero T
	if c == nil || c.client == nil {
		return load(ctx)
	}

	key, err := c.key(ctx, userID, name, variant)
	if err != nil {
		// Without the user's cache version the key is unsafe to reuse.
		return load(ctx)
	}

	if cached, err := c.client.Get(ctx, key).Bytes(); err == nil {
		var value T
		if err := json.Unmarshal(cached, &value); err == nil {
			return value, nil
		}
		// A value that no longer decodes is stale by definition.
		c.client.Del(ctx, key)
	} else if !errors.Is(err, redis.Nil) && ctx.Err() != nil {
		return zero, ctx.Err()
	}

	// One loader per key: a cold cache under load must not become a stampede
	// of identical queries.
	loaded, err, _ := c.group.Do(key, func() (any, error) {
		value, err := load(ctx)
		if err != nil {
			return nil, err
		}
		if encoded, err := json.Marshal(value); err == nil {
			// Jitter keeps a burst of keys written together from expiring together.
			c.client.Set(ctx, key, encoded, jitter(ttl))
		}
		return value, nil
	})
	if err != nil {
		return zero, err
	}

	value, ok := loaded.(T)
	if !ok {
		return load(ctx)
	}
	return value, nil
}

// InvalidateUser drops everything cached for one user by moving their version
// forward. No scan, no key list, and in-flight readers simply miss.
func (c *Cache) InvalidateUser(ctx context.Context, userID string) error {
	if c == nil || c.client == nil {
		return nil
	}
	if err := c.client.Incr(ctx, c.versionKey(userID)).Err(); err != nil {
		return fmt.Errorf("invalidating cached values: %w", err)
	}
	return nil
}

func (c *Cache) key(ctx context.Context, userID, name, variant string) (string, error) {
	version, err := c.client.Get(ctx, c.versionKey(userID)).Int64()
	if errors.Is(err, redis.Nil) {
		version = 0
	} else if err != nil {
		return "", err
	}

	// The user id is hashed so an operator reading Redis keys sees no account
	// identifiers.
	subject := sha256.Sum256([]byte(userID))
	variantSum := sha256.Sum256([]byte(variant))

	return c.prefix + ":" + schemaVersion + ":" +
		hex.EncodeToString(subject[:])[:16] + ":" +
		strconv.FormatInt(version, 10) + ":" +
		name + ":" + hex.EncodeToString(variantSum[:])[:16], nil
}

func (c *Cache) versionKey(userID string) string {
	subject := sha256.Sum256([]byte(userID))
	return c.prefix + ":uv:" + hex.EncodeToString(subject[:])[:16]
}

// jitter spreads expiry by up to 10 percent.
func jitter(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return ttl
	}
	spread := float64(ttl) * 0.1
	return ttl + time.Duration(rand.Float64()*spread)
}
