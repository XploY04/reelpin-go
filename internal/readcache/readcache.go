// Package readcache keeps a user's most common reads in Redis for a minute,
// so opening the app with nothing new to show never reaches PostgreSQL.
//
// Every key carries the user's generation, a counter that any write to their
// data increments. Invalidation is that one INCR: entries under an older
// generation are never read again and expire on their own. Writers bump after
// their commit, so a read that misses always finds the committed state.
//
// Redis is optional. A nil Cache, or a Redis that fails, passes every read
// straight through to the reader underneath.
package readcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// TTL bounds how stale an entry can be when a write somewhere failed to bump
// the generation.
const TTL = 60 * time.Second

// Store is the slice of a Redis client the cache needs.
type Store interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	Incr(ctx context.Context, key string) *redis.IntCmd
}

type Cache struct {
	store  Store
	prefix string
	logger *slog.Logger
}

func New(store Store, prefix string, logger *slog.Logger) *Cache {
	if store == nil {
		return nil
	}
	if prefix == "" {
		prefix = "reelpin"
	}
	return &Cache{store: store, prefix: prefix + ":cache", logger: logger}
}

// subject keeps the raw user id out of Redis keys. It is the full digest, not
// a log-sized prefix of one, because a collision here would hand one user's
// cached library to another.
func subject(userID string) string {
	sum := sha256.Sum256([]byte(userID))
	return hex.EncodeToString(sum[:])
}

func (c *Cache) generationKey(userID string) string {
	return c.prefix + ":gen:" + subject(userID)
}

// InvalidateUser moves the user to a new generation. The counter never
// expires: if it did, it could restart at a number whose old entries are
// still alive.
func (c *Cache) InvalidateUser(ctx context.Context, userID string) error {
	if c == nil {
		return nil
	}
	if err := c.store.Incr(ctx, c.generationKey(userID)).Err(); err != nil {
		return fmt.Errorf("bumping the cache generation: %w", err)
	}
	return nil
}

func (c *Cache) generation(ctx context.Context, userID string) (string, error) {
	value, err := c.store.Get(ctx, c.generationKey(userID)).Result()
	if errors.Is(err, redis.Nil) {
		return "0", nil
	}
	if err != nil {
		return "", err
	}
	if _, err := strconv.ParseInt(value, 10, 64); err != nil {
		return "", fmt.Errorf("the cache generation is not a number")
	}
	return value, nil
}

// read answers from the cache, or from fetch and then fills the cache. An
// error from fetch is returned and never cached. params must identify the
// read completely: two reads with equal params share an entry.
func read[T any](ctx context.Context, c *Cache, userID, name string, params any, fetch func() (T, error)) (T, error) {
	if c == nil {
		return fetch()
	}

	generation, err := c.generation(ctx, userID)
	if err != nil {
		c.logger.Warn("read cache unavailable, reading through", "error", err)
		return fetch()
	}
	encodedParams, err := json.Marshal(params)
	if err != nil {
		// Params are plain structs; a read whose key cannot be built is
		// still a read, just an uncached one.
		return fetch()
	}
	paramsSum := sha256.Sum256(encodedParams)
	key := fmt.Sprintf("%s:%s:%s:%s:%s", c.prefix, subject(userID), generation, name,
		hex.EncodeToString(paramsSum[:]))

	cached, err := c.store.Get(ctx, key).Bytes()
	if err == nil {
		var value T
		if err := json.Unmarshal(cached, &value); err == nil {
			return value, nil
		}
		// An entry written by an older shape of T is a miss, not a failure.
	} else if !errors.Is(err, redis.Nil) {
		c.logger.Warn("read cache unavailable, reading through", "error", err)
		return fetch()
	}

	value, err := fetch()
	if err != nil {
		return value, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		c.logger.Warn("a read could not be cached", "read", name, "error", err)
		return value, nil
	}
	if err := c.store.Set(ctx, key, encoded, TTL).Err(); err != nil {
		c.logger.Warn("filling the read cache failed", "error", err)
	}
	return value, nil
}
