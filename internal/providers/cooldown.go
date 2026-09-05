// Package providers holds cross-worker provider state that is allowed to be
// lost. A cooldown in Redis stops every worker from hammering a provider that
// just pushed back; if Redis restarts and the cooldown vanishes, the provider
// pushes back again and the cooldown is re-created. Durable work never lives
// here.
package providers

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Cooldowns stores one expiring entry per provider. Keys carry provider names
// (infrastructure, not user data), never URLs, tokens or user identifiers.
type Cooldowns struct {
	client *redis.Client
	prefix string
}

func NewCooldowns(client *redis.Client, prefix string) *Cooldowns {
	if prefix == "" {
		prefix = "reelpin:cooldown"
	}
	return &Cooldowns{client: client, prefix: prefix}
}

func (c *Cooldowns) key(provider string) string { return c.prefix + ":" + provider }

// Set starts or extends a cooldown. A shorter new cooldown never truncates a
// longer running one: the longest known push-back wins.
func (c *Cooldowns) Set(ctx context.Context, provider, reason string, duration time.Duration) error {
	if c.client == nil || duration <= 0 {
		return nil
	}

	remaining, err := c.client.PTTL(ctx, c.key(provider)).Result()
	if err == nil && remaining > duration {
		return nil
	}
	if err := c.client.Set(ctx, c.key(provider), reason, duration).Err(); err != nil {
		return fmt.Errorf("setting a cooldown for %s: %w", provider, err)
	}
	return nil
}

// Remaining reports how long the provider is still cooling down. Zero means it
// is not. An unreachable Redis returns an error and the caller proceeds: a
// missing cooldown is safe, because the provider will push back again.
func (c *Cooldowns) Remaining(ctx context.Context, provider string) (time.Duration, error) {
	if c.client == nil {
		return 0, nil
	}
	remaining, err := c.client.PTTL(ctx, c.key(provider)).Result()
	if err != nil {
		return 0, fmt.Errorf("reading the cooldown for %s: %w", provider, err)
	}
	// PTTL answers a negative duration for a missing key or one with no expiry.
	if remaining <= 0 {
		return 0, nil
	}
	return remaining, nil
}
