// Package ratelimit is the token bucket in front of the expensive paths. The
// decision is made inside Redis in one atomic script, so two API replicas
// cannot each hand out the last token.
//
// Rate state is disposable: a Redis restart resets every window and nothing
// durable is lost. Jobs, checkpoints and outbox events never live here.
package ratelimit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrUnavailable means Redis could not answer. The caller decides whether that
// closes the door (provider-costing work fails closed) or opens it (cheap
// authenticated reads stay available and readiness reports degraded).
var ErrUnavailable = errors.New("rate limiter unavailable")

// Policy is a refill rate plus a burst. Requests per Window refill the bucket
// steadily; Burst is how many may arrive at once.
type Policy struct {
	Name     string
	Requests int
	Window   time.Duration
	Burst    int
}

func (p Policy) capacity() int {
	if p.Burst > 0 {
		return p.Burst
	}
	return p.Requests
}

type Decision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// Hasher makes a user id or client IP safe to use as a Redis key or a log
// field: HMAC-SHA256 under a secret salt, truncated to 64 bits. Stable enough
// to correlate, useless to reverse without the salt.
//
// The salt rotates by changing RATE_LIMIT_SALT and restarting. Rotation resets
// every window, which is acceptable: rate state is disposable by design.
type Hasher struct {
	salt []byte
}

func NewHasher(salt string) Hasher {
	return Hasher{salt: []byte(salt)}
}

// Hash is used for every subject in a Redis key and belongs in any log line
// that would otherwise carry the raw value.
func (h Hasher) Hash(subject string) string {
	mac := hmac.New(sha256.New, h.salt)
	mac.Write([]byte(subject))
	return hex.EncodeToString(mac.Sum(nil)[:8])
}

// bucketScript is a token bucket kept as two fields with one expiry. It returns
// whether the call is allowed, the tokens left, and how long until one refills.
//
// KEYS[1] bucket   ARGV[1] capacity  ARGV[2] refill per second
// ARGV[3] now (ms) ARGV[4] cost      ARGV[5] ttl (ms)
var bucketScript = redis.NewScript(`
local capacity = tonumber(ARGV[1])
local refill_per_second = tonumber(ARGV[2])
local now_ms = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])
local ttl_ms = tonumber(ARGV[5])

local state = redis.call('HMGET', KEYS[1], 'tokens', 'updated_ms')
local tokens = tonumber(state[1])
local updated_ms = tonumber(state[2])

if tokens == nil or updated_ms == nil then
  tokens = capacity
  updated_ms = now_ms
end

local elapsed_ms = math.max(0, now_ms - updated_ms)
tokens = math.min(capacity, tokens + (elapsed_ms / 1000.0) * refill_per_second)

local allowed = 0
local retry_after_ms = 0
if tokens >= cost then
  allowed = 1
  tokens = tokens - cost
else
  local missing = cost - tokens
  retry_after_ms = math.ceil((missing / refill_per_second) * 1000)
end

redis.call('HSET', KEYS[1], 'tokens', tokens, 'updated_ms', now_ms)
redis.call('PEXPIRE', KEYS[1], ttl_ms)

return {allowed, math.floor(tokens), retry_after_ms}
`)

type Limiter struct {
	client *redis.Client
	prefix string
	hasher Hasher
	now    func() time.Time
}

// New builds a limiter whose keys never carry a raw subject: every subject is
// hashed with the salted hasher before it reaches Redis.
func New(client *redis.Client, prefix string, hasher Hasher) *Limiter {
	if prefix == "" {
		prefix = "reelpin:ratelimit"
	}
	return &Limiter{client: client, prefix: prefix, hasher: hasher, now: time.Now}
}

// WithClock is for tests that need to move time without sleeping.
func (l *Limiter) WithClock(now func() time.Time) *Limiter {
	return &Limiter{client: l.client, prefix: l.prefix, hasher: l.hasher, now: now}
}

// Hash exposes the limiter's own hasher so a log site correlates with the key
// this limiter actually wrote.
func (l *Limiter) Hash(subject string) string { return l.hasher.Hash(subject) }

// Allow spends one token for subject under policy.
func (l *Limiter) Allow(ctx context.Context, policy Policy, subject string) (Decision, error) {
	if policy.Requests <= 0 || policy.Window <= 0 {
		return Decision{Allowed: true}, nil
	}

	capacity := policy.capacity()
	refillPerSecond := float64(policy.Requests) / policy.Window.Seconds()
	// The bucket is kept long enough to refill completely, and no longer.
	ttl := time.Duration(math.Ceil(float64(capacity)/refillPerSecond))*time.Second + policy.Window

	key := fmt.Sprintf("%s:%s:%s", l.prefix, policy.Name, l.hasher.Hash(subject))
	result, err := bucketScript.Run(ctx, l.client, []string{key},
		capacity,
		refillPerSecond,
		l.now().UnixMilli(),
		1,
		ttl.Milliseconds(),
	).Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if len(result) != 3 {
		return Decision{}, fmt.Errorf("%w: unexpected script result", ErrUnavailable)
	}

	allowed, _ := result[0].(int64)
	remaining, _ := result[1].(int64)
	retryAfterMS, _ := result[2].(int64)

	decision := Decision{
		Allowed:   allowed == 1,
		Remaining: int(remaining),
	}
	if !decision.Allowed {
		decision.RetryAfter = time.Duration(retryAfterMS) * time.Millisecond
		if decision.RetryAfter < time.Second {
			decision.RetryAfter = time.Second
		}
	}
	return decision, nil
}
